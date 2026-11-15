package postgresql

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func TestAccPostgresqlSecurityLabel_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testCheckCanSetSecurityLabel(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSecurityLabelDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSecurityLabelConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelOfRole(t, "security_label_test_role", "classified"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_type", "role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test_role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label_provider", "dummy"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "classified"),
				),
			},
			// the label is removed, the role stays: the catalog has no label
			{
				Config: testAccPostgresqlSecurityLabelRoleOnlyBasic,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelAbsentOfRole(t, "security_label_test_role"),
					testAccCheckRoleExistsInCatalog("security_label_test_role"),
				),
			},
		},
	})
}

func TestAccPostgresqlSecurityLabel_Update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testCheckCanSetSecurityLabel(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSecurityLabelDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSecurityLabelConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelOfRole(t, "security_label_test_role", "classified"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_type", "role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test_role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label_provider", "dummy"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "classified"),
				),
			},
			{
				Config: testAccPostgresqlSecurityLabelChanges2,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelOfRole(t, "security_label_test_role", "unclassified"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "unclassified"),
				),
			},
			{
				Config: testAccPostgresqlSecurityLabelChanges3,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test-role2"),
					// the label is on the renamed role, not only in the state
					testAccCheckSecurityLabelOfRole(t, "security_label_test-role2", "unclassified"),
					testAccCheckSecurityLabelAbsentOfRole(t, "security_label_test_role"),
				),
			},
			// the label is removed, the role stays: the catalog has no label
			{
				Config: testAccPostgresqlSecurityLabelRoleOnly,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelAbsentOfRole(t, "security_label_test-role2"),
					testAccCheckRoleExistsInCatalog("security_label_test-role2"),
				),
			},
		},
	})
}

