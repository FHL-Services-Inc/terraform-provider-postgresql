package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// grantReadDriver is a fake database/sql driver recording the transactions
// and statements issued by the grant / default privileges Reads. Every query
// returns the single configured row; when openErr is set, opening a connection
// fails with it instead (e.g. SQLSTATE 3D000 for a dropped database).
type grantReadDriver struct {
	mu         sync.Mutex
	begins     int
	statements []string
	row        []driver.Value
	openErr    error
}

func (d *grantReadDriver) reset(row []driver.Value, openErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.begins = 0
	d.statements = nil
	d.row = row
	d.openErr = openErr
}

func (d *grantReadDriver) counts() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begins, append([]string(nil), d.statements...)
}

func (d *grantReadDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.openErr != nil {
		return nil, d.openErr
	}
	return &grantReadConn{driver: d}, nil
}

type grantReadConn struct {
	driver *grantReadDriver
}

func (c *grantReadConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *grantReadConn) Close() error                        { return nil }

func (c *grantReadConn) Begin() (driver.Tx, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.begins++
	return grantReadTx{}, nil
}

func (c *grantReadConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.statements = append(c.driver.statements, query)
	return driver.RowsAffected(0), nil
}

func (c *grantReadConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.statements = append(c.driver.statements, query)
	return &grantReadRows{row: c.driver.row}, nil
}

type grantReadTx struct{}

func (grantReadTx) Commit() error   { return nil }
func (grantReadTx) Rollback() error { return nil }

type grantReadRows struct {
	row  []driver.Value
	done bool
}

func (r *grantReadRows) Columns() []string {
	cols := make([]string, len(r.row))
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	return cols
}

func (r *grantReadRows) Close() error { return nil }

func (r *grantReadRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.row)
	return nil
}

const grantReadDriverName = "pg-grant-read-counter"

var (
	grantReadCounter         = &grantReadDriver{}
	registerGrantReadCounter sync.Once
)

