package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blang/semver"
)

func TestConfigConnParams(t *testing.T) {
	var tests = []struct {
		input *Config
		want  []string
	}{
		{&Config{Scheme: "postgres", SSLMode: "require", ConnectTimeoutSec: 10}, []string{"connect_timeout=10", "sslmode=require"}},
		{&Config{Scheme: "postgres", SSLMode: "disable"}, []string{"connect_timeout=0", "sslmode=disable"}},
		{&Config{Scheme: "awspostgres", ConnectTimeoutSec: 10}, []string{}},
		{&Config{Scheme: "awspostgres", SSLMode: "disable"}, []string{}},
		{&Config{ExpectedVersion: semver.MustParse("9.0.0"), ApplicationName: "Terraform provider"}, []string{"fallback_application_name=Terraform+provider"}},
		{&Config{ExpectedVersion: semver.MustParse("8.0.0"), ApplicationName: "Terraform provider"}, []string{}},
		{&Config{SSLClientCert: &ClientCertificateConfig{CertificatePath: "/path/to/public-certificate.pem", KeyPath: "/path/to/private-key.pem"}}, []string{"sslcert=%2Fpath%2Fto%2Fpublic-certificate.pem", "sslkey=%2Fpath%2Fto%2Fprivate-key.pem"}},
		{&Config{SSLRootCertPath: "/path/to/root.pem"}, []string{"sslrootcert=%2Fpath%2Fto%2Froot.pem"}},
		{&Config{Scheme: "postgres", SSLMode: "disable", BinaryParameters: true}, []string{"binary_parameters=yes", "connect_timeout=0", "sslmode=disable"}},
		{&Config{Scheme: "postgres", SSLMode: "disable", BinaryParameters: false}, []string{"connect_timeout=0", "sslmode=disable"}},
		{&Config{Scheme: "awspostgres", BinaryParameters: true}, []string{"binary_parameters=yes"}},
		{&Config{Scheme: "gcppostgres", BinaryParameters: true}, []string{"binary_parameters=yes"}},
	}

	for _, test := range tests {

		connParams := test.input.connParams()

		sort.Strings(connParams)
		sort.Strings(test.want)

		if !reflect.DeepEqual(connParams, test.want) {
			t.Errorf("Config.connParams(%+v) returned %#v, want %#v", test.input, connParams, test.want)
		}

	}
}

func TestConfigConnStr(t *testing.T) {
	var tests = []struct {
		input        *Config
		wantDbURL    string
		wantDbParams []string
	}{
		{&Config{Scheme: "postgres", Host: "localhost", Port: 5432, Username: "postgres_user", Password: "postgres_password", SSLMode: "disable"}, "postgres://postgres_user:postgres_password@localhost:5432/postgres", []string{"connect_timeout=0", "sslmode=disable"}},
		{&Config{Scheme: "postgres", Host: "localhost", Port: 5432, Username: "spaced user", Password: "spaced password", SSLMode: "disable"}, "postgres://spaced%20user:spaced%20password@localhost:5432/postgres", []string{"connect_timeout=0", "sslmode=disable"}},
	}

	for _, test := range tests {

		connStr := test.input.connStr("postgres")

		splitConnStr := strings.Split(connStr, "?")

		if splitConnStr[0] != test.wantDbURL {
			t.Errorf("Config.connStr(%+v) returned %#v, want %#v", test.input, splitConnStr[0], test.wantDbURL)
		}

		connParams := strings.Split(splitConnStr[1], "&")

		sort.Strings(connParams)
		sort.Strings(test.wantDbParams)

		if !reflect.DeepEqual(connParams, test.wantDbParams) {
			t.Errorf("Config.connStr(%+v) returned %#v, want %#v", test.input, connParams, test.wantDbParams)
		}

	}
}

// flakyPingDriver is a fake database/sql/driver.Driver whose connections fail
// to Ping a configurable number of times before succeeding, used to exercise
// Client.connectWithRetry without a real PostgreSQL server.
type flakyPingDriver struct {
	failuresBeforeSuccess int32
	attempts              atomic.Int32
}

func (d *flakyPingDriver) Open(name string) (driver.Conn, error) {
	return &flakyPingConn{driver: d}, nil
}

type flakyPingConn struct {
	driver *flakyPingDriver
}

func (c *flakyPingConn) Prepare(query string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *flakyPingConn) Close() error                              { return nil }
func (c *flakyPingConn) Begin() (driver.Tx, error)                 { return nil, driver.ErrSkip }

