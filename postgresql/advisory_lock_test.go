package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

func TestIsLockTimeoutError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pq lock_not_available", &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"}, true},
		{"wrapped pq lock_not_available", fmt.Errorf("exec: %w", &pq.Error{Code: "55P03"}), true},
		{"plain error with lock timeout message", errors.New("pq: canceling statement due to lock timeout"), true},
		{"pq statement timeout", &pq.Error{Code: "57014", Message: "canceling statement due to statement timeout"}, false},
		{"unrelated error", errors.New("connection refused"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLockTimeoutError(tt.err); got != tt.want {
				t.Fatalf("isLockTimeoutError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestProviderAdvisoryLockTimeoutDefault(t *testing.T) {
	s, ok := Provider().Schema["advisory_lock_timeout"]
	if !ok {
		t.Fatal("provider schema has no advisory_lock_timeout argument")
	}
	if s.Default != 0 {
		t.Fatalf("advisory_lock_timeout default = %v, want 0 (wait indefinitely)", s.Default)
	}
}

// openSingleConnLockDB returns a DBConnection backed by a pool limited to one
// physical connection, so session state can be inspected across transactions.
func openSingleConnLockDB(t *testing.T, timeoutMs int) *DBConnection {
	config := getTestConfig(t)
	config.AdvisoryLockTimeout = timeoutMs

	raw, err := sql.Open("postgres", config.connStr("postgres"))
	if err != nil {
		t.Fatalf("could not open connection: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { closeDB(t, raw) })

	return &DBConnection{DB: raw, client: config.NewClient("postgres")}
}

// holdRoleAdvisoryLock takes a session-level advisory lock on the role OID in
// the given database from a dedicated connection, the same key pgLockRole waits
// for (advisory lock keys are per database). The returned function releases it.
func holdRoleAdvisoryLock(t *testing.T, database, role string) func() {
	config := getTestConfig(t)
	holder, err := sql.Open("postgres", config.connStr(database))
	if err != nil {
		t.Fatalf("could not open holder connection: %v", err)
	}
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("could not get holder connection: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		"SELECT pg_advisory_lock(oid::bigint) FROM pg_roles WHERE rolname = $1", role,
	); err != nil {
		t.Fatalf("could not take advisory lock: %v", err)
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = conn.Close()
		_ = holder.Close()
	}
}

func TestAccAdvisoryLockTimeoutScopedToTransaction(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()
	_, roleName := getTestDBNames(dbSuffix)

	db := openSingleConnLockDB(t, 500)

	// Session-level values that must survive the lock transaction untouched.
	if _, err := db.Exec("SET statement_timeout = '123s'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("SET lock_timeout = '45s'"); err != nil {
		t.Fatal(err)
	}

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := pgLockRole(db, txn, roleName); err != nil {
		t.Fatalf("pgLockRole on an uncontended role failed: %v", err)
	}

	var stmtTimeout, lockTimeout string
	if err := txn.QueryRow("SELECT current_setting('statement_timeout'), current_setting('lock_timeout')").Scan(&stmtTimeout, &lockTimeout); err != nil {
		t.Fatal(err)
	}
	if stmtTimeout != "0" {
		t.Errorf("statement_timeout inside lock txn = %q, want 0", stmtTimeout)
	}
	if lockTimeout != "45s" {
		t.Errorf("lock_timeout after acquiring the lock = %q, want the previous value 45s restored", lockTimeout)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := db.QueryRow("SELECT current_setting('statement_timeout'), current_setting('lock_timeout')").Scan(&stmtTimeout, &lockTimeout); err != nil {
		t.Fatal(err)
	}
	if stmtTimeout != "123s" || lockTimeout != "45s" {
		t.Errorf("session settings leaked after commit: statement_timeout=%q lock_timeout=%q, want 123s/45s", stmtTimeout, lockTimeout)
	}
}

func TestAccAdvisoryLockTimeoutRoleContended(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()
	_, roleName := getTestDBNames(dbSuffix)

	release := holdRoleAdvisoryLock(t, "postgres", roleName)
	defer release()

	db := openSingleConnLockDB(t, 300)
	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer deferredRollback(txn)

	err = pgLockRole(db, txn, roleName)
	if err == nil {
		t.Fatal("expected pgLockRole to time out while another session holds the lock")
	}
	want := regexp.MustCompile(`timed out after 300ms waiting for advisory lock on role "` + roleName + `"`)
	if !want.MatchString(err.Error()) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAccPostgresqlGrant_advisoryLockTimeout(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	dbName, roleName := getTestDBNames(dbSuffix)

	// The grant transaction runs in dbName, so the lock is held there.
	release := holdRoleAdvisoryLock(t, dbName, roleName)
	defer release()

	grant := fmt.Sprintf(`
resource "postgresql_grant" "test" {
  database    = %q
  role        = %q
  object_type = "database"
  privileges  = ["CONNECT"]
}
`, dbName, roleName)

	// A dedicated provider instance with advisory_lock_timeout set; the
	// acceptance framework generates the provider block itself, so the
	// argument is set through the schema default.
	//
	// Connections are cached per DSN in dbRegistry together with the client
	// (and so the config) that opened them first. A distinct connect_timeout
	// gives this provider its own DSNs, so its 300ms limit neither depends on
	// test order nor leaks into later tests sharing the default DSNs.
	bounded := Provider()
	bounded.Schema["advisory_lock_timeout"].Default = 300
	bounded.Schema["connect_timeout"].DefaultFunc = func() (any, error) { return 179, nil }

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
		},
		Providers: map[string]*schema.Provider{"postgresql": bounded},
		Steps: []resource.TestStep{
			{
				Config:      grant,
				ExpectError: regexp.MustCompile(`timed out after 300ms waiting for advisory lock on role`),
			},
		},
	})

	// Default (0) keeps the legacy unbounded wait; with the lock released the
	// same grant applies normally.
	release()
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: grant,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
				),
			},
		},
	})
}