// newGrantReadConnection returns a DBConnection backed by grantReadCounter and
// registers it as the pool for database "testdb", so the per-database lookup
// done by the Reads resolves to it instead of dialing a real server.
func newGrantReadConnection(t *testing.T, row []driver.Value, openErr error) *DBConnection {
	t.Helper()
	registerGrantReadCounter.Do(func() { sql.Register(grantReadDriverName, grantReadCounter) })
	grantReadCounter.reset(row, openErr)

	sqlDB, err := sql.Open(grantReadDriverName, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	sqlDB.SetMaxIdleConns(0)

	// A non-"postgres" scheme keeps connStr down to a single query parameter,
	// so the DSN used as registry key is stable from one call to the next.
	client := &Client{config: Config{Scheme: "awspostgres"}, databaseName: "testdb"}
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

type grantReadCase struct {
	name string
	res  *schema.Resource
	read func(*DBConnection, *schema.ResourceData) error
	// attrs of the resource; "database" is always "testdb".
	attrs map[string]any
	// extra holds the columns returned after the role_exists/schema_exists
	// flags when the role and schema exist: the object name (multi-row
	// types) and the privileges array, matching the configured privileges.
	extra []driver.Value
	// hasSchema reports whether the Read checks that the schema exists.
	hasSchema bool
}

func grantReadCases() []grantReadCase {
	grant := func(name string, attrs map[string]any, hasSchema bool, extra ...driver.Value) grantReadCase {
		attrs["database"] = "testdb"
		if _, ok := attrs["role"]; !ok {
			attrs["role"] = "r"
		}
		return grantReadCase{
			name: "grant_" + name, res: resourcePostgreSQLGrant(), read: resourcePostgreSQLGrantRead,
			attrs: attrs, extra: extra, hasSchema: hasSchema,
		}
	}
	defaultPrivileges := func(name string, attrs map[string]any, hasSchema bool) grantReadCase {
		attrs["database"] = "testdb"
		attrs["role"] = "r"
		attrs["owner"] = "o"
		attrs["privileges"] = []any{"SELECT"}
		return grantReadCase{
			name: "default_privileges_" + name, res: resourcePostgreSQLDefaultPrivileges(), read: resourcePostgreSQLDefaultPrivilegesRead,
			attrs: attrs, extra: []driver.Value{[]byte("{SELECT}")}, hasSchema: hasSchema,
		}
	}
	priv := func(p string) []driver.Value { return []driver.Value{[]byte("{" + p + "}")} }

	return []grantReadCase{
		grant("table", map[string]any{"schema": "s", "object_type": "table", "privileges": []any{"SELECT"}}, true, append([]driver.Value{"t"}, priv("SELECT")...)...),
		grant("table_objects", map[string]any{"schema": "s", "object_type": "table", "objects": []any{"t"}, "privileges": []any{"SELECT"}}, true, append([]driver.Value{"t"}, priv("SELECT")...)...),
		grant("sequence", map[string]any{"schema": "s", "object_type": "sequence", "privileges": []any{"USAGE"}}, true, append([]driver.Value{"q"}, priv("USAGE")...)...),
		grant("function", map[string]any{"schema": "s", "object_type": "function", "objects": []any{"f"}, "privileges": []any{"EXECUTE"}}, true, append([]driver.Value{"f"}, priv("EXECUTE")...)...),
		grant("procedure", map[string]any{"schema": "s", "object_type": "procedure", "privileges": []any{"EXECUTE"}}, true, append([]driver.Value{"p"}, priv("EXECUTE")...)...),
		grant("routine", map[string]any{"schema": "s", "object_type": "routine", "privileges": []any{"EXECUTE"}}, true, append([]driver.Value{"f"}, priv("EXECUTE")...)...),
		grant("column", map[string]any{"schema": "s", "object_type": "column", "objects": []any{"t"}, "columns": []any{"c"}, "privileges": []any{"UPDATE"}}, true, append([]driver.Value{"t", "c"}, priv("UPDATE")...)...),
		grant("schema", map[string]any{"schema": "s", "object_type": "schema", "privileges": []any{"USAGE"}}, true, priv("USAGE")...),
		grant("schema_public", map[string]any{"role": "public", "schema": "s", "object_type": "schema", "privileges": []any{"USAGE"}}, true, priv("USAGE")...),
		grant("database", map[string]any{"object_type": "database", "privileges": []any{"CONNECT"}}, false, priv("CONNECT")...),
		grant("foreign_data_wrapper", map[string]any{"object_type": "foreign_data_wrapper", "objects": []any{"w"}, "privileges": []any{"USAGE"}}, false, priv("USAGE")...),
		grant("foreign_server", map[string]any{"object_type": "foreign_server", "objects": []any{"srv"}, "privileges": []any{"USAGE"}}, false, priv("USAGE")...),
		defaultPrivileges("schema", map[string]any{"schema": "s", "object_type": "table"}, true),
		defaultPrivileges("noschema", map[string]any{"object_type": "sequence"}, false),
	}
}

func (tc grantReadCase) row(roleExists, schemaExists bool) []driver.Value {
	row := []driver.Value{roleExists, schemaExists}
	if roleExists && schemaExists {
		return append(row, tc.extra...)
	}
	// A missing role or schema yields no object: NULL name / privileges.
	for range tc.extra {
		row = append(row, nil)
	}
	return row
}

func (tc grantReadCase) runRead(t *testing.T, row []driver.Value, openErr error) *schema.ResourceData {
	t.Helper()
	conn := newGrantReadConnection(t, row, openErr)
	d := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
	d.SetId("some-id")
	if err := tc.read(conn, d); err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	return d
}

// TestGrantReadIssuesOneStatement pins that postgresql_grant and
// postgresql_default_privileges Reads cost a single statement on the target
// database: no BEGIN/ROLLBACK, no separate database/role/schema existence
// checks, no role OID lookup and no advisory lock. They used to cost 7-12
// statements on three connections, and the default privileges Read held an
// exclusive advisory lock on the owner while reading.
func TestGrantReadIssuesOneStatement(t *testing.T) {
	for _, tc := range grantReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.runRead(t, tc.row(true, true), nil)

			begins, statements := grantReadCounter.counts()
			if begins != 0 {
				t.Errorf("Read opened %d transaction(s), want 0", begins)
			}
			if len(statements) != 1 {
				t.Fatalf("Read issued %d statements, want 1: %q", len(statements), statements)
			}
			if strings.Contains(statements[0], "pg_advisory") {
				t.Errorf("Read must not take advisory locks: %q", statements[0])
			}
			if d.Id() == "" {
				t.Errorf("Read of an existing grant cleared its ID")
			}
		})
	}
}

