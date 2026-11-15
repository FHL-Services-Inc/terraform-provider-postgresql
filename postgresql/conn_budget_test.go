package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
)

// countingConnector is a fake driver.Connector that tracks how many physical
// connections are open at once and which driver methods were called.
type countingConnector struct {
	open    atomic.Int32
	maxOpen atomic.Int32
	dials   atomic.Int32
	calls   sync.Map
}

func (c *countingConnector) Connect(context.Context) (driver.Conn, error) {
	time.Sleep(time.Millisecond) // let concurrent dials overlap
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

func (c *countingConnector) record(name string) {
	v, _ := c.calls.LoadOrStore(name, new(atomic.Int32))
	v.(*atomic.Int32).Add(1)
}

func (c *countingConnector) count(name string) int {
	if v, ok := c.calls.Load(name); ok {
		return int(v.(*atomic.Int32).Load())
	}
	return 0
}

// countingConn implements the same optional driver interfaces as lib/pq's
// connection.
type countingConn struct {
	c      *countingConnector
	closed atomic.Bool
}

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

// newBudgetTestPool opens a pool on inner that draws from budget b, with the
// pool settings configurePool would apply.
func newBudgetTestPool(t *testing.T, inner driver.Connector, b *connBudget, maxIdle int, wait time.Duration) *sql.DB {
	t.Helper()
	db := openBudgetedDB(inner, b, maxIdle, wait)
	db.SetMaxIdleConns(maxIdle)
	db.SetMaxOpenConns(defaultProviderMaxOpenConnections)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestConnBudgetCapsConcurrentConnections(t *testing.T) {
	const budget = 3
	b := newConnBudget("test", budget)
	inner := &countingConnector{}
	pools := []*sql.DB{
		newBudgetTestPool(t, inner, b, 2, 5*time.Second),
		newBudgetTestPool(t, inner, b, 2, 5*time.Second),
		newBudgetTestPool(t, inner, b, 0, 5*time.Second),
	}

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for i := range 60 {
		wg.Go(func() {
			conn, err := pools[i%len(pools)].Conn(context.Background())
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(2 * time.Millisecond)
			_ = conn.Close()
		})
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

func TestConnBudgetClosesIdleConnectionOfAnotherPool(t *testing.T) {
	b := newConnBudget("test", 1)
	inner := &countingConnector{}
	idlePool := newBudgetTestPool(t, inner, b, 1, 5*time.Second)
	other := newBudgetTestPool(t, inner, b, 0, 100*time.Millisecond)

	if err := idlePool.Ping(); err != nil {
		t.Fatalf("Ping() on first pool: %v", err)
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

// TestConnBudgetBusyPoolGivesUpConnection checks that a pool whose queries
// keep its connections busy cannot starve another pool: a connection returned
// while another pool waits for a slot is closed instead of being reused.
func TestConnBudgetBusyPoolGivesUpConnection(t *testing.T) {
	b := newConnBudget("test", 1)
	inner := &countingConnector{}
	busy := newBudgetTestPool(t, inner, b, 1, 5*time.Second)
	waiting := newBudgetTestPool(t, inner, b, 1, 5*time.Second)

	// Two workers take turns on the busy pool's only connection, so one of
	// them is always queued in database/sql and a returned connection goes
	// straight to it, never sitting idle.
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
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
		})
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
		t.Errorf("waiting pool got a slot after %s", elapsed)
	}
	if got := inner.maxOpen.Load(); got > 1 {
		t.Errorf("peak open connections = %d, want 1", got)
	}
}

func TestConnBudgetWaitTimesOutWithClearError(t *testing.T) {
	b := newConnBudget("postgres://admin@db:5432", 1)
	holder := newBudgetTestPool(t, &countingConnector{}, b, 0, 5*time.Second)
	other := newBudgetTestPool(t, &countingConnector{}, b, 0, 100*time.Millisecond)

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
	for _, want := range []string{"max_total_connections = 1", "-parallelism", "postgres://admin@db:5432"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestConnBudgetSecondConnectionTimesOut checks that a caller that holds every
// slot and asks for another connection, of the same pool or another one, gets
// the budget error after the wait instead of hanging. With the pool's open
// limit capped at the budget, database/sql would queue the second request
// without a deadline.
func TestConnBudgetSecondConnectionTimesOut(t *testing.T) {
	b := newConnBudget("test", 1)
	db := newBudgetTestPool(t, &countingConnector{}, b, 1, 100*time.Millisecond)
	other := newBudgetTestPool(t, &countingConnector{}, b, 1, 100*time.Millisecond)

	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn(): %v", err)
	}
	defer func() { _ = held.Close() }()

	for name, pool := range map[string]*sql.DB{"same pool": db, "other pool": other} {
		done := make(chan error, 1)
		go func() { done <- pool.Ping() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "max_total_connections") {
				t.Errorf("%s: Ping() = %v, want the budget error", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Ping() hangs while the caller holds the only slot", name)
		}
	}
}

type failingConnector struct{}

func (failingConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("dial failed")
}
func (failingConnector) Driver() driver.Driver { return nil }

func TestConnBudgetReleasesSlotOnFailedDial(t *testing.T) {
	db := newBudgetTestPool(t, failingConnector{}, newConnBudget("test", 1), 0, 100*time.Millisecond)
	for i := range 3 {
		if err := db.Ping(); err == nil || !strings.Contains(err.Error(), "dial failed") {
			t.Fatalf("Ping() #%d = %v, want the dial error", i, err)
		}
	}
}

// TestBudgetConnForwardsDriverInterfaces checks that the wrapped connection
// keeps database/sql's fast paths: no statement is prepared for Query/Exec
// with arguments, transactions use BeginTx, and the driver resets and
// validates sessions.
func TestBudgetConnForwardsDriverInterfaces(t *testing.T) {
	inner := &countingConnector{}
	db := newBudgetTestPool(t, inner, newConnBudget("test", 2), 1, time.Second)
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

	if n := inner.count("Prepare") + inner.count("PrepareContext"); n != 0 {
		t.Errorf("statements prepared = %d, want 0", n)
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

func TestConfigConnBudget(t *testing.T) {
	base := Config{Scheme: "postgres", Host: "db.example.com", Port: 5432, Username: "admin", Password: "secret", MaxTotalConns: 4}
	if (&Config{Scheme: "postgres"}).connBudget() != nil {
		t.Error("connBudget() is not nil with max_total_connections = 0")
	}
	aws := base
	aws.Scheme = "awspostgres"
	if aws.connBudget() != nil {
		t.Error("connBudget() is not nil for a gocloud scheme")
	}

	b := base.connBudget()
	same := base
	same.MaxIdleConns = 5
	if same.connBudget() != b {
		t.Error("two clients of the same configuration got different budgets")
	}
	for name, mod := range map[string]func(*Config){
		"host": func(c *Config) { c.Host = "other.example.com" },
		"port": func(c *Config) { c.Port = 5433 },
		"user": func(c *Config) { c.Username = "other" },
		"size": func(c *Config) { c.MaxTotalConns = 5 },
	} {
		c := base
		mod(&c)
		if c.connBudget() == b {
			t.Errorf("budget shared across different %s", name)
		}
	}
	if strings.Contains(b.name, "secret") {
		t.Errorf("budget name %q contains the password", b.name)
	}
}

func TestProviderMaxTotalConnectionsEnvDefault(t *testing.T) {
	s := Provider().Schema["max_total_connections"]
	t.Setenv("PGMAXTOTALCONNECTIONS", "")
	if v, err := s.DefaultValue(); err != nil || v != 0 {
		t.Errorf("without PGMAXTOTALCONNECTIONS: DefaultValue() = %v, %v; want 0", v, err)
	}
	t.Setenv("PGMAXTOTALCONNECTIONS", "12")
	if v, err := s.DefaultValue(); err != nil || v != "12" {
		t.Errorf("with PGMAXTOTALCONNECTIONS=12: DefaultValue() = %#v, %v; want \"12\"", v, err)
	}
}

// TestAccConnBudgetWrapsPQConnection checks the wrapper against a real lib/pq
// connection, which must implement every interface the wrapper forwards.
func TestAccConnBudgetWrapsPQConnection(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	config.ConnectTimeoutSec = 10
	connector, err := pq.NewConnector(config.connStr("postgres"))
	if err != nil {
		t.Fatalf("pq.NewConnector(): %v", err)
	}
	connector.Dialer(proxyDriver{})
	db := newBudgetTestPool(t, connector, newConnBudget("test", 1), 1, 5*time.Second)

	var got int
	if err := db.QueryRow("SELECT $1::int", 42).Scan(&got); err != nil || got != 42 {
		t.Fatalf("QueryRow() = %d, %v; want 42", got, err)
	}
}