func (c *flakyPingConn) Ping(ctx context.Context) error {
	attempt := c.driver.attempts.Add(1)
	if attempt <= c.driver.failuresBeforeSuccess {
		return errors.New("connection refused")
	}
	return nil
}

func newFlakyClient(driverName string, drv *flakyPingDriver, maxConnRetries, timeoutSeconds int) *Client {
	sql.Register(driverName, drv)
	return &Client{
		config: Config{
			Scheme:                        "postgres",
			MaxConnRetries:                maxConnRetries,
			ConnectionRetryTimeoutSeconds: timeoutSeconds,
		},
	}
}

func TestConnectWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	drv := &flakyPingDriver{failuresBeforeSuccess: 2}
	c := newFlakyClient("flaky-retry-success", drv, 5, 5)

	db, err := c.connectWithRetry(context.Background(), openDriver("flaky-retry-success"), "dsn")
	if err != nil {
		t.Fatalf("connectWithRetry() returned unexpected error: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := drv.attempts.Load(); got != 3 {
		t.Errorf("expected 3 connection attempts (2 failures + 1 success), got %d", got)
	}
}

func TestConnectWithRetryGivesUpAfterMaxConnRetries(t *testing.T) {
	drv := &flakyPingDriver{failuresBeforeSuccess: 100}
	c := newFlakyClient("flaky-retry-exhausted", drv, 3, 30)

	_, err := c.connectWithRetry(context.Background(), openDriver("flaky-retry-exhausted"), "dsn")
	if err == nil {
		t.Fatal("connectWithRetry() expected an error, got nil")
	}

	if got := drv.attempts.Load(); got != 4 {
		t.Errorf("expected exactly 4 connection attempts (1 initial + MaxConnRetries), got %d", got)
	}
}

func TestConfigConnStrIsDeterministic(t *testing.T) {
	// dbRegistry is keyed by the DSN, so connStr must return the same string
	// for the same database on every call (connParams is built from a map).
	c := &Config{
		Scheme:            "postgres",
		Host:              "localhost",
		Port:              5432,
		Username:          "user",
		Password:          "pass",
		SSLMode:           "disable",
		ConnectTimeoutSec: 10,
		ApplicationName:   "Terraform provider",
		ExpectedVersion:   semver.MustParse("16.0.0"),
		SSLRootCertPath:   "/path/to/root.pem",
	}

	want := c.connStr("mydb")
	for i := 0; i < 50; i++ {
		if got := c.connStr("mydb"); got != want {
			t.Fatalf("Config.connStr() is not stable: got %q, then %q", want, got)
		}
	}
}

func TestConfigConfigurePoolIdleConnections(t *testing.T) {
	drv := &flakyPingDriver{}
	sql.Register("pool-idle-test", drv)

	for _, tc := range []struct {
		maxIdle  int
		wantIdle int
	}{
		{maxIdle: 0, wantIdle: 0},
		{maxIdle: 2, wantIdle: 1},
	} {
		db, err := sql.Open("pool-idle-test", "dsn")
		if err != nil {
			t.Fatalf("sql.Open() returned unexpected error: %v", err)
		}
		c := &Config{MaxConns: 20, MaxIdleConns: tc.maxIdle}
		c.configurePool(db)

		if err := db.Ping(); err != nil {
			t.Fatalf("Ping() returned unexpected error: %v", err)
		}
		if got := db.Stats().Idle; got != tc.wantIdle {
			t.Errorf("max_idle_connections=%d: expected %d idle connection(s) after a query, got %d", tc.maxIdle, tc.wantIdle, got)
		}
		_ = db.Close()
	}
}