// TestGrantReadPrunesMissingRole pins that a grantee dropped out-of-band
// removes the resource from state instead of being read as "no privileges".
func TestGrantReadPrunesMissingRole(t *testing.T) {
	for _, tc := range grantReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.runRead(t, tc.row(false, true), nil)
			if d.Id() != "" {
				t.Errorf("Read with a missing role kept ID %q, want it cleared", d.Id())
			}
		})
	}
}

// TestGrantReadPrunesMissingSchema pins that a schema dropped out-of-band
// removes the resource from state for every schema-scoped object type.
func TestGrantReadPrunesMissingSchema(t *testing.T) {
	for _, tc := range grantReadCases() {
		if !tc.hasSchema {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			d := tc.runRead(t, tc.row(true, false), nil)
			if d.Id() != "" {
				t.Errorf("Read with a missing schema kept ID %q, want it cleared", d.Id())
			}
		})
	}
}

// TestGrantReadPrunesDroppedDatabase pins that SQLSTATE 3D000 on the target
// database (dropped out-of-band) prunes the resource instead of failing the
// refresh, now that the database is no longer checked from the maintenance
// database first.
func TestGrantReadPrunesDroppedDatabase(t *testing.T) {
	dropped := &pq.Error{Code: "3D000", Message: `database "testdb" does not exist`}
	for _, tc := range grantReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.runRead(t, nil, dropped)
			if d.Id() != "" {
				t.Errorf("Read of a grant in a dropped database kept ID %q, want it cleared", d.Id())
			}
		})
	}
}

// grantReadDisappearsConfig manages one resource of each high-volume kind
// against the test database, role and schema, so dropping one of them
// out-of-band shows which resources a refresh prunes.
func grantReadDisappearsConfig(dbName, roleName, owner string) string {
	return fmt.Sprintf(`
resource "postgresql_grant" "table" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "table"
	privileges  = ["SELECT"]
}

resource "postgresql_grant" "public_table" {
	database    = "%[1]s"
	role        = "public"
	schema      = "test_schema"
	object_type = "table"
	privileges  = ["INSERT"]

	# Both table grants rewrite the same relacl rows; concurrent GRANT/REVOKE
	# on them fails with "tuple concurrently updated".
	depends_on = [postgresql_grant.table]
}

resource "postgresql_grant" "schema" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "schema"
	privileges  = ["USAGE"]
}

resource "postgresql_grant" "function" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["test_read_fn"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_grant" "database" {
	database    = "%[1]s"
	role        = "%[2]s"
	object_type = "database"
	privileges  = ["CONNECT"]
}

resource "postgresql_default_privileges" "schema" {
	database    = "%[1]s"
	role        = "%[2]s"
	owner       = "%[3]s"
	schema      = "test_schema"
	object_type = "table"
	privileges  = ["SELECT"]
}

resource "postgresql_default_privileges" "noschema" {
	database    = "%[1]s"
	role        = "%[2]s"
	owner       = "%[3]s"
	object_type = "sequence"
	privileges  = ["USAGE"]
}
`, dbName, roleName, owner)
}

var grantReadDisappearsResources = []string{
	"postgresql_grant.table",
	"postgresql_grant.public_table",
	"postgresql_grant.schema",
	"postgresql_grant.function",
	"postgresql_grant.database",
	"postgresql_default_privileges.schema",
	"postgresql_default_privileges.noschema",
}