// The label provider can be omitted when only one provider is loaded: it is then read back.
func TestAccPostgresqlSecurityLabel_NoProvider(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	dbName, _ := getTestDBNames(dbSuffix)
	cfg := getTestConfig(t)
	dbExecute(t, cfg.connStr(dbName), "CREATE TABLE test_schema.sl_table (id int)")

	config := fmt.Sprintf(`
resource "postgresql_security_label" "test" {
  database    = %q
  object_type = "table"
  object_name = "test_schema.sl_table"
  label       = "classified"
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testCheckCanExecute(
				t, "set a security label without a provider",
				"CREATE ROLE tf_probe_label_noprovider",
				"SECURITY LABEL ON ROLE tf_probe_label_noprovider IS 'classified'",
			)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_security_label.test", "label_provider", "dummy"),
					testAccCheckSecurityLabelInCatalog(t, dbName, "table", "test_schema.sl_table", "dummy", "classified"),
				),
			},
			{
				ResourceName:      "postgresql_security_label.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// securityLabelObjectCase describes a test of one object type. The placeholders {db} and
// {suffix} are replaced by the name of the test database and by its suffix.
type securityLabelObjectCase struct {
	// objectType and objectName are the attributes of the resource
	objectType string
	objectName string
	// sqlName is the name of the object as written in a SECURITY LABEL statement
	sqlName string
	// catalogType and catalogName are the objtype and the objname of pg_seclabels
	catalogType string
	catalogName string
	// setup creates the object, cleanup removes it when it is not dropped with the test database.
	// setupOptions are libpq options of the connection used for setup (e.g. for in-place tablespaces).
	setup        []string
	setupOptions string
	cleanup      []string
	// precheck skips the test when the connected user cannot create the object
	precheck func(t *testing.T)
}

func testAccPostgresqlSecurityLabelObject(t *testing.T, c securityLabelObjectCase) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	dbName, _ := getTestDBNames(dbSuffix)
	replacer := strings.NewReplacer("{db}", dbName, "{suffix}", dbSuffix)
	objectName := replacer.Replace(c.objectName)
	sqlName := replacer.Replace(c.sqlName)
	catalogName := replacer.Replace(c.catalogName)
	config := getTestConfig(t)

	// The objects which are not dropped with the test database are removed (a subscription is
	// dropped in its own database)
	defer func() {
		for _, statement := range c.cleanup {
			dbExecute(t, config.connStr(dbName), replacer.Replace(statement))
		}
	}()

	resourceConfig := func(label string) string {
		return fmt.Sprintf(`
resource "postgresql_security_label" "test" {
  database       = %q
  object_type    = %q
  object_name    = %q
  label_provider = "dummy"
  label          = %q
}
`, dbName, c.objectType, objectName, label)
	}

	setupDSN := config.connStr(dbName)
	if c.setupOptions != "" {
		setupDSN += "&options=" + url.QueryEscape(c.setupOptions)
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testCheckCanSetSecurityLabel(t)
			if c.precheck != nil {
				c.precheck(t)
			}
			for _, statement := range c.setup {
				dbExecute(t, setupDSN, replacer.Replace(statement))
			}
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: resourceConfig("classified"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelInCatalog(t, dbName, c.catalogType, catalogName, "dummy", "classified"),
					resource.TestCheckResourceAttr("postgresql_security_label.test", "label", "classified"),
				),
			},
			{
				Config: resourceConfig("unclassified"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelInCatalog(t, dbName, c.catalogType, catalogName, "dummy", "unclassified"),
					resource.TestCheckResourceAttr("postgresql_security_label.test", "label", "unclassified"),
				),
			},
			{
				ResourceName:      "postgresql_security_label.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// The label is changed outside of Terraform: the plan must show it
				PreConfig: func() {
					dbExecute(t, setupDSN, fmt.Sprintf(
						"SECURITY LABEL FOR dummy ON %s %s IS 'classified'",
						strings.ToUpper(c.objectType), replacer.Replace(sqlName),
					))
				},
				Config:             resourceConfig("unclassified"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: resourceConfig("unclassified"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelInCatalog(t, dbName, c.catalogType, catalogName, "dummy", "unclassified"),
				),
			},
			{
				// The resource is removed: its label is removed from the catalogs
				Config: "# no security label\n",
				Check: resource.ComposeTestCheckFunc(
					testAccCheckSecurityLabelAbsent(t, dbName, c.catalogType, catalogName, "dummy"),
				),
			},
		},
	})
}

// testAccSecurityLabelQuery returns the label of an object as pg_seclabels lists it, or sql.ErrNoRows.
func testAccSecurityLabelQuery(t *testing.T, dbName, catalogType, catalogName, provider string) (string, error) {
	cfg := getTestConfig(t)
	db, err := sql.Open("postgres", cfg.connStr(dbName))
	if err != nil {
		t.Fatalf("could not create connection pool: %v", err)
	}
	defer closeDB(t, db)

	var label string
	err = db.QueryRow(
		"SELECT label FROM pg_seclabels WHERE objtype = $1 AND objname = $2 AND provider = $3",
		catalogType, catalogName, provider,
	).Scan(&label)
	return label, err
}

func testAccCheckSecurityLabelInCatalog(t *testing.T, dbName, catalogType, catalogName, provider, label string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := testAccSecurityLabelQuery(t, dbName, catalogType, catalogName, provider)
		if err != nil {
			return fmt.Errorf("no security label for %s %s in pg_seclabels: %w", catalogType, catalogName, err)
		}
		if got != label {
			return fmt.Errorf("security label of %s %s is %q, expected %q", catalogType, catalogName, got, label)
		}
		return nil
	}
}

func testAccCheckSecurityLabelAbsent(t *testing.T, dbName, catalogType, catalogName, provider string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := testAccSecurityLabelQuery(t, dbName, catalogType, catalogName, provider)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("could not read security label: %w", err)
		}
		return fmt.Errorf("security label %q of %s %s still exists", got, catalogType, catalogName)
	}
}

// testAccRoleCatalogName returns the name of a role as pg_seclabels writes it (quote_ident).
func testAccRoleCatalogName(role string) string {
	if regexp.MustCompile(`^[a-z_][a-z0-9_]*$`).MatchString(role) {
		return role
	}
	return pq.QuoteIdentifier(role)
}

// testAccCheckSecurityLabelOfRole checks the label of a role in pg_seclabels (the labels of the
// roles are shared, any database shows them).
func testAccCheckSecurityLabelOfRole(t *testing.T, role, label string) resource.TestCheckFunc {
	return testAccCheckSecurityLabelInCatalog(t, "postgres", "role", testAccRoleCatalogName(role), "dummy", label)
}

// testAccCheckSecurityLabelAbsentOfRole checks that a role has no label of the provider.
func testAccCheckSecurityLabelAbsentOfRole(t *testing.T, role string) resource.TestCheckFunc {
	return testAccCheckSecurityLabelAbsent(t, "postgres", "role", testAccRoleCatalogName(role), "dummy")
}

// The label of each resource of the state is gone. The roles of the tests are dropped by the
// destroy, so this proves that the label is not left in the catalog for any other object
// either: pg_shseclabel has no row of the provider whose object does not exist.
func testAccCheckPostgresqlSecurityLabelDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)
	db, err := client.Connect()
	if err != nil {
		return err
	}

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_security_label" {
			continue
		}
		objectType := rs.Primary.Attributes[securityLabelObjectTypeAttr]
		objectName := rs.Primary.Attributes[securityLabelObjectNameAttr]
		if objectType != "role" {
			return fmt.Errorf("the destroy check only knows the roles, not %s %s", objectType, objectName)
		}

		var count int
		err := db.QueryRow(
			"SELECT count(*) FROM pg_seclabels WHERE objtype = 'role' AND objname = $1 AND provider = $2",
			testAccRoleCatalogName(objectName), rs.Primary.Attributes[securityLabelProviderAttr],
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("error checking security label: %w", err)
		}
		if count != 0 {
			return fmt.Errorf("the security label of role %s still exists after destroy", objectName)
		}
	}

	var orphans int
	if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_shseclabel l
		WHERE l.provider = 'dummy' AND l.classoid = 'pg_authid'::regclass
		AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_authid a WHERE a.oid = l.objoid)`).Scan(&orphans); err != nil {
		return fmt.Errorf("error checking security labels: %w", err)
	}
	if orphans != 0 {
		return fmt.Errorf("%d security labels of dropped roles are left", orphans)
	}

	return nil
}

