package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// budgetReapInterval is how often a dial waiting for a slot of an exhausted
// endpoint budget looks for idle connections to close.
const budgetReapInterval = 50 * time.Millisecond

// endpointBudget caps the number of physical connections the provider keeps
// open to one server endpoint (scheme, host, port and user), across all the
// per-database pools that connect there.
//
// A slot is taken before a connection is dialed and given back when it is
// closed. When the budget is exhausted, idle connections of the endpoint's
// pools are closed to make room, and connections returned by a pool while
// another pool waits are closed instead of being kept, so a slot moves to the
// database that needs it. Waiting for a slot is bounded: the caller gets an
// error after connect_timeout instead of hanging.
type endpointBudget struct {
	key  string
	size int
	sem  *semaphore.Weighted

	// waiting counts dials currently blocked on the semaphore.
	waiting atomic.Int32

	mu    sync.Mutex
	pools map[*budgetConnector]struct{}

	// reapMu serializes reaping, which temporarily lowers a pool's idle limit.
	reapMu sync.Mutex
}

var (
	endpointBudgetsLock sync.Mutex
	endpointBudgets     = map[string]*endpointBudget{}
)

func newEndpointBudget(key string, size int) *endpointBudget {
	return &endpointBudget{
		key:   key,
		size:  size,
		sem:   semaphore.NewWeighted(int64(size)),
		pools: map[*budgetConnector]struct{}{},
	}
}

// getEndpointBudget returns the process-wide budget for an endpoint, creating
// it with size on first use. Provider configurations (aliases) that connect to
// the same endpoint as the same user share one budget; the first one sets its
// size.
func getEndpointBudget(key string, size int) *endpointBudget {
	endpointBudgetsLock.Lock()
	defer endpointBudgetsLock.Unlock()

	if b, ok := endpointBudgets[key]; ok {
		if b.size != size {
			log.Printf("[WARN] max_total_connections = %d ignored for %s: a provider configuration for the same endpoint already set it to %d", size, key, b.size)
		}
		return b
	}
	b := newEndpointBudget(key, size)
	endpointBudgets[key] = b
	return b
}

// endpointKey identifies the server endpoint and user a configuration
// connects as. It never contains the password.
func (c *Config) endpointKey() string {
	return c.Scheme + "://" + c.Username + "@" + c.Host + ":" + strconv.Itoa(c.Port)
}

// effectiveMaxConns is the per-pool connection limit: max_connections, but
// never more than the endpoint budget when one is set.
func (c *Config) effectiveMaxConns() int {
	if c.MaxTotalConns > 0 && (c.MaxConns <= 0 || c.MaxConns > c.MaxTotalConns) {
		return c.MaxTotalConns
	}
	return c.MaxConns
}

// openBudgetedDB opens a pool whose physical connections, dialed by inner,
// count against budget b. maxIdle must be the pool's idle limit, which is
// restored after the pool's idle connections are reaped; wait bounds how long
// a dial waits for a free slot.
func openBudgetedDB(inner driver.Connector, b *endpointBudget, maxIdle int, wait time.Duration) *sql.DB {
	c := &budgetConnector{inner: inner, budget: b, maxIdle: maxIdle, wait: wait}
	db := sql.OpenDB(c)
	c.db = db
	b.mu.Lock()
	b.pools[c] = struct{}{}
	b.mu.Unlock()
	return db
}

// budgetConnector is the driver.Connector of one pool. It takes a slot of the
// endpoint budget before each physical dial.
type budgetConnector struct {
	inner   driver.Connector
	budget  *endpointBudget
	maxIdle int
	wait    time.Duration
	db      *sql.DB

	// waiting counts dials of this pool blocked on the budget.
	waiting atomic.Int32
}

func (c *budgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := c.budget.acquire(ctx, c); err != nil {
		return nil, err
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		c.budget.sem.Release(1)
		return nil, err
	}
	return &budgetConn{Conn: conn, pool: c}, nil
}

func (c *budgetConnector) Driver() driver.Driver { return c.inner.Driver() }