func TestConfigEvictDBConnection(t *testing.T) {
	drv := &flakyPingDriver{}
	sql.Register("pool-evict-test", drv)

	newEntry := func(c *Config, database string) (string, *sql.DB) {
		db, err := sql.Open("pool-evict-test", "dsn")
		if err != nil {
			t.Fatalf("sql.Open() returned unexpected error: %v", err)
		}
		dsn := c.registryKey(database)
		dbRegistryLock.Lock()
		dbRegistry[dsn] = &DBConnection{DB: db, client: c.NewClient(database), pool: &dbPool{config: *c}}
		dbRegistryLock.Unlock()
		t.Cleanup(func() {
			dbRegistryLock.Lock()
			delete(dbRegistry, dsn)
			dbRegistryLock.Unlock()
			_ = db.Close()
		})
		return dsn, db
	}
	inRegistry := func(dsn string) bool {
		dbRegistryLock.Lock()
		defer dbRegistryLock.Unlock()
		_, found := dbRegistry[dsn]
		return found
	}

	pooled := &Config{Scheme: "postgres", Host: "evict-test", Port: 5432, SSLMode: "disable", MaxIdleConns: 2}

	dsn, db := newEntry(pooled, "dropped_db")
	if !pooled.evictDBConnection("dropped_db") {
		t.Fatal("evictDBConnection() returned false for a pooled database in the registry")
	}
	if inRegistry(dsn) {
		t.Error("evictDBConnection() left the entry in dbRegistry")
	}
	if err := db.Ping(); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Errorf("expected the evicted pool to be closed, Ping() returned %v", err)
	}

	if pooled.evictDBConnection("dropped_db") {
		t.Error("evictDBConnection() returned true for a database that is no longer in the registry")
	}
	if pooled.evictDBConnection("never_connected") {
		t.Error("evictDBConnection() returned true for a database that was never connected")
	}

	// Pools created with max_idle_connections = 0 keep no idle connections,
	// so they are left alone (default behavior unchanged).
	unpooled := &Config{Scheme: "postgres", Host: "evict-test-unpooled", Port: 5432, SSLMode: "disable"}
	dsn, _ = newEntry(unpooled, "dropped_db")
	if unpooled.evictDBConnection("dropped_db") {
		t.Error("evictDBConnection() returned true for a pool without idle connections")
	}
	if !inRegistry(dsn) {
		t.Error("evictDBConnection() removed a pool without idle connections")
	}
}

// openDriver opens pools with the registered database/sql driver name.
func openDriver(name string) func(string) (*sql.DB, error) {
	return func(dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }
}

// gatedDialDriver is a fake driver that counts physical dials and blocks every
// Ping until release is closed, so concurrent Connect() calls overlap.
type gatedDialDriver struct {
	dials   atomic.Int32
	release chan struct{}
}

func (d *gatedDialDriver) Open(name string) (driver.Conn, error) {
	d.dials.Add(1)
	return &gatedDialConn{driver: d}, nil
}

type gatedDialConn struct {
	driver *gatedDialDriver
}

func (c *gatedDialConn) Prepare(query string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *gatedDialConn) Close() error                              { return nil }
func (c *gatedDialConn) Begin() (driver.Tx, error)                 { return nil, driver.ErrSkip }

func (c *gatedDialConn) Ping(ctx context.Context) error {
	<-c.driver.release
	return nil
}

func TestConnectConcurrentCallsDialOnce(t *testing.T) {
	drv := &gatedDialDriver{release: make(chan struct{})}
	sql.Register("connect-singleflight-test", drv)

	config := Config{
		Scheme:                        "postgres",
		Host:                          "singleflight-test",
		Port:                          5432,
		SSLMode:                       "disable",
		MaxConns:                      20,
		MaxIdleConns:                  10,
		ConnectionRetryTimeoutSeconds: 5,
		// Not the default version, so Connect() skips the fingerprint query
		// the fake driver can't answer.
		ExpectedVersion: semver.MustParse("16.0.0"),
	}
	key := config.registryKey("db")
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		conn := dbRegistry[key]
		delete(dbRegistry, key)
		dbRegistryLock.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})

	const callers = 10
	conns := make([]*DBConnection, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conns[i], errs[i] = config.NewClient("db").connect(context.Background(), openDriver("connect-singleflight-test"))
		}()
	}

	// Give every caller time to reach the dial before the first Ping returns.
	deadline := time.Now().Add(500 * time.Millisecond)
	for drv.dials.Load() < callers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(drv.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("connect() #%d returned unexpected error: %v", i, err)
		}
		// Every caller gets its own view of the pool (forClient), but all
		// views share the one *sql.DB the single flight opened.
		if conns[i].DB != conns[0].DB {
			t.Errorf("connect() #%d returned a different pool than #0", i)
		}
	}
	if got := drv.dials.Load(); got != 1 {
		t.Errorf("expected %d concurrent connect() calls for one DSN to dial once, got %d dials", callers, got)
	}
}

