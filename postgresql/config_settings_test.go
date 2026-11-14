package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"sync/atomic"
	"testing"

	"github.com/blang/semver"
)

// versionDriver is a database/sql driver whose connections answer every query
// with a single "PostgreSQL <version> ..." row, like SELECT VERSION().
type versionDriver struct {
	version string
	queries atomic.Int32
}

func (d *versionDriver) Open(string) (driver.Conn, error) { return &versionConn{driver: d}, nil }

type versionConn struct{ driver *versionDriver }

func (c *versionConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *versionConn) Close() error                        { return nil }
func (c *versionConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *versionConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.driver.queries.Add(1)
	return &versionRows{value: "PostgreSQL " + c.driver.version + " on aarch64-unknown-linux-gnu, 64-bit"}, nil
}

type versionRows struct {
	value string
	done  bool
}

func (r *versionRows) Columns() []string { return []string{"version"} }
func (r *versionRows) Close() error      { return nil }
func (r *versionRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

// registerTestPool opens a pool on drv and publishes it in dbRegistry as if
// opener had connected to database first. fingerprinted is the server version
// detected when the pool was opened, nil if opener had expected_version set.
func registerTestPool(t *testing.T, driverName string, drv driver.Driver, opener *Config, database string, fingerprinted *semver.Version) *sql.DB {
	t.Helper()

	sql.Register(driverName, drv)
	db, err := sql.Open(driverName, "dsn")
	if err != nil {
		t.Fatalf("sql.Open() returned unexpected error: %v", err)
	}
	opener.configurePool(db)

	version := opener.ExpectedVersion
	if fingerprinted != nil {
		version = *fingerprinted
	}
	pool := &dbPool{config: *opener}
	pool.serverVersion.Store(fingerprinted)
	conn := &DBConnection{
		DB:      db,
		client:  opener.NewClient(database),
		version: version,
		pool:    pool,
	}

	dsn := opener.connStr(database)
	dbRegistryLock.Lock()
	dbRegistry[dsn] = conn
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, dsn)
		dbRegistryLock.Unlock()
		_ = db.Close()
	})
	return db
}

// sameDSNConfigs returns two provider configurations that produce the same
// connection string but differ in settings that are not part of it.
func sameDSNConfigs(host string) (first, second *Config) {
	base := Config{
		Scheme:          "postgres",
		Host:            host,
		Port:            5432,
		Username:        "admin",
		Password:        "secret",
		SSLMode:         "disable",
		ApplicationName: "Terraform provider",
		MaxConns:        20,
		ExpectedVersion: semver.MustParse(defaultExpectedPostgreSQLVersion),
	}
	a, b := base, base
	a.Superuser = true
	b.Superuser = false
	b.DatabaseUsername = "admin@server"
	return &a, &b
}

// Settings that are read per call (superuser, database_username, ...) must
// come from the configuration of the caller, not from whichever configuration
// opened the shared pool for the DSN first.
func TestConnectReturnsCallerSettingsOnSharedPool(t *testing.T) {
	first, second := sameDSNConfigs("settings-test")
	if first.connStr("postgres") != second.connStr("postgres") {
		t.Fatal("test setup: configurations must share the DSN")
	}

	pg15 := semver.MustParse("15.4.0")
	db := registerTestPool(t, "settings-test", &versionDriver{version: "15.4"}, first, "postgres", &pg15)

	for _, tc := range []struct {
		name   string
		config *Config
	}{
		{"first", first},
		{"second", second},
	} {
		conn, err := tc.config.NewClient("postgres").Connect()
		if err != nil {
			t.Fatalf("%s: Connect() returned unexpected error: %v", tc.name, err)
		}
		if conn.DB != db {
			t.Errorf("%s: Connect() opened a new pool, expected the shared one", tc.name)
		}
		if got, want := conn.client.config.Superuser, tc.config.Superuser; got != want {
			t.Errorf("%s: superuser = %t, want %t", tc.name, got, want)
		}
		if got, want := conn.client.config.getDatabaseUsername(), tc.config.getDatabaseUsername(); got != want {
			t.Errorf("%s: database username = %q, want %q", tc.name, got, want)
		}
		if !conn.version.Equals(pg15) {
			t.Errorf("%s: version = %s, want the fingerprinted %s", tc.name, conn.version, pg15)
		}
	}
}

