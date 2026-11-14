package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blang/semver"
	"github.com/lib/pq"
)

// countingConnector is a fake driver.Connector that tracks how many physical
// connections are open at once.
type countingConnector struct {
	open    atomic.Int32
	maxOpen atomic.Int32
	dials   atomic.Int32
	// dialDelay makes each dial take a while, so concurrent dials overlap.
	dialDelay time.Duration
	// calls records which driver methods were used, by name.
	calls sync.Map
}

func (c *countingConnector) Connect(context.Context) (driver.Conn, error) {
	time.Sleep(c.dialDelay)
	c.dials.Add(1)
	n := c.open.Add(1)
	for {
		m := c.maxOpen.Load()
		if n <= m || c.maxOpen.CompareAndSwap(m, n) {
			break
		}
	}
	return &countingConn{c: c}, nil
}

func (c *countingConnector) Driver() driver.Driver { return nil }

func (c *countingConnector) count(name string) int {
	v, ok := c.calls.Load(name)
	if !ok {
		return 0
	}
	return int(v.(*atomic.Int32).Load())
}

func (c *countingConnector) record(name string) {
	v, _ := c.calls.LoadOrStore(name, new(atomic.Int32))
	v.(*atomic.Int32).Add(1)
}

// countingConn implements every optional interface lib/pq's conn implements.
type countingConn struct {
	c      *countingConnector
	closed atomic.Bool
}

var (
	_ driver.QueryerContext     = (*countingConn)(nil)
	_ driver.ExecerContext      = (*countingConn)(nil)
	_ driver.ConnBeginTx        = (*countingConn)(nil)
	_ driver.ConnPrepareContext = (*countingConn)(nil)
	_ driver.SessionResetter    = (*countingConn)(nil)
	_ driver.Validator          = (*countingConn)(nil)
	_ driver.NamedValueChecker  = (*countingConn)(nil)
	_ driver.Pinger             = (*countingConn)(nil)
)

func (cn *countingConn) Prepare(string) (driver.Stmt, error) {
	cn.c.record("Prepare")
	return nil, errors.New("unexpected Prepare")
}

func (cn *countingConn) Close() error {
	if cn.closed.CompareAndSwap(false, true) {
		cn.c.open.Add(-1)
	}
	return nil
}

func (cn *countingConn) Begin() (driver.Tx, error) {
	cn.c.record("Begin")
	return fakeTx{}, nil
}

func (cn *countingConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	cn.c.record("QueryContext")
	return &fakeRows{}, nil
}

func (cn *countingConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	cn.c.record("ExecContext")
	return driver.RowsAffected(1), nil
}

func (cn *countingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	cn.c.record("BeginTx")
	return fakeTx{}, nil
}

func (cn *countingConn) PrepareContext(context.Context, string) (driver.Stmt, error) {
	cn.c.record("PrepareContext")
	return nil, errors.New("unexpected PrepareContext")
}

func (cn *countingConn) ResetSession(context.Context) error {
	cn.c.record("ResetSession")
	return nil
}

func (cn *countingConn) IsValid() bool {
	cn.c.record("IsValid")
	return true
}

func (cn *countingConn) CheckNamedValue(*driver.NamedValue) error {
	cn.c.record("CheckNamedValue")
	return nil
}