func TestConnectDoesNotCacheErrors(t *testing.T) {
	drv := &flakyPingDriver{failuresBeforeSuccess: 1}
	sql.Register("connect-no-error-cache-test", drv)

	config := Config{
		Scheme:                        "postgres",
		Host:                          "no-error-cache-test",
		Port:                          5432,
		SSLMode:                       "disable",
		MaxConns:                      20,
		ConnectionRetryTimeoutSeconds: 5,
		ExpectedVersion:               semver.MustParse("16.0.0"),
	}
	key := config.registryKey("db")
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		conn := dbRegistry[key]
		delete(dbRegistry, key)
		dbRegistryLock.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})

	if _, err := config.NewClient("db").connect(context.Background(), openDriver("connect-no-error-cache-test")); err == nil {
		t.Fatal("first connect() expected an error, got nil")
	}
	if _, err := config.NewClient("db").connect(context.Background(), openDriver("connect-no-error-cache-test")); err != nil {
		t.Fatalf("second connect() returned unexpected error, the first error was cached: %v", err)
	}
	if got := drv.attempts.Load(); got != 2 {
		t.Errorf("expected 2 connection attempts, got %d", got)
	}
}

func TestConfigRegistryKeySeparatesAuthKinds(t *testing.T) {
	tokens := func(name string) *authTokenSource {
		return newAuthTokenSource(name, func(ctx context.Context) (string, time.Time, error) {
			return "token", time.Now().Add(time.Hour), nil
		})
	}
	base := Config{Scheme: "postgres", Host: "db", Port: 5432, Username: "app", SSLMode: "require"}

	passwordless := base
	rdsIAM := base
	rdsIAM.AuthTokens = tokens("AWS RDS IAM")
	azure := base
	azure.AuthTokens = tokens("Azure AD")

	// Precondition: the DSNs are identical, only the auth kind differs.
	if passwordless.connStr("postgres") != rdsIAM.connStr("postgres") {
		t.Fatal("expected identical DSNs for passwordless and token auth")
	}

	keys := map[string]string{}
	for name, c := range map[string]Config{"passwordless": passwordless, "rds": rdsIAM, "azure": azure} {
		key := c.registryKey("postgres")
		if other, dup := keys[key]; dup {
			t.Errorf("%s and %s share the registry key %q", name, other, key)
		}
		keys[key] = name
	}

	// The impersonated service account changes the pool's identity.
	gcp := Config{Scheme: "gcppostgres", Host: "project/region/instance", Port: 5432, Username: "app"}
	gcpImpersonated := gcp
	gcpImpersonated.GCPIAMImpersonateServiceAccount = "sa@project.iam.gserviceaccount.com"
	gcpOther := gcp
	gcpOther.GCPIAMImpersonateServiceAccount = "other@project.iam.gserviceaccount.com"
	gcpKeys := map[string]bool{}
	for _, c := range []Config{gcp, gcpImpersonated, gcpOther} {
		gcpKeys[c.registryKey("postgres")] = true
	}
	if len(gcpKeys) != 3 {
		t.Errorf("gcppostgres configs differing only in the impersonated service account share registry keys: %v", gcpKeys)
	}

	// Two Clients of the same provider (same token source) share a pool.
	rdsIAM2 := rdsIAM
	if rdsIAM.registryKey("postgres") != rdsIAM2.registryKey("postgres") {
		t.Error("copies of one provider Config must map to the same registry key")
	}
	if rdsIAM.registryKey("postgres") == rdsIAM.registryKey("other") {
		t.Error("different databases must map to different registry keys")
	}
}

func TestClientConnectSeparatesAuthKinds(t *testing.T) {
	drv := &flakyPingDriver{}
	sql.Register("registry-auth-kind", drv)

	base := Config{Scheme: "postgres", Host: "auth-kind-test", Port: 5432, Username: "app", SSLMode: "disable", ExpectedVersion: semver.MustParse("16.0.0")}
	tokenAuth := base
	tokenAuth.AuthTokens = newAuthTokenSource("test", func(ctx context.Context) (string, time.Time, error) {
		return "token", time.Now().Add(time.Hour), nil
	})

	for _, c := range []*Config{&base, &tokenAuth} {
		key := c.registryKey("postgres")
		db, err := sql.Open("registry-auth-kind", "dsn")
		if err != nil {
			t.Fatal(err)
		}
		dbRegistryLock.Lock()
		dbRegistry[key] = &DBConnection{DB: db, client: c.NewClient("postgres"), pool: &dbPool{config: *c}}
		dbRegistryLock.Unlock()
		t.Cleanup(func() {
			dbRegistryLock.Lock()
			delete(dbRegistry, key)
			dbRegistryLock.Unlock()
			_ = db.Close()
		})
	}

	plain, err := base.NewClient("postgres").Connect()
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokenAuth.NewClient("postgres").Connect()
	if err != nil {
		t.Fatal(err)
	}
	if plain.DB == token.DB {
		t.Error("a passwordless and a token-auth provider got the same pool")
	}
	if token.pool.config.AuthTokens == nil {
		t.Error("the token-auth provider got the passwordless pool")
	}
}
