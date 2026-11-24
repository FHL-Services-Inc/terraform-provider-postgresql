package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/lib/pq"
	"gocloud.dev/gcp"
	"gocloud.dev/gcp/cloudsql"
	"gocloud.dev/postgres"
	"gocloud.dev/postgres/gcppostgres"
	"google.golang.org/api/impersonate"
)

type featureName uint

const (
	featureCreateRoleWith featureName = iota
	featureDatabaseOwnerRole
	featureDBAllowConnections
	featureDBIsTemplate
	featureFallbackApplicationName
	featureRLS
	featureSchemaCreateIfNotExist
	featureReplication
	featureExtension
	featurePrivileges
	featureProcedure
	featureRoutine
	featurePrivilegesOnSchemas
	featureForceDropDatabase
	featurePid
	featurePublishViaRoot
	featurePubTruncate
	featurePublication
	featurePubWithoutTruncate
	featureFunction
	featureServer
	featureCreateRoleSelfGrant
	featureSecurityLabel
	featureMaintainPrivilege
	featureAuthMembersSetOption
)

// defaultConnMaxIdleTime closes connections that stayed idle in the pool for
// that long. Only used when max_idle_connections > 0.
//
// The provider keeps one pool per database it connects to, so the idle
// connections it holds add up across pools. Busy pools reuse their connections
// within milliseconds, so a short timeout costs little reuse, while it bounds
// how long a pool that is no longer used (e.g. for a database whose resources
// were all refreshed early in the run) keeps its sessions open on the server,
// where they count against max_connections and block an out-of-band
// DROP DATABASE.
const defaultConnMaxIdleTime = 30 * time.Second

var (
	dbRegistryLock sync.Mutex
	dbRegistry     map[string]*DBConnection = make(map[string]*DBConnection, 1)

	// Mapping of feature flags to versions
	featureSupported = map[featureName]semver.Range{
		// CREATE ROLE WITH
		featureCreateRoleWith: semver.MustParseRange(">=8.1.0"),

		// CREATE DATABASE has ALLOW_CONNECTIONS support
		featureDBAllowConnections: semver.MustParseRange(">=9.5.0"),

		// CREATE DATABASE has IS_TEMPLATE support
		featureDBIsTemplate: semver.MustParseRange(">=9.5.0"),

		// https://www.postgresql.org/docs/9.0/static/libpq-connect.html
		featureFallbackApplicationName: semver.MustParseRange(">=9.0.0"),

		// CREATE SCHEMA IF NOT EXISTS
		featureSchemaCreateIfNotExist: semver.MustParseRange(">=9.3.0"),

		// row-level security
		featureRLS: semver.MustParseRange(">=9.5.0"),

		// CREATE ROLE has REPLICATION support.
		featureReplication: semver.MustParseRange(">=9.1.0"),

		// CREATE EXTENSION support.
		featureExtension: semver.MustParseRange(">=9.1.0"),

		// We do not support postgresql_grant and postgresql_default_privileges
		// for Postgresql < 9.
		featurePrivileges: semver.MustParseRange(">=9.0.0"),

		// Object PROCEDURE support
		featureProcedure: semver.MustParseRange(">=11.0.0"),

		// Object ROUTINE support
		featureRoutine: semver.MustParseRange(">=11.0.0"),
		// ALTER DEFAULT PRIVILEGES has ON SCHEMAS support
		// for Postgresql >= 10
		featurePrivilegesOnSchemas: semver.MustParseRange(">=10.0.0"),

		// DROP DATABASE WITH FORCE
		// for Postgresql >= 13
		featureForceDropDatabase: semver.MustParseRange(">=13.0.0"),

		// Column procpid was replaced by pid in pg_stat_activity
		// for Postgresql >= 9.2 and above
		featurePid: semver.MustParseRange(">=9.2.0"),

		// attribute publish_via_partition_root for partition is supported
		featurePublishViaRoot: semver.MustParseRange(">=13.0.0"),

		// attribute pubtruncate for publications is supported
		featurePubTruncate: semver.MustParseRange(">=11.0.0"),

		// attribute pubtruncate for publications is supported
		featurePubWithoutTruncate: semver.MustParseRange("<11.0.0"),

		// publication is Supported
		featurePublication: semver.MustParseRange(">=10.0.0"),

		// We do not support CREATE FUNCTION for Postgresql < 8.4
		featureFunction: semver.MustParseRange(">=8.4.0"),
		// CREATE SERVER support
		featureServer: semver.MustParseRange(">=10.0.0"),

		featureDatabaseOwnerRole: semver.MustParseRange(">=15.0.0"),

		// New privileges rules in version 16
		// https://www.postgresql.org/docs/16/release-16.html#RELEASE-16-PRIVILEGES
		featureCreateRoleSelfGrant: semver.MustParseRange(">=16.0.0"),
		featureSecurityLabel:       semver.MustParseRange(">=11.0.0"),

		// MAINTAIN privilege on tables (PG 17+)
		featureMaintainPrivilege: semver.MustParseRange(">=17.0.0"),

		// pg_auth_members has a set_option column (PG 16+)
		featureAuthMembersSetOption: semver.MustParseRange(">=16.0.0"),
	}
)