// testAccCheckOnlyInState checks that, of grantReadDisappearsResources,
// exactly the kept ones are still in state.
func testAccCheckOnlyInState(kept ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, name := range grantReadDisappearsResources {
			_, inState := s.RootModule().Resources[name]
			want := sliceContainsStr(kept, name)
			if inState != want {
				return fmt.Errorf("%s: in state = %t after refresh, want %t", name, inState, want)
			}
		}
		return nil
	}
}

// testAccGrantReadDisappears applies grantReadDisappearsConfig, runs drop
// out-of-band, then checks that the following refresh succeeds and prunes
// every resource except kept — a dropped role, schema or database must never
// be read as "object without privileges".
func testAccGrantReadDisappears(t *testing.T, drop func(t *testing.T, dbName, roleName string), kept ...string) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	createTestTables(t, dbSuffix, []string{"test_schema.test_table"}, "")
	dbExecute(t, config.connStr(dbName), "CREATE FUNCTION test_schema.test_read_fn() RETURNS int LANGUAGE sql AS 'SELECT 1'")

	tfConfig := grantReadDisappearsConfig(dbName, roleName, config.Username)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckOnlyInState(grantReadDisappearsResources...),
					func(*terraform.State) error {
						drop(t, dbName, roleName)
						return nil
					},
				),
				ExpectNonEmptyPlan: true,
			},
			{
				RefreshState:       true,
				Check:              testAccCheckOnlyInState(kept...),
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccPostgresqlGrantRead_RoleDisappears(t *testing.T) {
	testAccGrantReadDisappears(t, func(t *testing.T, dbName, roleName string) {
		config := getTestConfig(t)
		// Membership lets a non-superuser (rds profile) drop what the role owns.
		dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT %s TO CURRENT_USER", pq.QuoteIdentifier(roleName)))
		dbExecute(t, config.connStr(dbName), fmt.Sprintf("DROP OWNED BY %s", pq.QuoteIdentifier(roleName)))
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("DROP ROLE %s", pq.QuoteIdentifier(roleName)))
	}, "postgresql_grant.public_table")
}

func TestAccPostgresqlGrantRead_SchemaDisappears(t *testing.T) {
	testAccGrantReadDisappears(t, func(t *testing.T, dbName, _ string) {
		config := getTestConfig(t)
		dbExecute(t, config.connStr(dbName), "DROP SCHEMA test_schema CASCADE")
	}, "postgresql_grant.database", "postgresql_default_privileges.noschema")
}

func TestAccPostgresqlGrantRead_DatabaseDisappears(t *testing.T) {
	testAccGrantReadDisappears(t, func(t *testing.T, dbName, _ string) {
		if err := testAccDropDatabase(t, dbName)(nil); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAccPostgresqlGrantRead_PublicColumns pins column grants to PUBLIC: the
// Read resolves PUBLIC as grantee 0 like every other object type, so the
// granted columns are found and the plan is empty after apply. The previous
// column query joined grantees on pg_roles, where PUBLIC has no row, so it
// always reported the columns as missing.
func TestAccPostgresqlGrantRead_PublicColumns(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	testTables := []string{"test_schema.test_table"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, roleName := getTestDBNames(dbSuffix)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "postgresql_grant" "test" {
	database    = "%s"
	role        = "public"
	schema      = "test_schema"
	object_type = "column"
	objects     = ["test_table"]
	columns     = ["test_column_one", "test_column_two"]
	privileges  = ["UPDATE"]
}
`, dbName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.test", "columns.#", "2"),
					resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "1"),
					func(*terraform.State) error {
						// PUBLIC applies to every role, including the test role.
						return testCheckColumnPrivileges(t, dbName, roleName, []string{testTables[0]}, []string{"UPDATE"}, []string{"test_column_one", "test_column_two"})
					},
				),
			},
		},
	})
}