// Close is called by sql.DB.Close and removes the pool from the budget.
// Connections still in use give their slots back when they are closed.
func (c *budgetConnector) Close() error {
	c.budget.mu.Lock()
	delete(c.budget.pools, c)
	c.budget.mu.Unlock()
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// acquire takes a slot for a dial of pool, closing idle connections of the
// endpoint's pools if none is free, and waits at most pool.wait for one.
func (b *endpointBudget) acquire(ctx context.Context, pool *budgetConnector) error {
	if b.sem.TryAcquire(1) {
		return nil
	}

	b.waiting.Add(1)
	pool.waiting.Add(1)
	defer func() {
		pool.waiting.Add(-1)
		b.waiting.Add(-1)
	}()

	wait := pool.wait

	parent := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	for {
		// Registered as waiting first, so a connection returned from now on is
		// closed (see budgetConn.IsValid); idle ones that are already parked
		// are closed here.
		if b.reapIdle() && b.sem.TryAcquire(1) {
			return nil
		}
		pollCtx, cancel := context.WithTimeout(ctx, budgetReapInterval)
		err := b.sem.Acquire(pollCtx, 1)
		cancel()
		if err == nil {
			return nil
		}
		if parent.Err() != nil {
			return parent.Err()
		}
		if ctx.Err() != nil {
			return fmt.Errorf(
				"timed out after %s waiting for a free connection to %s: all max_total_connections = %d connections to this server are in use; increase max_total_connections or lower terraform -parallelism",
				wait, b.key, b.size)
		}
	}
}

// reapIdle closes one idle connection of the endpoint's pool that has the
// most, which gives its slot back to the budget. It reports whether it found
// one.
func (b *endpointBudget) reapIdle() bool {
	b.reapMu.Lock()
	defer b.reapMu.Unlock()

	var victim *budgetConnector
	idle := 0
	b.mu.Lock()
	for c := range b.pools {
		if c.maxIdle == 0 {
			continue
		}
		if n := c.db.Stats().Idle; n > idle {
			victim, idle = c, n
		}
	}
	b.mu.Unlock()
	if victim == nil {
		return false
	}

	// database/sql has no call to close a single idle connection, but
	// lowering the idle limit closes the ones above it synchronously (giving
	// their slots back through budgetConn.Close). SetMaxIdleConns(0) means
	// "keep none".
	victim.db.SetMaxIdleConns(idle - 1)
	victim.db.SetMaxIdleConns(victim.maxIdle)
	return true
}

// budgetConn is a physical connection holding one slot of an endpoint budget.
//
// It forwards every optional interface lib/pq's connection implements.
// database/sql looks these up with type assertions on the driver.Conn it got
// from the connector, so a wrapper that only embedded driver.Conn would make
// every query with arguments go through an extra Prepare round-trip, and lose
// session reset and validation.
type budgetConn struct {
	driver.Conn
	pool     *budgetConnector
	released atomic.Bool
}

var (
	_ driver.QueryerContext     = (*budgetConn)(nil)
	_ driver.ExecerContext      = (*budgetConn)(nil)
	_ driver.ConnBeginTx        = (*budgetConn)(nil)
	_ driver.ConnPrepareContext = (*budgetConn)(nil)
	_ driver.SessionResetter    = (*budgetConn)(nil)
	_ driver.Validator          = (*budgetConn)(nil)
	_ driver.NamedValueChecker  = (*budgetConn)(nil)
	_ driver.Pinger             = (*budgetConn)(nil)
)

func (c *budgetConn) Close() error {
	err := c.Conn.Close()
	if c.released.CompareAndSwap(false, true) {
		c.pool.budget.sem.Release(1)
	}
	return err
}

func (c *budgetConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *budgetConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	if opts.ReadOnly || opts.Isolation != driver.IsolationLevel(sql.LevelDefault) {
		return nil, errors.New("driver does not support non-default transaction options")
	}
	return c.Conn.Begin() //nolint:staticcheck // fallback for drivers without BeginTx
}

func (c *budgetConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *budgetConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid is called by database/sql when the connection is returned to its
// pool. While a dial of another pool of the same endpoint waits for a slot,
// report the connection as unusable so database/sql closes it instead of
// keeping it idle or handing it to the next query of this pool; the freed slot
// goes to the waiting dial. Waiting dials of the connection's own pool don't
// count: its queries can reuse the connection, and those dials wait for a
// slot held by another pool (each pool is capped at the budget size).
func (c *budgetConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok && !v.IsValid() {
		return false
	}
	return c.pool.budget.waiting.Load()-c.pool.waiting.Load() <= 0
}

func (c *budgetConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (c *budgetConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}