var testAccPostgresqlSecurityLabelConfig = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "classified"
}
`

var testAccPostgresqlSecurityLabelChanges2 = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "unclassified"
}
`

var testAccPostgresqlSecurityLabelChanges3 = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test-role2"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "unclassified"
}
`

var testAccPostgresqlSecurityLabelRoleOnlyBasic = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
`

var testAccPostgresqlSecurityLabelRoleOnly = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test-role2"
  login           = true
  create_database = true
}
`

func testAccCheckRoleExistsInCatalog(role string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		db, err := testAccProvider.Meta().(*Client).Connect()
		if err != nil {
			return err
		}
		var exists bool
		if err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("role %s does not exist", role)
		}
		return nil
	}
}

// testCheckSecurityLabelSuperuser skips the test when the connected user is not a real superuser,
// because PostgreSQL itself requires one to create the object.
func testCheckSecurityLabelSuperuser(t *testing.T) {
	testSuperuserPreCheck(t)
}

// testCheckSecurityLabelInPlaceTablespace skips the test unless the server is PostgreSQL 16 or
// above (in-place tablespaces need no directory on the server) and the user is a superuser.
func testCheckSecurityLabelInPlaceTablespace(t *testing.T) {
	testSuperuserPreCheck(t)
	client := testAccProvider.Meta().(*Client)
	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect to database: %v", err)
	}
	var version int
	if err := db.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("could not read the server version: %v", err)
	}
	if version < 160000 {
		t.Skipf("Skip test: a tablespace needs an empty directory owned by the server user before PostgreSQL 16")
	}
}

func TestAccPostgresqlSecurityLabel_Table(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "table",
		objectName:  "test_schema.sl_table",
		sqlName:     "test_schema.sl_table",
		catalogType: "table",
		catalogName: "test_schema.sl_table",
		setup:       []string{"CREATE TABLE test_schema.sl_table (id int)"},
	})
}

func TestAccPostgresqlSecurityLabel_Column(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "column",
		objectName:  "test_schema.sl-Table.Col-x",
		sqlName:     `test_schema."sl-Table"."Col-x"`,
		catalogType: "column",
		catalogName: `test_schema."sl-Table".Col-x`,
		setup:       []string{`CREATE TABLE test_schema."sl-Table" (id int, "Col-x" int)`},
	})
}

func TestAccPostgresqlSecurityLabel_View(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "view",
		objectName:  "test_schema.sl_view",
		sqlName:     "test_schema.sl_view",
		catalogType: "view",
		catalogName: "test_schema.sl_view",
		setup:       []string{"CREATE VIEW test_schema.sl_view AS SELECT 1 AS x"},
	})
}

func TestAccPostgresqlSecurityLabel_MaterializedView(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "materialized view",
		objectName:  "test_schema.sl_matview",
		sqlName:     "test_schema.sl_matview",
		catalogType: "materialized view",
		catalogName: "test_schema.sl_matview",
		setup:       []string{"CREATE MATERIALIZED VIEW test_schema.sl_matview AS SELECT 1 AS x"},
	})
}

func TestAccPostgresqlSecurityLabel_Sequence(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "sequence",
		objectName:  "test_schema.sl_seq",
		sqlName:     "test_schema.sl_seq",
		catalogType: "sequence",
		catalogName: "test_schema.sl_seq",
		setup:       []string{"CREATE SEQUENCE test_schema.sl_seq"},
	})
}

func TestAccPostgresqlSecurityLabel_ForeignTable(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "foreign table",
		objectName:  "test_schema.sl_foreign",
		sqlName:     "test_schema.sl_foreign",
		catalogType: "foreign table",
		catalogName: "test_schema.sl_foreign",
		setup: []string{
			"CREATE FOREIGN DATA WRAPPER sl_fdw",
			"CREATE SERVER sl_server FOREIGN DATA WRAPPER sl_fdw",
			"CREATE FOREIGN TABLE test_schema.sl_foreign (id int) SERVER sl_server",
		},
		precheck: testCheckSecurityLabelSuperuser,
	})
}

func TestAccPostgresqlSecurityLabel_Function(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "function",
		objectName:  "test_schema.sl_func(int, text)",
		sqlName:     "test_schema.sl_func(int, text)",
		catalogType: "function",
		catalogName: "test_schema.sl_func(a integer, b text)",
		setup:       []string{"CREATE FUNCTION test_schema.sl_func(a int, b text) RETURNS int LANGUAGE sql AS 'SELECT 1'"},
	})
}

// A function without arguments, whose name needs quotes, is named without parentheses
func TestAccPostgresqlSecurityLabel_FunctionWithoutArguments(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "function",
		objectName:  "test_schema.sl-func",
		sqlName:     `test_schema."sl-func"`,
		catalogType: "function",
		catalogName: `test_schema."sl-func"()`,
		setup:       []string{`CREATE FUNCTION test_schema."sl-func"() RETURNS int LANGUAGE sql AS 'SELECT 1'`},
	})
}

func TestAccPostgresqlSecurityLabel_Routine(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "routine",
		objectName:  "test_schema.sl_func(int, text)",
		sqlName:     "test_schema.sl_func(int, text)",
		catalogType: "function",
		catalogName: "test_schema.sl_func(a integer, b text)",
		setup:       []string{"CREATE FUNCTION test_schema.sl_func(a int, b text) RETURNS int LANGUAGE sql AS 'SELECT 1'"},
	})
}

func TestAccPostgresqlSecurityLabel_Procedure(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "procedure",
		objectName:  "test_schema.sl_proc()",
		sqlName:     "test_schema.sl_proc()",
		catalogType: "procedure",
		catalogName: "test_schema.sl_proc()",
		setup:       []string{"CREATE PROCEDURE test_schema.sl_proc() LANGUAGE sql AS 'SELECT 1'"},
	})
}

func TestAccPostgresqlSecurityLabel_Aggregate(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "aggregate",
		objectName:  "test_schema.sl_agg(int)",
		sqlName:     "test_schema.sl_agg(int)",
		catalogType: "aggregate",
		catalogName: "test_schema.sl_agg(integer)",
		setup: []string{
			"CREATE AGGREGATE test_schema.sl_agg(int) (SFUNC = int4pl, STYPE = int)",
		},
	})
}

func TestAccPostgresqlSecurityLabel_Type(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "type",
		objectName:  "test_schema.sl_type",
		sqlName:     "test_schema.sl_type",
		catalogType: "type",
		catalogName: "test_schema.sl_type",
		setup:       []string{"CREATE TYPE test_schema.sl_type AS (a int)"},
	})
}

func TestAccPostgresqlSecurityLabel_Domain(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "domain",
		objectName:  "test_schema.sl_domain",
		sqlName:     "test_schema.sl_domain",
		catalogType: "domain",
		catalogName: "test_schema.sl_domain",
		setup:       []string{"CREATE DOMAIN test_schema.sl_domain AS int"},
	})
}

func TestAccPostgresqlSecurityLabel_Schema(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "schema",
		objectName:  "sl-schema-{suffix}",
		sqlName:     `"sl-schema-{suffix}"`,
		catalogType: "schema",
		catalogName: `"sl-schema-{suffix}"`,
		setup:       []string{`CREATE SCHEMA "sl-schema-{suffix}"`},
	})
}

// The label of a database is shared by all the databases: the test labels its own database
func TestAccPostgresqlSecurityLabel_Database(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "database",
		objectName:  "{db}",
		sqlName:     `"{db}"`,
		catalogType: "database",
		catalogName: "{db}",
	})
}

func TestAccPostgresqlSecurityLabel_RoleWithQuotedName(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "role",
		objectName:  "sl-role-{suffix}",
		sqlName:     `"sl-role-{suffix}"`,
		catalogType: "role",
		catalogName: `"sl-role-{suffix}"`,
		setup:       []string{`CREATE ROLE "sl-role-{suffix}"`},
		cleanup:     []string{`DROP ROLE IF EXISTS "sl-role-{suffix}"`},
	})
}

func TestAccPostgresqlSecurityLabel_Tablespace(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "tablespace",
		objectName:  "sl-ts-{suffix}",
		sqlName:     `"sl-ts-{suffix}"`,
		catalogType: "tablespace",
		catalogName: `"sl-ts-{suffix}"`,
		setup:       []string{`CREATE TABLESPACE "sl-ts-{suffix}" LOCATION ''`},
		// An in-place tablespace needs no directory on the server
		setupOptions: "-c allow_in_place_tablespaces=on",
		cleanup:      []string{`DROP TABLESPACE IF EXISTS "sl-ts-{suffix}"`},
		precheck:     testCheckSecurityLabelInPlaceTablespace,
	})
}

func TestAccPostgresqlSecurityLabel_EventTrigger(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "event trigger",
		objectName:  "sl_evt",
		sqlName:     "sl_evt",
		catalogType: "event trigger",
		catalogName: "sl_evt",
		setup: []string{
			"CREATE FUNCTION test_schema.sl_evt_fn() RETURNS event_trigger LANGUAGE plpgsql AS $$ BEGIN END $$",
			"CREATE EVENT TRIGGER sl_evt ON ddl_command_end WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION test_schema.sl_evt_fn()",
		},
		precheck: testCheckSecurityLabelSuperuser,
	})
}

func TestAccPostgresqlSecurityLabel_Language(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "language",
		objectName:  "sl_lang",
		sqlName:     "sl_lang",
		catalogType: "language",
		catalogName: "sl_lang",
		setup:       []string{"CREATE LANGUAGE sl_lang HANDLER plpgsql_call_handler"},
		precheck:    testCheckSecurityLabelSuperuser,
	})
}

func TestAccPostgresqlSecurityLabel_ProceduralLanguage(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "procedural language",
		objectName:  "sl_lang",
		sqlName:     "sl_lang",
		catalogType: "language",
		catalogName: "sl_lang",
		setup:       []string{"CREATE LANGUAGE sl_lang HANDLER plpgsql_call_handler"},
		precheck:    testCheckSecurityLabelSuperuser,
	})
}

func TestAccPostgresqlSecurityLabel_Publication(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "publication",
		objectName:  "sl-pub-{suffix}",
		sqlName:     `"sl-pub-{suffix}"`,
		catalogType: "publication",
		catalogName: `"sl-pub-{suffix}"`,
		setup:       []string{`CREATE PUBLICATION "sl-pub-{suffix}"`},
	})
}

// A subscription is created without connecting to a publisher
func TestAccPostgresqlSecurityLabel_Subscription(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "subscription",
		objectName:  "sl-sub-{suffix}",
		sqlName:     `"sl-sub-{suffix}"`,
		catalogType: "subscription",
		catalogName: `"sl-sub-{suffix}"`,
		setup: []string{
			`CREATE SUBSCRIPTION "sl-sub-{suffix}" CONNECTION 'dbname=postgres' PUBLICATION sl_pub WITH (connect = false, slot_name = NONE)`,
		},
		cleanup: []string{`DROP SUBSCRIPTION IF EXISTS "sl-sub-{suffix}"`},
		precheck: func(t *testing.T) {
			testCheckCanExecute(
				t, "create a subscription",
				"CREATE SUBSCRIPTION tf_probe_subscription CONNECTION 'dbname=postgres' PUBLICATION sl_pub WITH (connect = false, slot_name = NONE)",
			)
		},
	})
}

// A large object is named by its OID
func TestAccPostgresqlSecurityLabel_LargeObject(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "large object",
		objectName:  "424242",
		sqlName:     "424242",
		catalogType: "large object",
		catalogName: "424242",
		setup:       []string{"SELECT lo_create(424242)"},
	})
}

// The argument types can have a modifier or be schema-qualified.
func TestAccPostgresqlSecurityLabel_FunctionTypeModifiers(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "function",
		objectName:  "test_schema.sl_mod(varchar(10), numeric(10,2), test_schema.sl_t)",
		sqlName:     "test_schema.sl_mod(varchar(10), numeric(10,2), test_schema.sl_t)",
		catalogType: "function",
		catalogName: "test_schema.sl_mod(a character varying, b numeric, c test_schema.sl_t)",
		setup: []string{
			"CREATE TYPE test_schema.sl_t AS (a int)",
			"CREATE FUNCTION test_schema.sl_mod(a varchar(10), b numeric(10,2), c test_schema.sl_t) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		},
	})
}

// The name of a routine can contain a parenthesis (the name is quoted by the provider).
func TestAccPostgresqlSecurityLabel_FunctionParenthesisInName(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "function",
		objectName:  "test_schema.sl(odd(varchar(10))",
		sqlName:     `test_schema."sl(odd"(varchar(10))`,
		catalogType: "function",
		catalogName: `test_schema."sl(odd"(a character varying)`,
		setup:       []string{`CREATE FUNCTION test_schema."sl(odd"(a varchar(10)) RETURNS int LANGUAGE sql AS 'SELECT 1'`},
	})
}

// ... also when it has no arguments, and when the name of a table or a role contains one
func TestAccPostgresqlSecurityLabel_FunctionParenthesisInNameNoArguments(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "function",
		objectName:  "test_schema.sl(odd",
		sqlName:     `test_schema."sl(odd"`,
		catalogType: "function",
		catalogName: `test_schema."sl(odd"()`,
		setup:       []string{`CREATE FUNCTION test_schema."sl(odd"() RETURNS int LANGUAGE sql AS 'SELECT 1'`},
	})
}

func TestAccPostgresqlSecurityLabel_ParenthesisInNames(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "table",
		objectName:  "test_schema.sl(t)",
		sqlName:     `test_schema."sl(t)"`,
		catalogType: "table",
		catalogName: `test_schema."sl(t)"`,
		setup:       []string{`CREATE TABLE test_schema."sl(t)" (id int)`},
	})
}

func TestAccPostgresqlSecurityLabel_RoleWithParenthesisAndDot(t *testing.T) {
	testAccPostgresqlSecurityLabelObject(t, securityLabelObjectCase{
		objectType:  "role",
		objectName:  "sl.role(x)-{suffix}",
		sqlName:     `"sl.role(x)-{suffix}"`,
		catalogType: "role",
		catalogName: `"sl.role(x)-{suffix}"`,
		setup:       []string{`CREATE ROLE "sl.role(x)-{suffix}"`},
		cleanup:     []string{`DROP ROLE IF EXISTS "sl.role(x)-{suffix}"`},
	})
}

func TestSplitRoutineName(t *testing.T) {
	cases := []struct{ in, name, signature string }{
		{"f", "f", ""},
		{"s.f", "s.f", ""},
		{"f()", "f", "()"},
		{"s.f(int, text)", "s.f", "(int, text)"},
		{"f(varchar(10), numeric(10,2))", "f", "(varchar(10), numeric(10,2))"},
		{"f(s.t)", "f", "(s.t)"},
		{"f(o(int)", "f(o", "(int)"},
		{"f(o", "f(o", ""},
		{"a(b)(int)", "a(b)", "(int)"},
		{`f("my(type")`, "f", `("my(type")`},
	}
	for _, c := range cases {
		name, signature := splitRoutineName(c.in)
		if name != c.name || signature != c.signature {
			t.Errorf("splitRoutineName(%q) = %q, %q; expected %q, %q", c.in, name, signature, c.name, c.signature)
		}
	}
}

func TestSecurityLabelID(t *testing.T) {
	cases := []struct {
		id    string
		parts [4]string
	}{
		{"mydb.dummy.role.my_role", [4]string{"mydb", "dummy", "role", "my_role"}},
		{"mydb.dummy.table.s.t", [4]string{"mydb", "dummy", "table", "s.t"}},
		{`"my.db".dummy.role.r`, [4]string{"my.db", "dummy", "role", "r"}},
		{`mydb."my.prov".role.r`, [4]string{"mydb", "my.prov", "role", "r"}},
		{`"a""b.c"."p.q".table.s.t`, [4]string{`a"b.c`, "p.q", "table", "s.t"}},
		{".dummy.role.r", [4]string{"", "dummy", "role", "r"}},
	}
	for _, c := range cases {
		parts, err := parseSecurityLabelID(c.id)
		if err != nil || parts != c.parts {
			t.Errorf("parseSecurityLabelID(%q) = %q, %v; expected %q", c.id, parts, err, c.parts)
		}
		// the ID is written back the same way
		written := strings.Join([]string{quoteSecurityLabelIDPart(c.parts[0]), quoteSecurityLabelIDPart(c.parts[1]), c.parts[2], c.parts[3]}, ".")
		if strings.HasPrefix(c.id, `"`) || strings.Contains(c.parts[0]+c.parts[1], ".") {
			if written != c.id {
				t.Errorf("the ID of %q is %q, expected %q", c.parts, written, c.id)
			}
		}
	}
	for _, id := range []string{"", "a.b.c", `"a.b.c.d`, `"a"b.c.d.e`} {
		if _, err := parseSecurityLabelID(id); err == nil {
			t.Errorf("parseSecurityLabelID(%q) did not fail", id)
		}
	}
}

