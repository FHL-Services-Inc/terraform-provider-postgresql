package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// stmtCountingDriver is a fake database/sql driver that records how many
// transactions and queries a Read issues. Every query returns no rows, so a
// Read under test takes its "not found" path. When openErr is set, opening a
// connection fails with it (e.g. SQLSTATE 3D000 for a dropped database).
type stmtCountingDriver struct {
	mu      sync.Mutex
	begins  int
	queries []string
	openErr error
}

func (d *stmtCountingDriver) reset(openErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.begins = 0
	d.queries = nil
	d.openErr = openErr
}

func (d *stmtCountingDriver) counts() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begins, append([]string(nil), d.queries...)
}

func (d *stmtCountingDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.openErr != nil {
		return nil, d.openErr
	}
	return &stmtCountingConn{driver: d}, nil
}

type stmtCountingConn struct {
	driver *stmtCountingDriver
}

func (c *stmtCountingConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *stmtCountingConn) Close() error                        { return nil }

func (c *stmtCountingConn) Begin() (driver.Tx, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.begins++
	return stmtCountingTx{}, nil
}

func (c *stmtCountingConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.queries = append(c.driver.queries, query)
	return emptyRows{}, nil
}

type stmtCountingTx struct{}

func (stmtCountingTx) Commit() error   { return nil }
func (stmtCountingTx) Rollback() error { return nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"c"} }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

const stmtCountingDriverName = "pg-read-stmt-counter"

var (
	stmtCounter         = &stmtCountingDriver{}
	registerStmtCounter sync.Once
)

// newStmtCountingConnection returns a DBConnection backed by stmtCounter and
// registers it as the connection for database "testdb", so the per-database
// lookup done by Read functions (initConnection/startTransaction) resolves to
// it instead of dialing a real server.
func newStmtCountingConnection(t *testing.T, openErr error) *DBConnection {
	t.Helper()
	registerStmtCounter.Do(func() { sql.Register(stmtCountingDriverName, stmtCounter) })
	stmtCounter.reset(openErr)

	sqlDB, err := sql.Open(stmtCountingDriverName, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	sqlDB.SetMaxIdleConns(0)

	// A non-"postgres" scheme keeps connStr down to a single query parameter:
	// connParams builds them from a map, so with several parameters the DSN
	// (the registry key) is not stable from one call to the next.
	client := &Client{config: Config{Scheme: "awspostgres"}, databaseName: "testdb"}
	conn := &DBConnection{DB: sqlDB, client: client, version: semver.MustParse("16.0.0"), pool: &dbPool{config: client.config}}

	key := client.config.registryKey(client.databaseName)
	dbRegistryLock.Lock()
	dbRegistry[key] = conn
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, key)
		dbRegistryLock.Unlock()
		_ = sqlDB.Close()
	})

	return conn
}

type readStatementsCase struct {
	name    string
	res     *schema.Resource
	read    func(*DBConnection, *schema.ResourceData) error
	id      string
	attrs   map[string]any
	dataSrc bool
}

func readStatementsCases() []readStatementsCase {
	return []readStatementsCase{
		{
			name:  "schema",
			res:   resourcePostgreSQLSchema(),
			read:  resourcePostgreSQLSchemaRead,
			id:    "testdb.s",
			attrs: map[string]any{"name": "s", "database": "testdb"},
		},
		{
			name:  "extension",
			res:   resourcePostgreSQLExtension(),
			read:  resourcePostgreSQLExtensionRead,
			id:    "testdb.e",
			attrs: map[string]any{"name": "e", "database": "testdb"},
		},
		{
			name:  "function",
			res:   resourcePostgreSQLFunction(),
			read:  resourcePostgreSQLFunctionRead,
			id:    "testdb.public.f()",
			attrs: map[string]any{"name": "f", "database": "testdb"},
		},
		{
			name:  "server",
			res:   resourcePostgreSQLServer(),
			read:  resourcePostgreSQLServerRead,
			id:    "srv",
			attrs: map[string]any{serverNameAttr: "srv", serverFDWAttr: "postgres_fdw"},
		},
		{
			name:  "user_mapping",
			res:   resourcePostgreSQLUserMapping(),
			read:  resourcePostgreSQLUserMappingRead,
			id:    "u.srv",
			attrs: map[string]any{userMappingUserNameAttr: "u", userMappingServerNameAttr: "srv"},
		},
		{
			name: "security_label",
			res:  resourcePostgreSQLSecurityLabel(),
			read: resourcePostgreSQLSecurityLabelRead,
			id:   "dummy.role.r",
			attrs: map[string]any{
				securityLabelObjectTypeAttr: "role",
				securityLabelObjectNameAttr: "r",
				securityLabelProviderAttr:   "dummy",
				securityLabelLabelAttr:      "l",
			},
		},
		{
			name:    "data_source_schemas",
			res:     dataSourcePostgreSQLDatabaseSchemas(),
			read:    dataSourcePostgreSQLSchemasRead,
			attrs:   map[string]any{"database": "testdb"},
			dataSrc: true,
		},
		{
			name:    "data_source_sequences",
			res:     dataSourcePostgreSQLDatabaseSequences(),
			read:    dataSourcePostgreSQLSequencesRead,
			attrs:   map[string]any{"database": "testdb"},
			dataSrc: true,
		},
		{
			name:    "data_source_tables",
			res:     dataSourcePostgreSQLDatabaseTables(),
			read:    dataSourcePostgreSQLTablesRead,
			attrs:   map[string]any{"database": "testdb"},
			dataSrc: true,
		},
	}
}

// TestReadIssuesSingleStatementWithoutTransaction pins that these Reads run
// their one SELECT directly instead of wrapping it in BEGIN ... ROLLBACK,
// which cost two extra round-trips per resource on every refresh.
func TestReadIssuesSingleStatementWithoutTransaction(t *testing.T) {
	for _, tc := range readStatementsCases() {
		t.Run(tc.name, func(t *testing.T) {
			conn := newStmtCountingConnection(t, nil)

			d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			d.SetId(tc.id)

			if err := tc.read(conn, d); err != nil {
				t.Fatalf("Read returned error: %v", err)
			}

			begins, queries := stmtCounter.counts()
			if begins != 0 {
				t.Errorf("Read opened %d transaction(s), want 0", begins)
			}
			if len(queries) != 1 {
				t.Errorf("Read issued %d queries, want 1: %q", len(queries), queries)
			}
			if !tc.dataSrc && d.Id() != "" {
				t.Errorf("Read of a missing object kept ID %q, want it cleared", d.Id())
			}
		})
	}
}

// TestReadPrunesWhenDatabaseDropped pins that database-scoped Reads still
// remove the resource from state when connecting to its database fails with
// SQLSTATE 3D000 (database dropped out-of-band), now that the error surfaces
// on the query instead of on BEGIN.
func TestReadPrunesWhenDatabaseDropped(t *testing.T) {
	dropped := &pq.Error{Code: "3D000", Message: `database "testdb" does not exist`}

	for _, tc := range readStatementsCases() {
		switch tc.name {
		case "schema", "extension", "function":
		default:
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			conn := newStmtCountingConnection(t, dropped)

			d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			d.SetId(tc.id)

			if err := tc.read(conn, d); err != nil {
				t.Fatalf("Read returned error, want the resource pruned: %v", err)
			}
			if d.Id() != "" {
				t.Errorf("Read kept ID %q for a dropped database, want it cleared", d.Id())
			}
		})
	}
}
