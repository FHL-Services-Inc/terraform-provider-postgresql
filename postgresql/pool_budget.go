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

const (
	// budgetReapInterval is how often a dial waiting for a slot of an
	// exhausted endpoint budget looks for idle connections to close.
	budgetReapInterval = 50 * time.Millisecond

	// budgetBalanceInterval is how often the budget looks at pools whose open
	// limit it lowered, to give them free slots or slots of connections that
	// sit idle elsewhere.
	budgetBalanceInterval = 20 * time.Millisecond

	// budgetColdIdle is how long a connection must have been idle before
	// it is closed to give its slot to a pool whose queries are waiting.
	// Connections of a busy pool are reused within milliseconds.
	budgetColdIdle = 250 * time.Millisecond

	// budgetFairWait: when the queries of a pool wait longer than this on
	// average, and at least twice as long as those of another pool, that
	// other pool gives up one connection to it.
	budgetFairWait = 50 * time.Millisecond

	// budgetGrace: right after a pool's limit was raised, a dial that finds
	// the budget full briefly waits for a slot instead of going back to the
	// pool's queue (see budgetConnector.onBudgetFull).
	budgetGrace = 10 * time.Millisecond

	// budgetUnshrinkAfter: limits are restored once no limited pool had a
	// waiting query for this many balance intervals.
	budgetUnshrinkAfter = 25
)

