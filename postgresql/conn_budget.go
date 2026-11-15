package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

// budgetReapInterval is how often a dial waiting for a slot of a full budget
// looks again for an idle connection to close.
const budgetReapInterval = 50 * time.Millisecond

// connBudget caps the physical connections a provider configuration keeps
// open to its server across all its per-database pools
// (max_total_connections). A slot is taken before a connection is dialed and
// given back when the connection is closed.
//
// When no slot is free, the dial closes an idle connection of one of the
// pools and takes its slot; otherwise it waits, at most connect_timeout, and
// then fails with an error instead of hanging.
type connBudget struct {
	name  string // scheme://user@host:port, for the error message
	size  int
	slots chan struct{}

	// waiting counts dials blocked on the budget, across all pools.
	waiting atomic.Int32

	mu    sync.Mutex // guards pools and serializes reapIdle
	pools map[*budgetConnector]struct{}
}

var (
	connBudgetsLock sync.Mutex
	connBudgets     = map[string]*connBudget{}
)

func newConnBudget(name string, size int) *connBudget {
	return &connBudget{
		name:  name,
		size:  size,
		slots: make(chan struct{}, size),
		pools: map[*budgetConnector]struct{}{},
	}
}

// connBudget returns the budget shared by every pool of this configuration,
// or nil when max_total_connections is off. It is only supported for the
// postgres scheme: gocloud opens the awspostgres and gcppostgres pools itself.
//
// Under Terraform each provider block runs in its own plugin process, so the
// budget is per provider block. The key includes the size so configurations
// in the same process (acceptance tests) never share a budget of another
// size.
func (c *Config) connBudget() *connBudget {
	if c.MaxTotalConns <= 0 || c.Scheme != "postgres" {
		return nil
	}
	name := fmt.Sprintf("%s://%s@%s:%d", c.Scheme, c.Username, c.Host, c.Port)
	key := fmt.Sprintf("%s#%d", name, c.MaxTotalConns)

	connBudgetsLock.Lock()
	defer connBudgetsLock.Unlock()
	b, ok := connBudgets[key]
	if !ok {
		b = newConnBudget(name, c.MaxTotalConns)
		connBudgets[key] = b
	}
	return b
}

// budgetWait is how long a dial waits for a free slot: connect_timeout, or
// its default when connect_timeout is 0 ("wait indefinitely"), so a budget
// that is too small fails instead of hanging.
func (c *Config) budgetWait() time.Duration {
	if c.ConnectTimeoutSec > 0 {
		return time.Duration(c.ConnectTimeoutSec) * time.Second
	}
	return defaultProviderConnectTimeoutSeconds * time.Second
}

// openBudgetedDB opens a pool whose connections, dialed by inner, take slots
// of b. maxIdle is the pool's max_idle_connections; wait bounds how long a
// dial waits for a slot.
func openBudgetedDB(inner driver.Connector, b *connBudget, maxIdle int, wait time.Duration) *sql.DB {
	c := &budgetConnector{inner: inner, budget: b, maxIdle: maxIdle, wait: wait}
	c.db = sql.OpenDB(c)
	b.mu.Lock()
	b.pools[c] = struct{}{}
	b.mu.Unlock()
	return c.db
}

// budgetConnector is the driver.Connector of one pool.
type budgetConnector struct {
	inner   driver.Connector
	budget  *connBudget
	db      *sql.DB
	maxIdle int
	wait    time.Duration
}

// pqDriverConn is the set of driver interfaces lib/pq's connection
// implements. database/sql finds its fast paths (no extra Prepare round-trip
// for statements with arguments, BeginTx, session reset) by type assertions
// on the connection, so the wrapper must implement them all.
type pqDriverConn interface {
	driver.Conn
	driver.QueryerContext
	driver.ExecerContext
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.SessionResetter
	driver.Validator
	driver.NamedValueChecker
	driver.Pinger
}