type DBConnection struct {
	*sql.DB

	client *Client

	// version is the version number of the database as determined by parsing the
	// output of `SELECT VERSION()`.x
	version semver.Version

	// sessionRole memoizes the login role of this pool and its rolsuper
	// attribute; see sessionRoleMemo.
	sessionRole sessionRoleMemo
}

// sessionRoleMemo caches CURRENT_USER and its rolsuper attribute for one
// DBConnection. A DBConnection is keyed by DSN, so its login role is fixed
// for its whole lifetime; only successful lookups are stored, and a lookup
// racing with another one just stores the same value twice. The memo lives
// and dies with its pool: a pool evicted from dbRegistry (evictDBConnection)
// is replaced by a new DBConnection, which starts with an empty memo.
type sessionRoleMemo struct {
	mu             sync.Mutex
	user           string
	userKnown      bool
	superuser      bool
	superuserKnown bool
}

// currentUser returns the session user, querying it on q the first time.
// q should be the transaction the caller already holds, so the first lookup
// does not need a second connection from the pool.
func (db *DBConnection) currentUser(q QueryAble) (string, error) {
	db.sessionRole.mu.Lock()
	user, known := db.sessionRole.user, db.sessionRole.userKnown
	db.sessionRole.mu.Unlock()
	if known {
		return user, nil
	}

	user, err := getCurrentUser(q)
	if err != nil {
		return "", err
	}

	db.sessionRole.mu.Lock()
	db.sessionRole.user, db.sessionRole.userKnown = user, true
	db.sessionRole.mu.Unlock()
	return user, nil
}

// cachedSuperuser returns the memoized rolsuper of the session user, if known.
func (db *DBConnection) cachedSuperuser() (superuser, known bool) {
	db.sessionRole.mu.Lock()
	defer db.sessionRole.mu.Unlock()
	return db.sessionRole.superuser, db.sessionRole.superuserKnown
}

func (db *DBConnection) storeSuperuser(superuser bool) {
	db.sessionRole.mu.Lock()
	defer db.sessionRole.mu.Unlock()
	db.sessionRole.superuser, db.sessionRole.superuserKnown = superuser, true
}

// featureSupported returns true if a given feature is supported or not. This is
// slightly different from Config's featureSupported in that here we're
// evaluating against the fingerprinted version, not the expected version.
func (db *DBConnection) featureSupported(name featureName) bool {
	fn, found := featureSupported[name]
	if !found {
		// panic'ing because this is a provider-only bug
		panic(fmt.Sprintf("unknown feature flag %v", name))
	}

	return fn(db.version)
}

