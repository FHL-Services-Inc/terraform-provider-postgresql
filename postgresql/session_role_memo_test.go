package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
)

// sessionRoleDriver is a fake database/sql driver that answers the catalog
// queries issued around withRolesGranted and readRolePassword, and records
// every statement, so tests can count how often the session user, its
// rolsuper attribute and the set_option column are looked up.
type sessionRoleDriver struct {
	mu         sync.Mutex
	statements []string
	// superuser is the rolsuper attribute of the login role "admin".
	superuser bool
}

func (d *sessionRoleDriver) Open(string) (driver.Conn, error) {
	return &sessionRoleConn{driver: d}, nil
}

func (d *sessionRoleDriver) record(query string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statements = append(d.statements, query)
}

// count returns how many recorded statements contain substr.
func (d *sessionRoleDriver) count(substr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.statements {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func (d *sessionRoleDriver) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.statements...)
}

type sessionRoleConn struct {
	driver *sessionRoleDriver
}

func (c *sessionRoleConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *sessionRoleConn) Close() error                        { return nil }
func (c *sessionRoleConn) Begin() (driver.Tx, error)           { return sessionRoleTx{}, nil }

func (c *sessionRoleConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.driver.record(query)
	return driver.RowsAffected(0), nil
}

func (c *sessionRoleConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.driver.record(query)
	c.driver.mu.Lock()
	superuser := c.driver.superuser
	c.driver.mu.Unlock()

	switch {
	case query == "SELECT CURRENT_USER":
		return &sessionRoleRows{cols: []string{"current_user"}, rows: [][]driver.Value{{"admin"}}}, nil
	case strings.Contains(query, "rolname = CURRENT_USER"):
		return &sessionRoleRows{cols: []string{"rolsuper"}, rows: [][]driver.Value{{superuser}}}, nil
	case strings.Contains(query, "rolname = ANY($1)"):
		// Answer for every role named in the array argument: "admin" is the
		// login role, any other role is a plain role.
		names := strings.Split(strings.Trim(args[0].Value.(string), "{}"), ",")
		rows := make([][]driver.Value, 0, len(names))
		for _, name := range names {
			name = strings.Trim(name, `"`)
			rows = append(rows, []driver.Value{name, name == "admin" && superuser})
		}
		return &sessionRoleRows{cols: []string{"rolname", "rolsuper"}, rows: rows}, nil
	case strings.Contains(query, "information_schema.columns"):
		return &sessionRoleRows{cols: []string{"c"}, rows: [][]driver.Value{{int64(1)}}}, nil
	}
	// Membership checks and anything else: no rows.
	return &sessionRoleRows{cols: []string{"c"}}, nil
}

type sessionRoleTx struct{}

func (sessionRoleTx) Commit() error   { return nil }
func (sessionRoleTx) Rollback() error { return nil }

type sessionRoleRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *sessionRoleRows) Columns() []string { return r.cols }
func (r *sessionRoleRows) Close() error      { return nil }
func (r *sessionRoleRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// newSessionRoleConnection returns a DBConnection backed by a fresh
// sessionRoleDriver, fingerprinted as the given server version.
func newSessionRoleConnection(t *testing.T, version string, superuser bool) (*DBConnection, *sessionRoleDriver) {
	t.Helper()
	drv := &sessionRoleDriver{superuser: superuser}
	sqlDB := sql.OpenDB(sessionRoleConnector{drv})
	sqlDB.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = sqlDB.Close() })

	return &DBConnection{DB: sqlDB, version: semver.MustParse(version)}, drv
}

// sessionRoleConnector binds a sql.DB to one sessionRoleDriver instance.
type sessionRoleConnector struct{ drv *sessionRoleDriver }

func (c sessionRoleConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open("") }
func (c sessionRoleConnector) Driver() driver.Driver                        { return c.drv }

// runWithRolesGranted runs one withRolesGranted call in its own transaction,
// like a grant / default privileges / schema operation does.
func runWithRolesGranted(t *testing.T, db *DBConnection, roles []string) {
	t.Helper()
	txn, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer deferredRollback(txn)

	called := false
	if err := withRolesGranted(db, txn, roles, func() error { called = true; return nil }); err != nil {
		t.Fatalf("withRolesGranted: %v", err)
	}
	if !called {
		t.Fatal("withRolesGranted did not call fn")
	}
}

