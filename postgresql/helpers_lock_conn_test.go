package postgresql

import (
	"context"
	"database/sql"
	"testing"
)

// TestAccLockRoleConn checks that the session-level role lock taken by
// lockRoleConn conflicts with the transaction-level one taken by pgLockRole,
// and that release leaves a clean session behind when the connection is kept
// in the pool.
func TestAccLockRoleConn(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	// A connect_timeout unique to this test gives it its own pool in dbRegistry.
	config.ConnectTimeoutSec = 175
	config.MaxConns = 1
	config.ConnectionRetryTimeoutSeconds = 30
	db, err := config.NewClient("postgres").Connect()
	if err != nil {
		t.Fatalf("could not connect: %v", err)
	}
	// Keep the released connection so the next caller reuses the same session.
	db.SetMaxIdleConns(1)

	// Separate pool standing in for another resource using pgLockRole.
	otherConfig := getTestConfig(t)
	other, err := sql.Open("postgres", otherConfig.connStr("postgres"))
	if err != nil {
		t.Fatalf("could not open second pool: %v", err)
	}
	defer closeDB(t, other)

	tryXactLock := func() bool {
		t.Helper()
		txn, err := other.Begin()
		if err != nil {
			t.Fatalf("could not start transaction: %v", err)
		}
		defer deferredRollback(txn)
		var locked bool
		if err := txn.QueryRow(
			"SELECT pg_try_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = $1", config.Username,
		).Scan(&locked); err != nil {
			t.Fatalf("could not try advisory lock: %v", err)
		}
		return locked
	}

	var defaultTimeout string
	if err := other.QueryRow("SELECT current_setting('statement_timeout')").Scan(&defaultTimeout); err != nil {
		t.Fatalf("could not read statement_timeout: %v", err)
	}

	conn, release, err := lockRoleConn(db, config.Username)
	if err != nil {
		t.Fatalf("lockRoleConn: %v", err)
	}
	var lockedPid int
	if err := conn.QueryRow("SELECT pg_backend_pid()").Scan(&lockedPid); err != nil {
		t.Fatalf("could not read backend pid: %v", err)
	}
	var timeout string
	if err := conn.QueryRow("SELECT current_setting('statement_timeout')").Scan(&timeout); err != nil {
		t.Fatalf("could not read statement_timeout: %v", err)
	}
	if timeout != defaultTimeout {
		t.Errorf("statement_timeout under lock = %q, want %q", timeout, defaultTimeout)
	}
	if tryXactLock() {
		t.Error("transaction-level lock on the role was granted while lockRoleConn held it")
	}
	release()

	if !tryXactLock() {
		t.Error("transaction-level lock on the role is still blocked after release")
	}

	ctx := context.Background()
	reused, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("could not get connection: %v", err)
	}
	defer func() {
		if err := reused.Close(); err != nil {
			t.Errorf("could not close connection: %v", err)
		}
	}()
	var pid, advisoryLocks int
	if err := reused.QueryRowContext(ctx,
		"SELECT pg_backend_pid(), (SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid())",
	).Scan(&pid, &advisoryLocks); err != nil {
		t.Fatalf("could not read session state: %v", err)
	}
	if pid != lockedPid {
		t.Fatalf("released connection was not reused (pid %d, want %d)", pid, lockedPid)
	}
	if advisoryLocks != 0 {
		t.Errorf("released session still holds %d advisory locks", advisoryLocks)
	}
}
