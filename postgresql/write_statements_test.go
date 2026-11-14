package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/lib/pq"
)

// writeStmtDriver is a fake database/sql driver that records every statement
// sent to the server (Exec and Query alike, i.e. every round-trip) and whether
// it carried bind parameters. Queries return no rows. When execErr is set,
// every Exec fails with it.
type writeStmtDriver struct {
	mu      sync.Mutex
	stmts   []writeStmt
	execErr error
}

type writeStmt struct {
	query string
	args  int
	// committed is true for statements sent after the first COMMIT.
	committed bool
}

func (d *writeStmtDriver) reset(execErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = nil
	d.execErr = execErr
}

func (d *writeStmtDriver) record(query string, args int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	committed := false
	for _, s := range d.stmts {
		if s.query == "COMMIT" {
			committed = true
		}
	}
	d.stmts = append(d.stmts, writeStmt{query: query, args: args, committed: committed})
}

// inTxn returns the statements sent before the first COMMIT, i.e. the ones of
// the write transaction.
func (d *writeStmtDriver) inTxn() []writeStmt {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []writeStmt
	for _, s := range d.stmts {
		if s.committed || s.query == "COMMIT" {
			break
		}
		out = append(out, s)
	}
	return out
}

func (d *writeStmtDriver) Open(string) (driver.Conn, error) {
	return &writeStmtConn{driver: d}, nil
}

type writeStmtConn struct {
	driver *writeStmtDriver
}

func (c *writeStmtConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *writeStmtConn) Close() error              { return nil }
func (c *writeStmtConn) Begin() (driver.Tx, error) { return writeStmtTx{c.driver}, nil }

func (c *writeStmtConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.driver.record(query, len(args))
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	if c.driver.execErr != nil {
		return nil, c.driver.execErr
	}
	return driver.RowsAffected(0), nil
}

func (c *writeStmtConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.driver.record(query, len(args))
	return writeStmtRows{}, nil
}

type writeStmtTx struct{ driver *writeStmtDriver }

func (t writeStmtTx) Commit() error   { t.driver.record("COMMIT", 0); return nil }
func (t writeStmtTx) Rollback() error { return nil }

type writeStmtRows struct{}

func (writeStmtRows) Columns() []string         { return []string{"c"} }
func (writeStmtRows) Close() error              { return nil }
func (writeStmtRows) Next([]driver.Value) error { return io.EOF }

const writeStmtDriverName = "pg-write-stmt-counter"

var (
	writeStmtCounter         = &writeStmtDriver{}
	registerWriteStmtCounter sync.Once
)

const writeStmtDatabase = "writetestdb"

// newWriteStmtConnection returns a DBConnection backed by writeStmtCounter and
// registers it in dbRegistry for writeStmtDatabase, so startTransaction in the
// resource functions resolves to it instead of dialing a server.
func newWriteStmtConnection(t *testing.T, advisoryLockTimeout int, execErr error) *DBConnection {
	t.Helper()
	registerWriteStmtCounter.Do(func() { sql.Register(writeStmtDriverName, writeStmtCounter) })
	writeStmtCounter.reset(execErr)

	sqlDB, err := sql.Open(writeStmtDriverName, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	// A non-"postgres" scheme keeps connStr down to a single query parameter,
	// so the registry key is stable.
	client := &Client{
		config:       Config{Scheme: "awspostgres", AdvisoryLockTimeout: advisoryLockTimeout},
		databaseName: writeStmtDatabase,
	}
	conn := &DBConnection{DB: sqlDB, client: client, version: semver.MustParse("16.0.0")}

	dsn := client.config.connStr(client.databaseName)
	dbRegistryLock.Lock()
	dbRegistry[dsn] = conn
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, dsn)
		dbRegistryLock.Unlock()
		_ = sqlDB.Close()
	})

	return conn
}