// isSuperuser returns true if connected user is a Postgres SUPERUSER
func (db *DBConnection) isSuperuser() (bool, error) {
	if superuser, known := db.cachedSuperuser(); known {
		return superuser, nil
	}

	var superuser bool

	if err := db.QueryRow("SELECT rolsuper FROM pg_roles WHERE rolname = CURRENT_USER").Scan(&superuser); err != nil {
		return false, fmt.Errorf("could not check if current user is superuser: %w", err)
	}

	db.storeSuperuser(superuser)
	return superuser, nil
}

type ClientCertificateConfig struct {
	CertificatePath string
	KeyPath         string
	SSLInline       bool
}

// Config - provider config
type Config struct {
	Scheme                          string
	Host                            string
	Port                            int
	Username                        string
	Password                        string
	DatabaseUsername                string
	Superuser                       bool
	SSLMode                         string
	ApplicationName                 string
	Timeout                         int
	ConnectTimeoutSec               int
	MaxConnRetries                  int
	ConnectionRetryTimeoutSeconds   int
	ConnMaxLifetimeSeconds          int
	MaxConns                        int
	MaxIdleConns                    int
	MaxTotalConns                   int
	ExpectedVersion                 semver.Version
	SSLClientCert                   *ClientCertificateConfig
	SSLRootCertPath                 string
	GCPIAMImpersonateServiceAccount string
	BinaryParameters                bool

	// AuthTokens, when set, supplies the password of every new connection
	// for the "postgres" and "awspostgres" schemes (AWS RDS IAM and Azure AD
	// tokens, see newPostgresConnector), and
	// Password is left empty, so the DSN and the dbRegistry key carry no
	// token. It is a pointer: all Clients of a provider share the cache.
	AuthTokens *authTokenSource

	// CatalogSnapshot lets grant and default privileges Reads share one
	// per-database catalog snapshot (see catalog_snapshot.go) instead of each
	// querying the catalogs; CatalogSnapshotTTLSeconds bounds its age.
	CatalogSnapshot           bool
	CatalogSnapshotTTLSeconds int
}

// Client struct holding connection string
type Client struct {
	// Configuration for the client
	config Config

	databaseName string
}

// NewClient returns client config for the specified database.
func (c *Config) NewClient(database string) *Client {
	return &Client{
		config:       *c,
		databaseName: database,
	}
}

// featureSupported returns true if a given feature is supported or not.  This
// is slightly different from Client's featureSupported in that here we're
// evaluating against the expected version, not the fingerprinted version.
func (c *Config) featureSupported(name featureName) bool {
	fn, found := featureSupported[name]
	if !found {
		// panic'ing because this is a provider-only bug
		panic(fmt.Sprintf("unknown feature flag %v", name))
	}

	return fn(c.ExpectedVersion)
}

func (c *Config) connParams() []string {
	params := map[string]string{}

	// sslmode and connect_timeout are not allowed with gocloud
	// (TLS is provided by gocloud directly)
	if c.Scheme == "postgres" {
		params["sslmode"] = c.SSLMode
		params["connect_timeout"] = strconv.Itoa(c.ConnectTimeoutSec)
	}

	if c.featureSupported(featureFallbackApplicationName) {
		params["fallback_application_name"] = c.ApplicationName
	}
	if c.SSLClientCert != nil {
		params["sslcert"] = c.SSLClientCert.CertificatePath
		params["sslkey"] = c.SSLClientCert.KeyPath
		if c.SSLClientCert.SSLInline {
			params["sslinline"] = strconv.FormatBool(c.SSLClientCert.SSLInline)
		}
	}

	if c.SSLRootCertPath != "" {
		params["sslrootcert"] = c.SSLRootCertPath
	}

	// Send Parse/Bind/Describe/Execute/Sync in a single flush for statements
	// with bind parameters instead of a separate prepare round-trip.
	if c.BinaryParameters {
		params["binary_parameters"] = "yes"
	}

	paramsArray := []string{}
	for key, value := range params {
		paramsArray = append(paramsArray, fmt.Sprintf("%s=%s", key, url.QueryEscape(value)))
	}
	// Map iteration order is random; sort so the DSN (the key of dbRegistry
	// and of the catalog snapshots) is stable and a database always maps to a
	// single registry entry and a single snapshot.
	sort.Strings(paramsArray)

	return paramsArray
}