func (c *budgetConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := c.budget.acquire(ctx, c); err != nil {
		return nil, err
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		c.budget.release()
		return nil, err
	}
	pc, ok := conn.(pqDriverConn)
	if !ok {
		_ = conn.Close()
		c.budget.release()
		return nil, fmt.Errorf("max_total_connections: unsupported driver connection %T", conn)
	}
	return &budgetConn{pqDriverConn: pc, pool: c}, nil
}

func (c *budgetConnector) Driver() driver.Driver { return c.inner.Driver() }

// Close is called by sql.DB.Close. Connections still in use give their slots
// back when they are closed.
func (c *budgetConnector) Close() error {
	c.budget.mu.Lock()
	delete(c.budget.pools, c)
	c.budget.mu.Unlock()
	if closer, ok := c.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (b *connBudget) release() { <-b.slots }

// acquire takes a slot for a dial of pool. When none is free it closes an
// idle connection, and retries every budgetReapInterval until pool.wait runs
// out.
func (b *connBudget) acquire(ctx context.Context, pool *budgetConnector) error {
	select {
	case b.slots <- struct{}{}:
		return nil
	default:
	}

	// Registered as waiting first, so connections returned to their pool
	// from now on are closed (budgetConn.IsValid).
	b.waiting.Add(1)
	defer b.waiting.Add(-1)

	timeout := time.NewTimer(pool.wait)
	defer timeout.Stop()
	tick := time.NewTicker(budgetReapInterval)
	defer tick.Stop()
	for {
		b.reapIdle()
		select {
		case b.slots <- struct{}{}:
			return nil
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("timed out after %s waiting for a free connection to %s: all max_total_connections = %d connections are in use; "+
				"increase max_total_connections (at least terraform -parallelism + the number of databases) or lower -parallelism",
				pool.wait, b.name, b.size)
		}
	}
}

// reapIdle closes one idle connection of the pool that has the most, which
// gives its slot back.
func (b *connBudget) reapIdle() {
	b.mu.Lock()
	defer b.mu.Unlock()

	var victim *budgetConnector
	idle := 0
	for c := range b.pools {
		if n := c.db.Stats().Idle; n > idle {
			victim, idle = c, n
		}
	}
	if victim == nil {
		return
	}
	// database/sql cannot close a single idle connection, but lowering the
	// idle limit closes the connections above it right away (each gives its
	// slot back in budgetConn.Close). SetMaxIdleConns(0) keeps none.
	victim.db.SetMaxIdleConns(idle - 1)
	victim.db.SetMaxIdleConns(victim.maxIdle)
}

// budgetConn is a physical connection holding one slot. It forwards every
// pqDriverConn interface to the lib/pq connection.
type budgetConn struct {
	pqDriverConn
	pool     *budgetConnector
	released atomic.Bool
}

func (c *budgetConn) Close() error {
	err := c.pqDriverConn.Close()
	if c.released.CompareAndSwap(false, true) {
		c.pool.budget.release()
	}
	return err
}

// IsValid is called by database/sql when the connection goes back to its pool
// or is taken from it. While a dial waits for a slot, it reports false so
// database/sql closes the connection and the slot goes to that dial.
// Otherwise a pool whose queries keep all its connections busy would hand
// them from query to query, and the dial would wait for the whole timeout.
func (c *budgetConn) IsValid() bool {
	return c.pqDriverConn.IsValid() && c.pool.budget.waiting.Load() == 0
}

// openBudgetedPostgresDB opens a pool for the postgres scheme whose
// connections take slots of budget. They are dialed as without the budget:
// through proxyDriver (ALL_PROXY / HTTPS_PROXY), with an auth token when one
// is configured.
func (c *Config) openBudgetedPostgresDB(dsn string, budget *connBudget) (*sql.DB, error) {
	var inner driver.Connector
	if c.AuthTokens != nil {
		tc, err := c.newPostgresConnector(dsn)
		if err != nil {
			return nil, err
		}
		inner = tc
	} else {
		pc, err := pq.NewConnector(dsn)
		if err != nil {
			return nil, err
		}
		pc.Dialer(proxyDriver{})
		inner = pc
	}
	return openBudgetedDB(inner, budget, c.MaxIdleConns, c.budgetWait()), nil
}