// holdsAdvisoryLockOnRole reports whether the transaction's backend holds the
// transaction-level advisory lock pgLockRole takes on the role OID.
func holdsAdvisoryLockOnRole(t *testing.T, txn *sql.Tx, role string) bool {
	t.Helper()
	var n int
	if err := txn.QueryRow(`
SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
  AND objsubid = 1 AND objid = (SELECT oid FROM pg_roles WHERE rolname = $1)`, role,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestAccAdvisoryLockTimeoutRestoresTransactionValue(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()
	_, roleName := getTestDBNames(dbSuffix)

	db := openSingleConnLockDB(t, 500)

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer deferredRollback(txn)

	// A value set earlier in the same transaction differs from the session's
	// default, so restoring "to DEFAULT" would not bring it back.
	if _, err := txn.Exec("SET LOCAL lock_timeout = '7s'"); err != nil {
		t.Fatal(err)
	}
	if err := pgLockRole(db, txn, roleName); err != nil {
		t.Fatalf("pgLockRole on an uncontended role failed: %v", err)
	}

	var lockTimeout string
	if err := txn.QueryRow("SELECT current_setting('lock_timeout')").Scan(&lockTimeout); err != nil {
		t.Fatal(err)
	}
	if lockTimeout != "7s" {
		t.Errorf("lock_timeout after acquiring the lock = %q, want the transaction's previous value 7s", lockTimeout)
	}
	if !holdsAdvisoryLockOnRole(t, txn, roleName) {
		t.Errorf("advisory lock on role %s is not held after pgLockRole", roleName)
	}
}

func TestAccAdvisoryLockQuotedRoleName(t *testing.T) {
	skipIfNotAcc(t)

	admin := openSingleConnLockDB(t, 0)
	role := `tf_tests_lock_o'q\b`
	member := `tf_tests_lock_member`
	for _, r := range []string{role, member} {
		if _, err := admin.Exec("CREATE ROLE " + pq.QuoteIdentifier(r)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, r := range []string{member, role} {
			_, _ = admin.Exec("DROP ROLE IF EXISTS " + pq.QuoteIdentifier(r))
		}
	})
	if _, err := admin.Exec("GRANT " + pq.QuoteIdentifier(role) + " TO " + pq.QuoteIdentifier(member)); err != nil {
		t.Fatal(err)
	}

	for _, timeoutMs := range []int{0, 500} {
		db := openSingleConnLockDB(t, timeoutMs)
		txn, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := pgLockRole(db, txn, role); err != nil {
			t.Fatalf("timeout %d: pgLockRole(%q) failed: %v", timeoutMs, role, err)
		}
		if !holdsAdvisoryLockOnRole(t, txn, role) {
			t.Errorf("timeout %d: advisory lock on role %q is not held", timeoutMs, role)
		}
		if !holdsAdvisoryLockOnRole(t, txn, member) {
			t.Errorf("timeout %d: advisory lock on member %q is not held", timeoutMs, member)
		}
		deferredRollback(txn)
	}
}