// endpointBudget caps the number of physical connections the provider keeps
// open to one server endpoint (scheme, host, port and user), across all the
// per-database pools that connect there.
//
// A slot is taken before a connection is dialed and given back when it is
// closed. When the budget is full, a query of a pool that already holds a
// connection waits in that pool's own queue for one of its connections,
// like it does when the pool reaches max_connections: the connector lowers
// the pool's open limit to what it holds and returns driver.ErrBadConn, so
// database/sql retries and queues the query. A slot moves to another pool
// only when that pool holds no connection at all (it then waits for a slot,
// see acquire), when a connection has been idle for a while (budgetColdIdle),
// or when a pool's queries wait much longer than another pool's
// (budgetFairWait). Waiting for a slot is bounded: the caller gets an error
// after connect_timeout instead of hanging.
type endpointBudget struct {
	key  string
	size int
	sem  *semaphore.Weighted
	// used counts the slots taken.
	used atomic.Int32

	// waiting counts dials of pools without any connection that are blocked
	// on the semaphore.
	waiting atomic.Int32

	mu        sync.Mutex
	pools     map[*budgetConnector]struct{}
	balancing bool

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
// count against budget b. maxOpen and maxIdle are the pool's open and idle
// limits (maxOpen must be > 0); the budget lowers the open limit temporarily
// and restores both. wait bounds how long a dial waits for a free slot.
func openBudgetedDB(inner driver.Connector, b *endpointBudget, maxOpen, maxIdle int, wait time.Duration) *sql.DB {
	c := &budgetConnector{
		inner:   inner,
		budget:  b,
		maxOpen: maxOpen,
		maxIdle: maxIdle,
		wait:    wait,
		limit:   maxOpen,
		conns:   map[*budgetConn]struct{}{},
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
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
	maxOpen int
	maxIdle int
	wait    time.Duration
	db      *sql.DB

	// waiting counts dials of this pool blocked on the budget.
	waiting atomic.Int32

	// applyMu serializes changes of the pool's limits.
	applyMu sync.Mutex

	// Guarded by budget.mu.
	open     int // physical connections
	limit    int // open limit set on db, maxOpen unless lowered
	raisedAt time.Time
	evict    int // returned connections to close for another pool
	conns    map[*budgetConn]struct{}
	// database/sql wait counters at the last balance round
	lastWaitCount    int64
	lastWaitDuration time.Duration
}

func (c *budgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	b := c.budget
	if !b.tryTake() {
		switch c.onBudgetFull(ctx) {
		case budgetQueue:
			return nil, driver.ErrBadConn
		case budgetTransfer:
			if err := b.acquire(ctx, c); err != nil {
				return nil, err
			}
		case budgetTaken:
		}
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		b.give()
		return nil, err
	}
	bc := &budgetConn{Conn: conn, pool: c}
	b.mu.Lock()
	c.open++
	c.conns[bc] = struct{}{}
	b.mu.Unlock()
	return bc, nil
}

type budgetFullAction int

const (
	budgetQueue    budgetFullAction = iota // wait in the pool's own queue
	budgetTransfer                         // wait for a slot of another pool
	budgetTaken                            // got a slot after all
)

// onBudgetFull decides what a dial does when no slot is free.
//
// A pool that holds connections lowers its open limit to that number and
// sends the query back to database/sql with driver.ErrBadConn: database/sql
// retries it right away (twice with cachedOrNewConn, then once with
// alwaysNewConn), finds the pool at its limit and queues the query until one
// of the pool's connections is returned. The ErrBadConn never reaches the
// caller as long as the limit is still in place when the retry runs; the only
// thing that raises it is the balancer, after which a dial first waits
// budgetGrace for a slot instead of returning ErrBadConn again.
//
// A pool without connections has nothing to wait for and takes a slot from
// another pool (budgetTransfer).
func (c *budgetConnector) onBudgetFull(ctx context.Context) budgetFullAction {
	b := c.budget
	b.mu.Lock()
	open := c.open
	inGrace := time.Since(c.raisedAt) < budgetGrace
	b.mu.Unlock()
	if open == 0 {
		return budgetTransfer
	}
	if inGrace {
		wctx, cancel := context.WithTimeout(ctx, budgetGrace)
		err := b.sem.Acquire(wctx, 1)
		cancel()
		if err == nil {
			b.used.Add(1)
			return budgetTaken
		}
	}
	c.setLimit(func(int) int { return open })
	return budgetQueue
}

// setLimit changes the pool's open limit to f(open connections), at least 1
// and at most maxOpen, and applies it. A limit below maxOpen starts the
// balancer.
func (c *budgetConnector) setLimit(f func(open int) int) {
	b := c.budget
	b.mu.Lock()
	l := f(c.open)
	if l < 1 {
		l = 1
	}
	if l > c.maxOpen {
		l = c.maxOpen
	}
	if l > c.limit {
		c.raisedAt = time.Now()
	}
	c.limit = l
	startBalancer := l < c.maxOpen && !b.balancing
	if startBalancer {
		b.balancing = true
	}
	b.mu.Unlock()

	c.applyLimit()
	if startBalancer {
		go b.balance()
	}
}

// applyLimit sets the pool's current limits on its sql.DB. It must not be
// called with budget.mu held: lowering a limit closes idle connections, which
// gives their slots back.
func (c *budgetConnector) applyLimit() {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.budget.mu.Lock()
	l := c.limit
	c.budget.mu.Unlock()
	c.db.SetMaxOpenConns(l)
	// Lowering the open limit also lowers the idle limit; restore it (it is
	// capped at the open limit).
	c.db.SetMaxIdleConns(c.maxIdle)
}

// takeEvict reports whether a returned connection of the pool should be
// closed to give its slot to another pool.
func (c *budgetConnector) takeEvict() bool {
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	if c.evict == 0 {
		return false
	}
	c.evict--
	return true
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

// tryTake takes a slot if one is free.
func (b *endpointBudget) tryTake() bool {
	if !b.sem.TryAcquire(1) {
		return false
	}
	b.used.Add(1)
	return true
}

// give returns a slot.
func (b *endpointBudget) give() {
	b.used.Add(-1)
	b.sem.Release(1)
}

// acquire takes a slot for a dial of pool, which holds no connection,
// closing idle connections of the endpoint's pools if none is free, and waits
// at most pool.wait for one.
func (b *endpointBudget) acquire(ctx context.Context, pool *budgetConnector) error {
	if b.tryTake() {
		return nil
	}

	log.Printf("[DEBUG] connection budget %s full: a pool without connections waits for a slot", b.key)
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
		if b.reapIdle(nil) && b.tryTake() {
			return nil
		}
		pollCtx, cancel := context.WithTimeout(ctx, budgetReapInterval)
		err := b.sem.Acquire(pollCtx, 1)
		cancel()
		if err == nil {
			b.used.Add(1)
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

// reapIdle closes one idle connection of victim, or, if victim is nil, of the
// endpoint's pool that has the most idle connections, which gives its slot
// back to the budget. It reports whether it found one.
func (b *endpointBudget) reapIdle(victim *budgetConnector) bool {
	b.reapMu.Lock()
	defer b.reapMu.Unlock()

	var pools []*budgetConnector
	if victim != nil {
		pools = []*budgetConnector{victim}
	} else {
		pools = b.poolList()
	}
	victim = nil
	idle := 0
	for _, c := range pools {
		if c.maxIdle == 0 {
			continue
		}
		if n := c.db.Stats().Idle; n > idle {
			victim, idle = c, n
		}
	}
	if victim == nil {
		return false
	}

	// database/sql has no call to close a single idle connection, but
	// lowering the idle limit closes the ones above it synchronously (giving
	// their slots back through budgetConn.Close). SetMaxIdleConns(0) means
	// "keep none".
	victim.applyMu.Lock()
	defer victim.applyMu.Unlock()
	victim.db.SetMaxIdleConns(idle - 1)
	victim.db.SetMaxIdleConns(victim.maxIdle)
	return true
}

func (b *endpointBudget) poolList() []*budgetConnector {
	b.mu.Lock()
	defer b.mu.Unlock()
	pools := make([]*budgetConnector, 0, len(b.pools))
	for c := range b.pools {
		pools = append(pools, c)
	}
	return pools
}

// balance runs while some pool's open limit is lowered. Every
// budgetBalanceInterval it picks the limited pool whose queries waited the
// most since the last round and raises its limit by one when a slot is free,
// a connection elsewhere has been idle for budgetColdIdle, or another pool's
// queries wait much less (budgetFairWait). Limits are restored when no limited
// pool had waiting queries for budgetUnshrinkAfter rounds.
func (b *endpointBudget) balance() {
	quiet := 0
	for {
		time.Sleep(budgetBalanceInterval)

		type poolWait struct {
			c       *budgetConnector
			limited bool
			count   int64
			avg     time.Duration
		}
		var waits []poolWait
		for _, c := range b.poolList() {
			st := c.db.Stats()
			b.mu.Lock()
			w := poolWait{c: c, limited: c.limit < c.maxOpen, count: st.WaitCount - c.lastWaitCount}
			if w.count > 0 {
				w.avg = (st.WaitDuration - c.lastWaitDuration) / time.Duration(w.count)
			}
			c.lastWaitCount, c.lastWaitDuration = st.WaitCount, st.WaitDuration
			b.mu.Unlock()
			waits = append(waits, w)
		}

		var needy *poolWait
		anyLimited := false
		for i := range waits {
			w := &waits[i]
			anyLimited = anyLimited || w.limited
			if w.limited && w.count > 0 && (needy == nil || w.avg*time.Duration(w.count) > needy.avg*time.Duration(needy.count)) {
				needy = w
			}
		}
		if !anyLimited {
			b.mu.Lock()
			b.balancing = false
			b.mu.Unlock()
			return
		}
		if needy == nil {
			quiet++
			if quiet >= budgetUnshrinkAfter {
				for _, w := range waits {
					if w.limited {
						w.c.setLimit(func(int) int { return w.c.maxOpen })
					}
				}
			}
			continue
		}
		quiet = 0
		raise := func() { needy.c.setLimit(func(int) int { return needy.c.limit + 1 }) }

		if int(b.used.Load()) < b.size {
			raise()
			continue
		}
		if victim := b.coldIdlePool(needy.c); victim != nil && b.reapIdle(victim) {
			log.Printf("[DEBUG] connection budget %s: closed a connection idle for %s for a pool with waiting queries", b.key, budgetColdIdle)
			raise()
			continue
		}
		if needy.avg >= budgetFairWait {
			var donor *poolWait
			for i := range waits {
				w := &waits[i]
				if w.c == needy.c || w.avg*2 > needy.avg {
					continue
				}
				b.mu.Lock()
				ok := w.c.open > 1 && w.c.evict == 0
				b.mu.Unlock()
				if ok && (donor == nil || w.avg < donor.avg) {
					donor = w
				}
			}
			if donor != nil {
				log.Printf("[DEBUG] connection budget %s: queries of one pool wait %s on average, another pool gives up a connection", b.key, needy.avg)
				b.mu.Lock()
				donor.c.evict++
				b.mu.Unlock()
			}
		}
	}
}

// coldIdlePool returns a pool other than except with a connection that has
// been idle for at least budgetColdIdle.
func (b *endpointBudget) coldIdlePool(except *budgetConnector) *budgetConnector {
	cutoff := time.Now().Add(-budgetColdIdle).UnixNano()
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.pools {
		if c == except || c.maxIdle == 0 {
			continue
		}
		for conn := range c.conns {
			if t := conn.idleSince.Load(); t != 0 && t <= cutoff {
				return c
			}
		}
	}
	return nil
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
	// idleSince is when the connection was returned to its pool (UnixNano),
	// 0 while in use.
	idleSince atomic.Int64
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
		b := c.pool.budget
		b.mu.Lock()
		c.pool.open--
		delete(c.pool.conns, c)
		b.mu.Unlock()
		b.give()
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
	// Called by database/sql when the connection is taken from the pool.
	c.idleSince.Store(0)
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid is called by database/sql when the connection is returned to its
// pool. The connection is reported as unusable, so database/sql closes it
// instead of keeping it idle or handing it to the next query of this pool,
// when its slot is needed elsewhere: while a dial of another pool of the
// endpoint that holds no connection waits for a slot (waiting dials of the
// connection's own pool don't count: its queries can reuse the connection),
// or when the balancer asked this pool to give up a connection. The pool's
// open limit is lowered first, so database/sql doesn't dial a replacement
// for queries queued in this pool.
func (c *budgetConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok && !v.IsValid() {
		return false
	}
	p := c.pool
	if p.budget.waiting.Load()-p.waiting.Load() > 0 || p.takeEvict() {
		log.Printf("[DEBUG] connection budget %s: closing a returned connection to give its slot to another pool", p.budget.key)
		p.setLimit(func(open int) int { return open - 1 })
		return false
	}
	c.idleSince.Store(time.Now().UnixNano())
	return true
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