func (c *Config) connStr(database string) string {
	var hostPort string
	if c.Scheme == "gcppostgres" {
		// For GCP, support both project/region/instance and project:region:instance
		// (The second one allows to use the output of google_sql_database_instance as host
		hostPort = fmt.Sprintf("%s:%d", strings.ReplaceAll(c.Host, ":", "/"), c.Port)
	} else {
		// JoinHostPort brackets IPv6 literals; strip brackets the user may
		// already have put around one so they are not doubled.
		host := strings.TrimSuffix(strings.TrimPrefix(c.Host, "["), "]")
		hostPort = net.JoinHostPort(host, strconv.Itoa(c.Port))
	}

	// url.UserPassword escapes ':' in the username too, which PathEscape
	// leaves as is, so a username such as "AROA123:session" is not split.
	connStr := fmt.Sprintf(
		"%s://%s@%s/%s?%s",
		c.Scheme,
		url.UserPassword(c.Username, c.Password).String(),
		hostPort,
		database,
		strings.Join(c.connParams(), "&"),
	)

	return connStr
}

func (c *Config) getDatabaseUsername() string {
	if c.DatabaseUsername != "" {
		return c.DatabaseUsername
	}
	return c.Username
}

// Connect returns a copy to an sql.Open()'ed database connection wrapped in a DBConnection struct.
// Callers must return their database resources. Use of QueryRow() or Exec() is encouraged.
// Query() must have their rows.Close()'ed.
func (c *Client) Connect() (*DBConnection, error) {
	ctx := context.Background()
	dsn := c.config.connStr(c.databaseName)

	dbRegistryLock.Lock()
	conn, found := dbRegistry[dsn]
	dbRegistryLock.Unlock()
	if found {
		return conn, nil
	}

	// Dialing and retrying happen outside the lock so a slow/flaky connection
	// to one DSN doesn't stall Connect() calls for other DSNs.
	db, err := c.connectWithRetry(ctx, proxyDriverName, dsn)
	if err != nil {
		return nil, err
	}

	c.config.configurePool(db)

	defaultVersion, _ := semver.Parse(defaultExpectedPostgreSQLVersion)
	version := &c.config.ExpectedVersion
	if defaultVersion.Equals(c.config.ExpectedVersion) {
		// Version hint not set by user, need to fingerprint
		version, err = fingerprintCapabilities(db)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("error detecting capabilities: %w", err)
		}
	}

	conn = &DBConnection{
		DB:      db,
		client:  c,
		version: *version,
	}

	dbRegistryLock.Lock()
	defer dbRegistryLock.Unlock()
	if existing, found := dbRegistry[dsn]; found {
		// Another goroutine won the race and already published a connection
		// for this DSN; keep that one and discard ours.
		_ = db.Close()
		return existing, nil
	}
	dbRegistry[dsn] = conn
	return conn, nil
}

// configurePool applies the provider's pool settings to a freshly opened
// database handle.
func (c *Config) configurePool(db *sql.DB) {
	// With max_idle_connections > 0 (the default is 5) connections are reused
	// for up to defaultConnMaxIdleTime, and postgresql_database evicts the pool
	// of a database before dropping it (see evictDBConnection). With
	// max_idle_connections = 0 no connection is retained, so nothing is left
	// open to a database that might be dropped later in the plan.
	db.SetMaxIdleConns(c.MaxIdleConns)
	if c.MaxIdleConns > 0 {
		db.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	}
	db.SetMaxOpenConns(c.MaxConns)
	db.SetConnMaxLifetime(time.Duration(c.ConnMaxLifetimeSeconds) * time.Second)
}

