package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"reflect"
	"sort"
	"strings"
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

// TestConfigConnStrIsStable pins that connStr returns the same string on
// every call: it is the key of dbRegistry and of the catalog snapshots, and
// the connection parameters come from a map.
func TestConfigConnStrIsStable(t *testing.T) {
	config := &Config{
		Scheme: "postgres", Host: "localhost", Port: 5432, Username: "u", Password: "p",
		SSLMode: "require", ConnectTimeoutSec: 10, SSLRootCertPath: "/root.pem",
		ExpectedVersion: semver.MustParse("9.0.0"), ApplicationName: "Terraform provider",
	}
	want := config.connStr("db")
	for i := 0; i < 100; i++ {
		if got := config.connStr("db"); got != want {
			t.Fatalf("connStr differs between calls:\n%s\n%s", want, got)
		}
	}
}

// flakyPingDriver is a fake database/sql/driver.Driver whose connections fail
// to Ping a configurable number of times before succeeding, used to exercise
// Client.connectWithRetry without a real PostgreSQL server.
type flakyPingDriver struct {
	failuresBeforeSuccess int32
	attempts              atomic.Int32
	// pingDelay makes every Ping take this long, like a slow network or a
	// server that is slow to accept connections.
	pingDelay time.Duration
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
	if c.driver.pingDelay > 0 {
		select {
		case <-time.After(c.driver.pingDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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

	db, err := c.connectWithRetry(context.Background(), "flaky-retry-success", "dsn")
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

	_, err := c.connectWithRetry(context.Background(), "flaky-retry-exhausted", "dsn")
	if err == nil {
		t.Fatal("connectWithRetry() expected an error, got nil")
	}

	if got := drv.attempts.Load(); got != 4 {
		t.Errorf("expected exactly 4 connection attempts (1 initial + MaxConnRetries), got %d", got)
	}
}

// A single attempt that takes longer than connection_retry_timeout_seconds is
// not cut short: the retry timeout limits the retries, connect_timeout limits
// an attempt. With the SDK retry helper used before, an attempt was abandoned
// with "timeout while waiting for state to become 'success'" once the retry
// timeout plus the helper's 30 s grace period had passed.
func TestConnectWithRetryDoesNotCutSlowAttempt(t *testing.T) {
	drv := &flakyPingDriver{pingDelay: 1500 * time.Millisecond}
	c := newFlakyClient("flaky-retry-slow-attempt", drv, 0, 1)

	db, err := c.connectWithRetry(context.Background(), "flaky-retry-slow-attempt", "dsn")
	if err != nil {
		t.Fatalf("connectWithRetry() returned unexpected error: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := drv.attempts.Load(); got != 1 {
		t.Errorf("expected 1 connection attempt, got %d", got)
	}
}

// No retry starts once connection_retry_timeout_seconds have passed since the
// first attempt, even when max_conn_retries allows more.
func TestConnectWithRetryStopsRetryingAtRetryTimeout(t *testing.T) {
	drv := &flakyPingDriver{failuresBeforeSuccess: 1000}
	c := newFlakyClient("flaky-retry-deadline", drv, 100, 1)

	start := time.Now()
	_, err := c.connectWithRetry(context.Background(), "flaky-retry-deadline", "dsn")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("connectWithRetry() expected an error, got nil")
	}

	// Attempts at about 0 s, 0.5 s and 1 s (the second wait is cut to the
	// time left), then the retry timeout has passed.
	if got := drv.attempts.Load(); got < 2 || got > 4 {
		t.Errorf("expected 2 to 4 connection attempts within the 1 s retry timeout, got %d", got)
	}
	if elapsed > 3*time.Second {
		t.Errorf("connectWithRetry() kept retrying for %s after a 1 s retry timeout", elapsed)
	}
}

// A failed attempt that itself outlasts the retry timeout is not retried.
func TestConnectWithRetryNoRetryAfterSlowFailedAttempt(t *testing.T) {
	drv := &flakyPingDriver{failuresBeforeSuccess: 1000, pingDelay: 1200 * time.Millisecond}
	c := newFlakyClient("flaky-retry-slow-failure", drv, 5, 1)

	_, err := c.connectWithRetry(context.Background(), "flaky-retry-slow-failure", "dsn")
	if err == nil {
		t.Fatal("connectWithRetry() expected an error, got nil")
	}
	if got := drv.attempts.Load(); got != 1 {
		t.Errorf("expected 1 connection attempt, got %d", got)
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
		dsn := c.connStr(database)
		dbRegistryLock.Lock()
		dbRegistry[dsn] = &DBConnection{DB: db, client: c.NewClient(database)}
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

// TestConfigConnStrUserinfo pins that the username and password survive the
// round trip through the DSN, including characters with a meaning in URL
// userinfo such as ':' in an AWS-style role session name (#129).
func TestConfigConnStrUserinfo(t *testing.T) {
	tests := []struct {
		username string
		password string
	}{
		{"postgres_user", "postgres_password"},
		{"AROA123:me", "p387"},
		{"user@example.com", "pa:ss@w/rd?#% x"},
		{"spaced user", ""},
	}

	for _, test := range tests {
		c := &Config{Scheme: "postgres", Host: "localhost", Port: 5432, Username: test.username, Password: test.password, SSLMode: "disable"}
		dsn := c.connStr("postgres")
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("connStr() for user %q returned an unparsable DSN: %v", test.username, err)
		}
		if got := u.User.Username(); got != test.username {
			t.Errorf("username: got %q, want %q", got, test.username)
		}
		if got, _ := u.User.Password(); got != test.password {
			t.Errorf("password for user %q: got %q, want %q", test.username, got, test.password)
		}
		if u.Host != "localhost:5432" {
			t.Errorf("host for user %q: got %q, want %q", test.username, u.Host, "localhost:5432")
		}
	}
}

// TestConfigConnStrHost pins that IPv6 literals are bracketed in the DSN
// (#130), with or without brackets in the provider host, while host names,
// IPv4 addresses and GCP instance names keep their form.
func TestConfigConnStrHost(t *testing.T) {
	tests := []struct {
		scheme string
		host   string
		want   string
	}{
		{"postgres", "localhost", "localhost:5432"},
		{"postgres", "127.0.0.1", "127.0.0.1:5432"},
		{"postgres", "::1", "[::1]:5432"},
		{"postgres", "[::1]", "[::1]:5432"},
		{"postgres", "2001:db8::10", "[2001:db8::10]:5432"},
		{"awspostgres", "fd00::5", "[fd00::5]:5432"},
		{"gcppostgres", "project:region:instance", "project/region/instance:5432"},
		{"gcppostgres", "project/region/instance", "project/region/instance:5432"},
	}

	for _, test := range tests {
		c := &Config{Scheme: test.scheme, Host: test.host, Port: 5432, Username: "u", Password: "p"}
		dsn := c.connStr("postgres")
		prefix := test.scheme + "://u:p@"
		if !strings.HasPrefix(dsn, prefix) {
			t.Fatalf("connStr() = %q, want prefix %q", dsn, prefix)
		}
		hostPort := strings.TrimPrefix(dsn, prefix)
		hostPort = hostPort[:strings.LastIndex(strings.SplitN(hostPort, "?", 2)[0], "/postgres")]
		if hostPort != test.want {
			t.Errorf("%s host %q: got %q, want %q", test.scheme, test.host, hostPort, test.want)
		}
		if test.scheme != "gcppostgres" {
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("connStr() for host %q returned an unparsable DSN: %v", test.host, err)
			}
			if u.Port() != "5432" {
				t.Errorf("port for host %q: got %q, want 5432", test.host, u.Port())
			}
		}
	}
}

// TestConnectWithRetryRedactsEscapedPassword pins that a connection error
// echoing the DSN does not leak the password in any of its escaped forms
// (#135). The invalid host makes the driver fail to parse the DSN, and the
// parse error quotes the whole URL.
func TestConnectWithRetryRedactsEscapedPassword(t *testing.T) {
	for _, password := range []string{"s3cr3t/pa ss%x", "a:b@c?d#e+f&g=h;i,j", "plain"} {
		c := &Client{config: Config{
			Scheme:                        "postgres",
			Host:                          "bad host",
			Port:                          5432,
			Username:                      "u428",
			Password:                      password,
			SSLMode:                       "disable",
			ConnectionRetryTimeoutSeconds: 5,
		}}
		_, err := c.connectWithRetry(context.Background(), "postgres", c.config.connStr("postgres"))
		if err == nil {
			t.Fatalf("connectWithRetry() with an invalid host expected an error, got nil")
		}
		msg := err.Error()
		for _, form := range []string{
			password,
			url.PathEscape(password),
			url.QueryEscape(password),
			strings.TrimPrefix(url.UserPassword("", password).String(), ":"),
		} {
			if strings.Contains(msg, form) {
				t.Errorf("connection error leaks password form %q: %s", form, msg)
			}
		}
		if !strings.Contains(msg, "XXXX") {
			t.Errorf("connection error has no redaction marker, the test does not exercise the DSN echo: %s", msg)
		}
	}
}

// TestParseServerVersionNum pins the conversion of server_version_num to a
// semver version. The fingerprint no longer parses SELECT VERSION(), whose
// text carries vendor suffixes such as "13.23-rds.20250508" on RDS Extended
// Support that semver rejects (#140); server_version_num is a plain integer
// on PostgreSQL, RDS, Aurora and the other managed services alike.
func TestParseServerVersionNum(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		// PostgreSQL 10+: major * 10000 + minor.
		{"170006", "17.6.0", false},
		{"180000", "18.0.0", false},
		{"160011", "16.11.0", false},
		{"100023", "10.23.0", false},
		// RDS Extended Support "PostgreSQL 13.23-rds.20250508 on aarch64-unknown-linux-gnu, ...".
		{"130023", "13.23.0", false},
		// RDS Extended Support "PostgreSQL 12.22-rds.20250220 on x86_64-pc-linux-gnu, ...".
		{"120022", "12.22.0", false},
		// Aurora PostgreSQL 17.11: "PostgreSQL 17.11 on aarch64-linux, compiled by gcc-10.5.0, 64-bit".
		{"170011", "17.11.0", false},
		// Aurora PostgreSQL 16.6.
		{"160006", "16.6.0", false},
		// Before 10: major * 10000 + minor * 100 + patch.
		{"90621", "9.6.21", false},
		{"90224", "9.2.24", false},
		{" 170006\n", "17.6.0", false},
		{"", "", true},
		{"17.6", "", true},
		{"13.23-rds.20250508", "", true},
		{"-170006", "", true},
		{"0", "", true},
	}

	for _, test := range tests {
		got, err := parseServerVersionNum(test.input)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseServerVersionNum(%q) = %v, want an error", test.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseServerVersionNum(%q) returned unexpected error: %v", test.input, err)
			continue
		}
		if got.String() != test.want {
			t.Errorf("parseServerVersionNum(%q) = %v, want %v", test.input, got, test.want)
		}
	}
}