func (cn *countingConn) Ping(context.Context) error {
	cn.c.record("Ping")
	return nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"x"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

// newBudgetTestPool opens a pool on fake connections that draws from budget b.
func newBudgetTestPool(t *testing.T, inner driver.Connector, b *endpointBudget, maxIdle int, wait time.Duration) *sql.DB {
	t.Helper()
	db := openBudgetedDB(inner, b, maxIdle, wait)
	db.SetMaxIdleConns(maxIdle)
	if maxIdle > 0 {
		db.SetConnMaxIdleTime(time.Minute)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEndpointBudgetCapsConcurrentConnections(t *testing.T) {
	const budget = 3
	b := newEndpointBudget("test-caps", budget)
	inner := &countingConnector{dialDelay: time.Millisecond}
	pools := []*sql.DB{
		newBudgetTestPool(t, inner, b, 2, 5*time.Second),
		newBudgetTestPool(t, inner, b, 2, 5*time.Second),
		newBudgetTestPool(t, inner, b, 0, 5*time.Second),
	}

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(db *sql.DB) {
			defer wg.Done()
			conn, err := db.Conn(context.Background())
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(2 * time.Millisecond)
			_ = conn.Close()
		}(pools[i%len(pools)])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := inner.maxOpen.Load(); got > budget {
		t.Errorf("peak open connections = %d, want <= %d", got, budget)
	}
}

func TestEndpointBudgetReapsIdleConnectionOfSiblingPool(t *testing.T) {
	b := newEndpointBudget("test-reap", 1)
	inner := &countingConnector{}
	idlePool := newBudgetTestPool(t, inner, b, 1, 5*time.Second)
	other := newBudgetTestPool(t, inner, b, 0, 100*time.Millisecond)

	if err := idlePool.Ping(); err != nil {
		t.Fatalf("Ping() on first pool: %v", err)
	}
	if got := idlePool.Stats().Idle; got != 1 {
		t.Fatalf("first pool idle = %d, want 1", got)
	}

	// The only slot is held by the idle connection of the first pool.
	if err := other.Ping(); err != nil {
		t.Fatalf("Ping() on second pool: %v", err)
	}
	if got := idlePool.Stats().Idle; got != 0 {
		t.Errorf("first pool idle after reaping = %d, want 0", got)
	}

	// The reaped pool keeps its max_idle_connections setting.
	if err := idlePool.Ping(); err != nil {
		t.Fatalf("Ping() on first pool after reaping: %v", err)
	}
	if got := idlePool.Stats().Idle; got != 1 {
		t.Errorf("first pool idle after reuse = %d, want 1 (idle limit not restored)", got)
	}
}

func TestEndpointBudgetHandsBusyConnectionToWaitingPool(t *testing.T) {
	b := newEndpointBudget("test-handoff", 1)
	inner := &countingConnector{}
	busy := newBudgetTestPool(t, inner, b, 1, 5*time.Second)
	busy.SetMaxOpenConns(1)
	waiting := newBudgetTestPool(t, inner, b, 1, 5*time.Second)

	// Keep the busy pool's only connection in use: two workers take turns, so
	// one of them is always queued inside database/sql and a returned
	// connection goes straight to it, never sitting idle where the reaper
	// could close it.
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				conn, err := busy.Conn(context.Background())
				if err != nil {
					t.Errorf("busy pool Conn(): %v", err)
					return
				}
				time.Sleep(5 * time.Millisecond)
				_ = conn.Close()
			}
		}()
	}
	defer func() {
		close(stop)
		workers.Wait()
	}()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	if err := waiting.Ping(); err != nil {
		t.Fatalf("Ping() on waiting pool: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waiting pool got a slot after %s; a returned connection should be closed for it", elapsed)
	}
	if got := inner.maxOpen.Load(); got > 1 {
		t.Errorf("peak open connections = %d, want 1", got)
	}
}

