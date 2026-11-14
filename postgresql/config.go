package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/lib/pq"
	"gocloud.dev/gcp"
	"gocloud.dev/gcp/cloudsql"
	"gocloud.dev/postgres"
	_ "gocloud.dev/postgres/awspostgres"
	"gocloud.dev/postgres/gcppostgres"
	"golang.org/x/sync/singleflight"
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

	// connectGroup de-duplicates concurrent Connect() calls per DSN.
	connectGroup singleflight.Group

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
// racing with another one just stores the same value twice.
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
	AdvisoryLockTimeout             int
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

	paramsArray := []string{}
	for key, value := range params {
		paramsArray = append(paramsArray, fmt.Sprintf("%s=%s", key, url.QueryEscape(value)))
	}
	// Map iteration order is random; sort so the DSN (the dbRegistry key) is
	// stable and a database always maps to a single registry entry.
	sort.Strings(paramsArray)

	return paramsArray
}

func (c *Config) connStr(database string) string {
	host := c.Host
	// For GCP, support both project/region/instance and project:region:instance
	// (The second one allows to use the output of google_sql_database_instance as host
	if c.Scheme == "gcppostgres" {
		host = strings.ReplaceAll(host, ":", "/")
	}

	connStr := fmt.Sprintf(
		"%s://%s:%s@%s:%d/%s?%s",
		c.Scheme,
		url.PathEscape(c.Username),
		url.PathEscape(c.Password),
		host,
		c.Port,
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
	return c.connect(context.Background(), c.config.openPostgres)
}

// connect implements Connect. openPostgres opens the pool for the "postgres"
// scheme (overridable by tests).
func (c *Client) connect(ctx context.Context, openPostgres func(dsn string) (*sql.DB, error)) (*DBConnection, error) {
	dsn := c.config.connStr(c.databaseName)

	if conn, found := lookupDBConnection(dsn); found {
		return conn, nil
	}

	// Concurrent first calls for the same DSN share one dial + ping +
	// fingerprint: without this every caller that misses the registry opens
	// its own handle and all but one close it again, which with -parallelism=N
	// costs N-1 extra connections per pool and a startup spike of N
	// connections that no pool setting caps. Errors are not cached: the next
	// call after a failed one dials again.
	v, err, _ := connectGroup.Do(dsn, func() (any, error) {
		// A previous flight may have published the connection between the
		// lookup above and this call.
		if conn, found := lookupDBConnection(dsn); found {
			return conn, nil
		}

		// Dialing and retrying happen outside dbRegistryLock so a slow/flaky
		// connection to one DSN doesn't stall Connect() calls for other DSNs.
		conn, err := c.newDBConnection(ctx, openPostgres, dsn)
		if err != nil {
			return nil, err
		}

		dbRegistryLock.Lock()
		defer dbRegistryLock.Unlock()
		if existing, found := dbRegistry[dsn]; found {
			// Defensive: only flights publish, and there is one flight per DSN.
			_ = conn.Close()
			return existing, nil
		}
		dbRegistry[dsn] = conn
		return conn, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*DBConnection), nil
}

func lookupDBConnection(dsn string) (*DBConnection, bool) {
	dbRegistryLock.Lock()
	defer dbRegistryLock.Unlock()
	conn, found := dbRegistry[dsn]
	return conn, found
}

// newDBConnection opens and checks a new pool for dsn and fingerprints the
// server version if no expected_version is set.
func (c *Client) newDBConnection(ctx context.Context, openPostgres func(dsn string) (*sql.DB, error), dsn string) (*DBConnection, error) {
	db, err := c.connectWithRetry(ctx, openPostgres, dsn)
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

	return &DBConnection{
		DB:      db,
		client:  c,
		version: *version,
	}, nil
}

// configurePool applies the provider's pool settings to a freshly opened
// database handle.
func (c *Config) configurePool(db *sql.DB) {
	// By default (max_idle_connections = 0) no connection is retained: when we
	// connect to a specific database which might be managed by terraform, we
	// don't keep connections open in case the database has to be dropped in
	// the plan. With max_idle_connections > 0 connections are reused, and
	// postgresql_database evicts the pool of a database before dropping it
	// (see evictDBConnection).
	db.SetMaxIdleConns(c.MaxIdleConns)
	if c.MaxIdleConns > 0 {
		db.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	}
	db.SetMaxOpenConns(c.effectiveMaxConns())
	db.SetConnMaxLifetime(time.Duration(c.ConnMaxLifetimeSeconds) * time.Second)
}

// openPostgres opens a pool for the "postgres" scheme. Connections are dialed
// through proxyDriver (honoring ALL_PROXY / HTTPS_PROXY), and count against
// the endpoint's connection budget when max_total_connections is set.
//
// The gocloud schemes (awspostgres, gcppostgres) open their pools themselves
// and don't expose their connector, so the budget does not apply to them.
func (c *Config) openPostgres(dsn string) (*sql.DB, error) {
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	connector.Dialer(proxyDriver{})
	if c.MaxTotalConns <= 0 {
		return sql.OpenDB(connector), nil
	}
	budget := getEndpointBudget(c.endpointKey(), c.MaxTotalConns)
	return openBudgetedDB(connector, budget, c.effectiveMaxConns(), c.MaxIdleConns, c.budgetWait()), nil
}

// budgetWait is how long a dial waits for a free slot of the endpoint budget:
// connect_timeout, or 3 minutes (its default) when connect_timeout is 0.
func (c *Config) budgetWait() time.Duration {
	if c.ConnectTimeoutSec > 0 {
		return time.Duration(c.ConnectTimeoutSec) * time.Second
	}
	return defaultProviderConnectTimeoutSeconds * time.Second
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

// connectWithRetry opens a connection to dsn, retrying transient failures up
// to c.config.MaxConnRetries times or until c.config.ConnectionRetryTimeoutSeconds
// elapses, whichever comes first. openPostgres opens the pool for the
// "postgres" scheme (overridable by tests).
func (c *Client) connectWithRetry(ctx context.Context, openPostgres func(dsn string) (*sql.DB, error), dsn string) (*sql.DB, error) {
	var db *sql.DB
	var err error
	retryCount := 0

	connectRetryTimeout := time.Duration(c.config.ConnectionRetryTimeoutSeconds) * time.Second
	retryError := retry.RetryContext(ctx, connectRetryTimeout, func() *retry.RetryError {
		if c.config.Scheme == "postgres" {
			db, err = openPostgres(dsn)
		} else if c.config.Scheme == "gcppostgres" && c.config.GCPIAMImpersonateServiceAccount != "" {
			db, err = openImpersonatedGCPDBConnection(ctx, dsn, c.config.GCPIAMImpersonateServiceAccount)
		} else {
			db, err = postgres.Open(ctx, dsn)
		}
		if err == nil {
			err = db.PingContext(ctx)
		}

		if err == nil {
			return nil
		}

		if db != nil {
			_ = db.Close()
			db = nil
		}

		if !isRetryableConnectionError(err) || retryCount >= c.config.MaxConnRetries {
			return retry.NonRetryableError(err)
		}
		retryCount++
		return retry.RetryableError(err)
	})
	if retryError != nil {
		// Preserve *pq.Error so callers can detect conditions such as a dropped
		// database (SQLSTATE 3D000); server-side pq errors never contain the
		// password. Other errors may echo it, so keep redacting those.
		if _, ok := errors.AsType[*pq.Error](retryError); ok {
			return nil, fmt.Errorf("error connecting to PostgreSQL server %s (scheme: %s): %w", c.config.Host, c.config.Scheme, retryError)
		}
		errString := strings.Replace(retryError.Error(), c.config.Password, "XXXX", 2)
		return nil, fmt.Errorf("error connecting to PostgreSQL server %s (scheme: %s): %s", c.config.Host, c.config.Scheme, errString)
	}

	return db, nil
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
func fingerprintCapabilities(db *sql.DB) (*semver.Version, error) {
	var pgVersion string
	err := db.QueryRow(`SELECT VERSION()`).Scan(&pgVersion)
	if err != nil {
		return nil, fmt.Errorf("error PostgreSQL version: %w", err)
	}

	// PostgreSQL 9.2.21 on x86_64-apple-darwin16.5.0, compiled by Apple LLVM version 8.1.0 (clang-802.0.42), 64-bit
	// PostgreSQL 9.6.7, compiled by Visual C++ build 1800, 64-bit
	fields := strings.FieldsFunc(pgVersion, func(c rune) bool {
		return unicode.IsSpace(c) || c == ','
	})
	if len(fields) < 2 {
		return nil, fmt.Errorf("error determining the server version: %q", pgVersion)
	}

	version, err := semver.ParseTolerant(fields[1])
	if err != nil {
		return nil, fmt.Errorf("error parsing version: %w", err)
	}

	return &version, nil
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