// evictDBConnection removes the pooled connection to database from dbRegistry
// and closes it, so no idle connection to a database that is about to be
// dropped stays around, and a database later recreated under the same name
// gets a fresh pool. Pools created with max_idle_connections = 0 retain no
// idle connections and are left untouched.
//
// The entry is looked up by DSN, which only matches because every Client is
// created from the same provider Config (see NewClient) and connStr is
// deterministic. It returns true if a pool was found and evicted.
// It must not be called while holding dbRegistryLock.
func (c *Config) evictDBConnection(database string) bool {
	dsn := c.connStr(database)

	dbRegistryLock.Lock()
	conn, found := dbRegistry[dsn]
	if !found || conn.client.config.MaxIdleConns == 0 {
		dbRegistryLock.Unlock()
		return false
	}
	delete(dbRegistry, dsn)
	dbRegistryLock.Unlock()

	if err := conn.Close(); err != nil {
		log.Printf("[WARN] error closing connection pool to database %q: %v", database, err)
	}
	return true
}

// connectRetryInitialBackoff and connectRetryMaxBackoff bound the wait
// between two connection attempts: it starts at the initial value and doubles
// after each failed attempt, up to the maximum.
var (
	connectRetryInitialBackoff = 500 * time.Millisecond
	connectRetryMaxBackoff     = 10 * time.Second
)