func TestEndpointBudgetWaitTimesOutWithClearError(t *testing.T) {
	b := newEndpointBudget("test-timeout", 1)
	inner := &countingConnector{}
	holder := newBudgetTestPool(t, inner, b, 0, 5*time.Second)
	other := newBudgetTestPool(t, inner, b, 0, 100*time.Millisecond)

	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn(): %v", err)
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()
	err = other.Ping()
	if err == nil {
		t.Fatal("Ping() succeeded although the budget was exhausted")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Ping() took %s, want about the 100ms wait", elapsed)
	}
	for _, want := range []string{"max_total_connections", "parallelism"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestEndpointBudgetReleasesSlotOnFailedDial(t *testing.T) {
	b := newEndpointBudget("test-failed-dial", 1)
	db := newBudgetTestPool(t, failingConnector{}, b, 0, 100*time.Millisecond)
	for i := 0; i < 3; i++ {
		if err := db.Ping(); err == nil || strings.Contains(err.Error(), "max_total_connections") {
			t.Fatalf("Ping() #%d = %v, want the dial error", i, err)
		}
	}
}

type failingConnector struct{}

func (failingConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("dial failed")
}
func (failingConnector) Driver() driver.Driver { return nil }

// TestBudgetConnForwardsDriverInterfaces checks that wrapping a connection
// keeps the fast paths database/sql uses when the driver supports them: no
// statement is prepared for Query/Exec with arguments, transactions go
// through BeginTx, and sessions are reset and validated by the driver.
func TestBudgetConnForwardsDriverInterfaces(t *testing.T) {
	b := newEndpointBudget("test-forward", 2)
	inner := &countingConnector{}
	db := newBudgetTestPool(t, inner, b, 1, time.Second)
	ctx := context.Background()

	var x int
	if err := db.QueryRowContext(ctx, "SELECT $1", 1).Scan(&x); err != nil {
		t.Fatalf("QueryRow(): %v", err)
	}
	if _, err := db.ExecContext(ctx, "SELECT $1", 1); err != nil {
		t.Fatalf("Exec(): %v", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(): %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT $1", 1); err != nil {
		t.Fatalf("tx.Exec(): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("Ping(): %v", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn(): %v", err)
	}
	if err := conn.Raw(func(dc any) error {
		for name, ok := range map[string]bool{
			"QueryerContext":     implements[driver.QueryerContext](dc),
			"ExecerContext":      implements[driver.ExecerContext](dc),
			"ConnBeginTx":        implements[driver.ConnBeginTx](dc),
			"ConnPrepareContext": implements[driver.ConnPrepareContext](dc),
			"SessionResetter":    implements[driver.SessionResetter](dc),
			"Validator":          implements[driver.Validator](dc),
			"NamedValueChecker":  implements[driver.NamedValueChecker](dc),
			"Pinger":             implements[driver.Pinger](dc),
		} {
			if !ok {
				t.Errorf("wrapped connection does not implement driver.%s", name)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Raw(): %v", err)
	}
	_ = conn.Close()

	if n := inner.count("Prepare") + inner.count("PrepareContext"); n != 0 {
		t.Errorf("statements prepared = %d, want 0 (QueryerContext/ExecerContext not forwarded)", n)
	}
	for _, name := range []string{"QueryContext", "ExecContext", "BeginTx", "Ping", "ResetSession", "IsValid", "CheckNamedValue"} {
		if inner.count(name) == 0 {
			t.Errorf("driver method %s was never called through the wrapper", name)
		}
	}
	if n := inner.count("Begin"); n != 0 {
		t.Errorf("Begin called %d times, want BeginTx", n)
	}
	if got := inner.dials.Load(); got != 1 {
		t.Errorf("dials = %d, want 1 (connection should be reused)", got)
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

func TestEndpointBudgetKey(t *testing.T) {
	base := Config{Scheme: "postgres", Host: "db.example.com", Port: 5432, Username: "admin", Password: "secret", ConnectTimeoutSec: 10}

	sameEndpoint := base
	sameEndpoint.ConnectTimeoutSec = 30
	sameEndpoint.MaxIdleConns = 5
	if base.endpointKey() != sameEndpoint.endpointKey() {
		t.Errorf("endpoint key depends on pool settings: %q vs %q", base.endpointKey(), sameEndpoint.endpointKey())
	}

	for name, mod := range map[string]func(*Config){
		"host":   func(c *Config) { c.Host = "other.example.com" },
		"port":   func(c *Config) { c.Port = 5433 },
		"user":   func(c *Config) { c.Username = "other" },
		"scheme": func(c *Config) { c.Scheme = "awspostgres" },
	} {
		c := base
		mod(&c)
		if c.endpointKey() == base.endpointKey() {
			t.Errorf("endpoint key ignores %s", name)
		}
	}
	if strings.Contains(base.endpointKey(), "secret") {
		t.Error("endpoint key contains the password")
	}
}

func TestGetEndpointBudgetSharedPerEndpoint(t *testing.T) {
	a := getEndpointBudget("test-shared", 5)
	b := getEndpointBudget("test-shared", 7)
	if a != b {
		t.Fatal("two configurations of the same endpoint got different budgets")
	}
	if a.size != 5 {
		t.Errorf("budget size = %d, want the first configuration's 5", a.size)
	}
	if getEndpointBudget("test-shared-other", 5) == a {
		t.Error("different endpoints share a budget")
	}
}

func TestConfigEffectiveMaxConns(t *testing.T) {
	for _, tc := range []struct {
		maxConns, maxTotal, want int
	}{
		{maxConns: 20, maxTotal: 0, want: 20},
		{maxConns: 20, maxTotal: 10, want: 10},
		{maxConns: 5, maxTotal: 10, want: 5},
		{maxConns: 0, maxTotal: 10, want: 10},
		{maxConns: 0, maxTotal: 0, want: 0},
	} {
		c := Config{Scheme: "postgres", MaxConns: tc.maxConns, MaxTotalConns: tc.maxTotal}
		if got := c.effectiveMaxConns(); got != tc.want {
			t.Errorf("max_connections=%d max_total_connections=%d: effective = %d, want %d", tc.maxConns, tc.maxTotal, got, tc.want)
		}
	}
}

// TestAccEndpointBudgetWrapsPQConnection checks the wrapper against a real
// lib/pq connection: every optional interface the wrapper forwards is
// implemented by lib/pq, so none of them falls back.
func TestAccEndpointBudgetWrapsPQConnection(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	config.ConnectTimeoutSec = 10
	config.ApplicationName = "Terraform provider"
	config.ExpectedVersion = semver.MustParse("12.0.0")

	connector, err := pq.NewConnector(config.connStr("postgres"))
	if err != nil {
		t.Fatalf("pq.NewConnector(): %v", err)
	}
	connector.Dialer(proxyDriver{})
	db := newBudgetTestPool(t, connector, newEndpointBudget("test-pq", 1), 1, 5*time.Second)

	var got int
	if err := db.QueryRow("SELECT $1::int", 42).Scan(&got); err != nil || got != 42 {
		t.Fatalf("QueryRow() = %d, %v; want 42", got, err)
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn(): %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Raw(func(dc any) error {
		bc, ok := dc.(*budgetConn)
		if !ok {
			return fmt.Errorf("driver connection is %T, want *budgetConn", dc)
		}
		for name, ok := range map[string]bool{
			"QueryerContext":     implements[driver.QueryerContext](bc.Conn),
			"ExecerContext":      implements[driver.ExecerContext](bc.Conn),
			"ConnBeginTx":        implements[driver.ConnBeginTx](bc.Conn),
			"ConnPrepareContext": implements[driver.ConnPrepareContext](bc.Conn),
			"SessionResetter":    implements[driver.SessionResetter](bc.Conn),
			"Validator":          implements[driver.Validator](bc.Conn),
			"NamedValueChecker":  implements[driver.NamedValueChecker](bc.Conn),
			"Pinger":             implements[driver.Pinger](bc.Conn),
		} {
			if !ok {
				t.Errorf("lib/pq connection does not implement driver.%s", name)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Raw(): %v", err)
	}
}