func assertNoBindParameters(t *testing.T, stmts []writeStmt) {
	t.Helper()
	for _, s := range stmts {
		if s.args != 0 {
			t.Errorf("statement sent with %d bind parameters, a multi-statement string must have none: %q", s.args, s.query)
		}
	}
}

func lockRoleStatements(t *testing.T, timeoutMs int, role string) []writeStmt {
	t.Helper()
	db := newWriteStmtConnection(t, timeoutMs, nil)
	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer deferredRollback(txn)

	if err := pgLockRole(db, txn, role); err != nil {
		t.Fatalf("pgLockRole: %v", err)
	}
	return writeStmtCounter.inTxn()
}

func TestPgLockRoleSingleRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeoutMs int
		want      []string
	}{
		{
			name:      "no advisory_lock_timeout",
			timeoutMs: 0,
			want: []string{
				"SET LOCAL statement_timeout = 0",
				"SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = 'r1'",
				"SELECT pg_advisory_xact_lock(member::bigint) FROM pg_auth_members JOIN pg_roles ON roleid = pg_roles.oid WHERE rolname = 'r1' ORDER BY member",
			},
		},
		{
			name:      "advisory_lock_timeout 500ms",
			timeoutMs: 500,
			want: []string{
				"SET LOCAL statement_timeout = 0",
				"current_setting('lock_timeout')",
				"SET LOCAL lock_timeout = 500",
				"SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = 'r1'",
				"ORDER BY member",
				"SELECT set_config('lock_timeout',",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmts := lockRoleStatements(t, tc.timeoutMs, "r1")
			if len(stmts) != 1 {
				t.Fatalf("pgLockRole sent %d statements, want 1 round-trip: %+v", len(stmts), stmts)
			}
			assertNoBindParameters(t, stmts)

			// The parts must be present and in order.
			rest := stmts[0].query
			for _, part := range tc.want {
				i := strings.Index(rest, part)
				if i < 0 {
					t.Fatalf("statement is missing %q (or it is out of order):\n%s", part, stmts[0].query)
				}
				rest = rest[i+len(part):]
			}
		})
	}
}

func TestPgLockRoleQuotesRoleName(t *testing.T) {
	stmts := lockRoleStatements(t, 0, `o'q\b; DROP ROLE x`)
	if len(stmts) != 1 {
		t.Fatalf("pgLockRole sent %d statements, want 1: %+v", len(stmts), stmts)
	}
	want := `rolname =  E'o''q\\b; DROP ROLE x'`
	if got := strings.Count(stmts[0].query, want); got != 2 {
		t.Fatalf("role literal %s found %d times, want 2 (role and members lock):\n%s", want, got, stmts[0].query)
	}
}

func TestPgLockDatabaseSingleRoundTrip(t *testing.T) {
	for _, timeoutMs := range []int{0, 500} {
		db := newWriteStmtConnection(t, timeoutMs, nil)
		txn, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := pgLockDatabase(db, txn, "d1"); err != nil {
			t.Fatalf("pgLockDatabase: %v", err)
		}
		stmts := writeStmtCounter.inTxn()
		deferredRollback(txn)

		if len(stmts) != 1 {
			t.Fatalf("timeout %d: pgLockDatabase sent %d statements, want 1: %+v", timeoutMs, len(stmts), stmts)
		}
		assertNoBindParameters(t, stmts)
		if !strings.Contains(stmts[0].query, "FROM pg_database WHERE datname = 'd1'") {
			t.Fatalf("timeout %d: unexpected statement:\n%s", timeoutMs, stmts[0].query)
		}
	}
}

func TestPgLockRoleErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeoutMs int
		execErr   error
		want      string
		lockErr   bool
	}{
		{
			name:      "lock timeout with advisory_lock_timeout",
			timeoutMs: 300,
			execErr:   &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"},
			want:      `timed out after 300ms waiting for advisory lock on role "r1"`,
			lockErr:   true,
		},
		{
			name:      "lock timeout from a session lock_timeout without advisory_lock_timeout",
			timeoutMs: 0,
			execErr:   &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"},
			want:      "could not get advisory lock for role r1: pq: canceling statement due to lock timeout",
		},
		{
			name:      "other error",
			timeoutMs: 300,
			execErr:   &pq.Error{Code: "40P01", Message: "deadlock detected"},
			want:      "could not get advisory lock for role r1: pq: deadlock detected",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newWriteStmtConnection(t, tc.timeoutMs, tc.execErr)
			txn, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer deferredRollback(txn)

			err = pgLockRole(db, txn, "r1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("pgLockRole error = %v, want it to contain %q", err, tc.want)
			}
			var pqErr *pq.Error
			if !errors.As(err, &pqErr) {
				t.Fatalf("pgLockRole error does not wrap the *pq.Error: %v", err)
			}
			if tc.lockErr && !strings.Contains(err.Error(), "advisory_lock_timeout") {
				t.Fatalf("lock timeout error does not point at advisory_lock_timeout: %v", err)
			}
		})
	}
}

func TestGrantCreateWriteStatements(t *testing.T) {
	for _, tc := range []struct {
		timeoutMs int
	}{{0}, {500}} {
		db := newWriteStmtConnection(t, tc.timeoutMs, nil)

		d := resourcePostgreSQLGrant().TestResourceData()
		for k, v := range map[string]any{
			"database":    writeStmtDatabase,
			"role":        "r1",
			"object_type": "database",
			"privileges":  []any{"CONNECT"},
		} {
			if err := d.Set(k, v); err != nil {
				t.Fatalf("set %s: %v", k, err)
			}
		}

		// The read after the commit finds nothing on the fake driver; only the
		// write transaction matters here.
		_ = resourcePostgreSQLGrantCreate(db, d)

		stmts := writeStmtCounter.inTxn()
		assertNoBindParameters(t, stmts)
		// role and database locks, REVOKE+GRANT
		if len(stmts) != 2 {
			t.Fatalf("timeout %d: grant Create sent %d statements in its write transaction, want 2: %+v", tc.timeoutMs, len(stmts), stmts)
		}
		for _, part := range []string{"FROM pg_roles WHERE rolname = 'r1'", "FROM pg_database WHERE datname = 'writetestdb'"} {
			if !strings.Contains(stmts[0].query, part) {
				t.Fatalf("timeout %d: lock preamble is missing %q:\n%s", tc.timeoutMs, part, stmts[0].query)
			}
		}
		if strings.Contains(stmts[0].query, "pg_auth_members") {
			t.Fatalf("timeout %d: grant Create locks the members of the grantee:\n%s", tc.timeoutMs, stmts[0].query)
		}
		want := `REVOKE ALL PRIVILEGES ON DATABASE "writetestdb" FROM "r1";` + "\n" + `GRANT CONNECT ON DATABASE "writetestdb" TO "r1"`
		if stmts[1].query != want {
			t.Fatalf("timeout %d: revoke/grant statement = %q, want %q", tc.timeoutMs, stmts[1].query, want)
		}
	}
}

func TestGrantRoleCreateWriteStatements(t *testing.T) {
	db := newWriteStmtConnection(t, 0, nil)

	d := resourcePostgreSQLGrantRole().TestResourceData()
	for k, v := range map[string]any{
		"role":       "r1",
		"grant_role": "g1",
	} {
		if err := d.Set(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}

	_ = resourcePostgreSQLGrantRoleCreate(db, d)

	stmts := writeStmtCounter.inTxn()
	assertNoBindParameters(t, stmts)
	want := `REVOKE "g1" FROM "r1";` + "\n" + `GRANT "g1" TO "r1"`
	if len(stmts) != 1 || stmts[0].query != want {
		t.Fatalf("grant_role Create sent %+v, want exactly one statement %q", stmts, want)
	}
}