// A connection obtained through a caller's view must lead to the same caller's
// settings when it opens a connection to another database (initConnection).
func TestConnectCallerSettingsFollowToOtherDatabases(t *testing.T) {
	first, second := sameDSNConfigs("settings-follow-test")
	pg15 := semver.MustParse("15.4.0")
	registerTestPool(t, "settings-follow-test-postgres", &versionDriver{version: "15.4"}, first, "postgres", &pg15)
	registerTestPool(t, "settings-follow-test-app", &versionDriver{version: "15.4"}, first, "app", &pg15)

	conn, err := second.NewClient("postgres").Connect()
	if err != nil {
		t.Fatalf("Connect() returned unexpected error: %v", err)
	}
	appConn, err := initConnection(conn.client, "app")
	if err != nil {
		t.Fatalf("initConnection() returned unexpected error: %v", err)
	}
	if appConn.client.config.Superuser != second.Superuser || appConn.client.databaseName != "app" {
		t.Errorf("initConnection() returned superuser=%t database=%q, want superuser=%t database=%q",
			appConn.client.config.Superuser, appConn.client.databaseName, second.Superuser, "app")
	}
}

// expected_version is a per-configuration setting as well: a caller that set
// it sees its own version, a caller that did not sees the server's version,
// fingerprinted once per pool even if the pool was opened by a caller that
// skipped fingerprinting.
func TestConnectVersionPerCaller(t *testing.T) {
	pinned, unpinned := sameDSNConfigs("version-test")
	pinned.ExpectedVersion = semver.MustParse("12.0.0")

	drv := &versionDriver{version: "16.2"}
	registerTestPool(t, "version-test", drv, pinned, "postgres", nil)

	conn, err := pinned.NewClient("postgres").Connect()
	if err != nil {
		t.Fatalf("pinned: Connect() returned unexpected error: %v", err)
	}
	if want := semver.MustParse("12.0.0"); !conn.version.Equals(want) {
		t.Errorf("pinned: version = %s, want %s", conn.version, want)
	}

	for range 2 {
		conn, err = unpinned.NewClient("postgres").Connect()
		if err != nil {
			t.Fatalf("unpinned: Connect() returned unexpected error: %v", err)
		}
		if want := semver.MustParse("16.2.0"); !conn.version.Equals(want) {
			t.Errorf("unpinned: version = %s, want the fingerprinted %s", conn.version, want)
		}
	}
	if got := drv.queries.Load(); got != 1 {
		t.Errorf("expected the server version to be fingerprinted once, got %d queries", got)
	}
}

// Pool settings are applied when the pool is opened, so they come from the
// configuration that opened it; eviction before DROP/RENAME DATABASE must
// follow the pool's max_idle_connections, not the caller's.
func TestEvictDBConnectionUsesPoolSettings(t *testing.T) {
	opener, caller := sameDSNConfigs("evict-pool-settings-test")
	opener.MaxIdleConns = 2
	caller.MaxIdleConns = 0

	pg15 := semver.MustParse("15.4.0")
	registerTestPool(t, "evict-pool-settings-test", &versionDriver{version: "15.4"}, opener, "app", &pg15)

	conn, err := caller.NewClient("app").Connect()
	if err != nil {
		t.Fatalf("Connect() returned unexpected error: %v", err)
	}
	if !conn.client.config.evictDBConnection("app") {
		t.Error("evictDBConnection() kept a pool that holds idle connections because the caller has max_idle_connections = 0")
	}
}