func TestSessionRoleMemoizedSuperuser(t *testing.T) {
	db, drv := newSessionRoleConnection(t, "16.0.0", true)

	runWithRolesGranted(t, db, []string{"owner"})
	runWithRolesGranted(t, db, []string{"owner", "other"})

	superuser, err := db.isSuperuser()
	if err != nil {
		t.Fatalf("isSuperuser: %v", err)
	}
	if !superuser {
		t.Fatal("isSuperuser() = false, want true")
	}

	if got := drv.count("CURRENT_USER"); got != 1 {
		t.Errorf("CURRENT_USER looked up %d times, want 1; statements: %q", got, drv.all())
	}
	if got := drv.count("rolsuper"); got != 1 {
		t.Errorf("rolsuper looked up %d times, want 1; statements: %q", got, drv.all())
	}
}

func TestSessionRoleMemoizedNonSuperuser(t *testing.T) {
	db, drv := newSessionRoleConnection(t, "16.0.0", false)

	runWithRolesGranted(t, db, []string{"owner"})
	runWithRolesGranted(t, db, []string{"owner"})

	superuser, err := db.isSuperuser()
	if err != nil {
		t.Fatalf("isSuperuser: %v", err)
	}
	if superuser {
		t.Fatal("isSuperuser() = true, want false")
	}

	// The login role and its rolsuper are read once (with the first batch);
	// each call still needs the rolsuper of the roles it grants.
	if got := drv.count("SELECT CURRENT_USER"); got != 1 {
		t.Errorf("SELECT CURRENT_USER issued %d times, want 1; statements: %q", got, drv.all())
	}
	if got := drv.count("rolname = CURRENT_USER"); got != 0 {
		t.Errorf("rolsuper of CURRENT_USER queried %d times, want 0 (known from the batch); statements: %q", got, drv.all())
	}
	if got := drv.count("rolname = ANY($1)"); got != 2 {
		t.Errorf("batched rolsuper issued %d times, want 2; statements: %q", got, drv.all())
	}
	if got := drv.count("information_schema"); got != 0 {
		t.Errorf("information_schema probed %d times, want 0; statements: %q", got, drv.all())
	}
	// Each call: revoke check + grant check + revoke check after fn.
	if got := drv.count("FROM pg_auth_members"); got != 6 {
		t.Errorf("membership checks = %d, want 6; statements: %q", got, drv.all())
	}
}

func TestFeatureAuthMembersSetOption(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"12.0.0", false},
		{"15.8.0", false},
		{"16.0.0", true},
		{"17.2.0", true},
	} {
		db := &DBConnection{version: semver.MustParse(tc.version)}
		if got := db.featureSupported(featureAuthMembersSetOption); got != tc.want {
			t.Errorf("featureSupported(featureAuthMembersSetOption) on %s = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestIsMemberOfRoleSetOptionGate(t *testing.T) {
	for _, tc := range []struct {
		version       string
		wantSetOption bool
	}{
		{"15.0.0", false},
		{"16.0.0", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			db, drv := newSessionRoleConnection(t, tc.version, false)

			if _, err := isMemberOfRole(db, db.featureSupported(featureAuthMembersSetOption), "owner", "admin"); err != nil {
				t.Fatalf("isMemberOfRole: %v", err)
			}

			if got := drv.count("information_schema"); got != 0 {
				t.Errorf("information_schema probed %d times, want 0", got)
			}
			if got := drv.count("AND set_option"); (got == 1) != tc.wantSetOption {
				t.Errorf("membership query filters on set_option = %v, want %v; statements: %q", got == 1, tc.wantSetOption, drv.all())
			}
		})
	}
}

// Concurrent operations on one pool share the memo; run with -race.
func TestSessionRoleMemoConcurrent(t *testing.T) {
	db, drv := newSessionRoleConnection(t, "16.0.0", false)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := db.isSuperuser(); err != nil {
				t.Errorf("isSuperuser: %v", err)
			}
			txn, err := db.Begin()
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			defer deferredRollback(txn)
			if err := withRolesGranted(db, txn, []string{"owner"}, func() error { return nil }); err != nil {
				t.Errorf("withRolesGranted: %v", err)
			}
		})
	}
	wg.Wait()

	if superuser, known := db.cachedSuperuser(); !known || superuser {
		t.Errorf("cachedSuperuser() = %v, %v; want false, true", superuser, known)
	}
	if got := drv.count("SELECT CURRENT_USER"); got < 1 || got > 8 {
		t.Errorf("SELECT CURRENT_USER issued %d times, want 1..8", got)
	}
}