// connectWithRetry opens a connection to dsn, retrying transient failures up
// to c.config.MaxConnRetries times. postgresDriverName is the database/sql
// driver used for the "postgres" scheme (overridable by tests).
//
// c.config.ConnectionRetryTimeoutSeconds limits the retries only: no new
// attempt starts once that much time has passed since the first one. An
// attempt in progress is never cut short; connect_timeout bounds each attempt.
// With MaxConnRetries = 0 there is a single attempt and the retry timeout
// plays no part.
func (c *Client) connectWithRetry(ctx context.Context, postgresDriverName, dsn string) (*sql.DB, error) {
	var err error
	retryDeadline := time.Now().Add(time.Duration(c.config.ConnectionRetryTimeoutSeconds) * time.Second)
	backoff := connectRetryInitialBackoff
	for retryCount := 0; ; retryCount++ {
		var db *sql.DB
		db, err = c.openAndPing(ctx, postgresDriverName, dsn)
		if err == nil {
			return db, nil
		}
		if !isRetryableConnectionError(err) || retryCount >= c.config.MaxConnRetries {
			break
		}
		remaining := time.Until(retryDeadline)
		if remaining <= 0 {
			break
		}
		wait := min(backoff, remaining)
		log.Printf("[DEBUG] connection attempt %d to %s failed, retrying in %s: %v", retryCount+1, c.config.Host, wait, redactTokenError(err, c.config.Password))
		select {
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
		if ctx.Err() != nil {
			break
		}
		backoff = min(backoff*2, connectRetryMaxBackoff)
	}

	// The loop only ends here on a failure. Preserve *pq.Error so callers can
	// detect conditions such as a dropped database (SQLSTATE 3D000);
	// server-side pq errors never contain the password. Other errors may echo
	// it, so keep redacting those.
	if _, ok := errors.AsType[*pq.Error](err); ok {
		return nil, fmt.Errorf("error connecting to PostgreSQL server %s (scheme: %s): %w", c.config.Host, c.config.Scheme, err)
	}
	// The DSN holds the password URL-escaped, and errors such as a URL
	// parse failure echo the DSN, so redact the escaped forms as well.
	// On the auth token path Password is empty and the connector has
	// already redacted the token.
	redacted := redactTokenError(err, c.config.Password)
	return nil, fmt.Errorf("error connecting to PostgreSQL server %s (scheme: %s): %s", c.config.Host, c.config.Scheme, redacted)
}

// openAndPing opens a database handle for dsn and checks it with a ping, which
// establishes the first connection. On failure the handle is closed.
func (c *Client) openAndPing(ctx context.Context, postgresDriverName, dsn string) (*sql.DB, error) {
	var db *sql.DB
	var err error
	if budget := c.config.connBudget(); budget != nil {
		db, err = c.config.openBudgetedPostgresDB(dsn, budget)
	} else if c.config.AuthTokens != nil {
		db, err = c.config.openTokenAuthDB(dsn)
	} else if c.config.Scheme == "postgres" {
		db, err = sql.Open(postgresDriverName, dsn)
	} else if c.config.Scheme == "awspostgres" {
		db, err = c.config.openAWSPostgres(dsn)
	} else if c.config.Scheme == "gcppostgres" && c.config.GCPIAMImpersonateServiceAccount != "" {
		db, err = openImpersonatedGCPDBConnection(ctx, dsn, c.config.GCPIAMImpersonateServiceAccount)
	} else {
		db, err = postgres.Open(ctx, dsn)
	}
	if err == nil {
		err = db.PingContext(ctx)
	}
	if err == nil {
		return db, nil
	}
	if db != nil {
		_ = db.Close()
	}
	return nil, err
}

// isRetryableConnectionError reports whether a connection failure is likely
// transient (network blips, server not ready yet) as opposed to a
// configuration error (bad credentials, unknown database) that will not be
// fixed by retrying.
func isRetryableConnectionError(err error) bool {
	if pqErr, ok := errors.AsType[*pq.Error](err); ok {
		switch pqErr.Code.Class() {
		case "28", // Invalid Authorization Specification
			"3D": // Invalid Catalog Name
			return false
		}
	}
	return true
}

// fingerprintCapabilities queries PostgreSQL to populate a local catalog of
// capabilities.  This is only run once per Client.
//
// It reads server_version_num rather than parsing SELECT VERSION(): the
// version text carries vendor suffixes (e.g. "13.23-rds.20250508" on RDS
// Extended Support) that are not valid semver, while server_version_num is a
// plain integer everywhere, RDS and Aurora included.
func fingerprintCapabilities(db *sql.DB) (*semver.Version, error) {
	var versionNum string
	if err := db.QueryRow(`SHOW server_version_num`).Scan(&versionNum); err != nil {
		return nil, fmt.Errorf("error PostgreSQL version: %w", err)
	}

	version, err := parseServerVersionNum(versionNum)
	if err != nil {
		return nil, err
	}
	return &version, nil
}

// parseServerVersionNum converts a server_version_num value to a version:
// major * 10000 + minor from PostgreSQL 10 on (170006 is 17.6.0), and
// major * 10000 + minor * 100 + patch before (90621 is 9.6.21).
func parseServerVersionNum(s string) (semver.Version, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil || n == 0 {
		return semver.Version{}, fmt.Errorf("error parsing server_version_num %q", s)
	}
	if n >= 100000 {
		return semver.Version{Major: n / 10000, Minor: n % 10000}, nil
	}
	return semver.Version{Major: n / 10000, Minor: (n / 100) % 100, Patch: n % 100}, nil
}

func openImpersonatedGCPDBConnection(ctx context.Context, dsn string, targetServiceAccountEmail string) (*sql.DB, error) {
	ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: targetServiceAccountEmail,
		Scopes:          []string{"https://www.googleapis.com/auth/sqlservice.admin"},
	})
	if err != nil {
		return nil, fmt.Errorf("error creating token source with service account impersonation of %s: %w", targetServiceAccountEmail, err)
	}
	client, err := gcp.NewHTTPClient(gcp.DefaultTransport(), ts)
	if err != nil {
		return nil, fmt.Errorf("error creating HTTP client with service account impersonation of %s: %w", targetServiceAccountEmail, err)
	}
	certSource := cloudsql.NewCertSourceWithIAM(client, ts)
	opener := gcppostgres.URLOpener{CertSource: certSource}
	dbURL, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("error parsing connection string: %w", err)
	}
	return opener.OpenPostgresURL(ctx, dbURL)
}