// A database whose name contains a dot is imported (its ID has the name between quotes).
func TestAccPostgresqlSecurityLabel_ImportDatabaseWithDot(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, false)
	defer teardown()
	dbName := "sl.db-" + dbSuffix
	cfg := getTestConfig(t)
	var created bool
	defer func() {
		if created {
			dbExecute(t, cfg.connStr("postgres"), fmt.Sprintf("DROP DATABASE IF EXISTS %s", pq.QuoteIdentifier(dbName)))
		}
	}()

	config := fmt.Sprintf(`
resource "postgresql_security_label" "test" {
  database       = %q
  object_type    = "table"
  object_name    = "sl_table"
  label_provider = "dummy"
  label          = "classified"
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testCheckCanSetSecurityLabel(t)
			dbExecute(t, cfg.connStr("postgres"), fmt.Sprintf("CREATE DATABASE %s", pq.QuoteIdentifier(dbName)))
			created = true
			dbExecute(t, cfg.connStr(dbName), "CREATE TABLE sl_table (id int)")
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_security_label.test", "id", fmt.Sprintf(`"%s".dummy.table.sl_table`, dbName)),
					testAccCheckSecurityLabelInCatalog(t, dbName, "table", "sl_table", "dummy", "classified"),
				),
			},
			{
				ResourceName:      "postgresql_security_label.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// nothing to do after the import
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}
