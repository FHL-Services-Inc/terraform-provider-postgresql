package postgresql

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
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
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
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
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
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
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "unclassified"),
				),
			},
			{
				Config: testAccPostgresqlSecurityLabelChanges3,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test-role2"),
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

func checkSecurityLabelExists(txn *sql.Tx, objectType string, objectName string, provider string) (bool, error) {
	var _rez bool
	err := txn.QueryRow("SELECT TRUE FROM pg_seclabels WHERE objtype = $1 AND objname = quote_ident($2) AND provider = $3", objectType, objectName, provider).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about security label: %s", err)
	}

	return true, nil
}

func testAccCheckPostgresqlSecurityLabelDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_security_label" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkSecurityLabelExists(
			txn,
			rs.Primary.Attributes[securityLabelObjectTypeAttr],
			rs.Primary.Attributes[securityLabelObjectNameAttr],
			rs.Primary.Attributes[securityLabelProviderAttr],
		)

		if err != nil {
			return fmt.Errorf("error checking security label%s", err)
		}

		if exists {
			return fmt.Errorf("Security label still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlSecurityLabelExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		objectType, ok := rs.Primary.Attributes[securityLabelObjectTypeAttr]
		if !ok {
			return fmt.Errorf("No Attribute for object type is set")
		}

		objectName, ok := rs.Primary.Attributes[securityLabelObjectNameAttr]
		if !ok {
			return fmt.Errorf("No Attribute for object name is set")
		}

		provider, ok := rs.Primary.Attributes[securityLabelProviderAttr]
		if !ok {
			return fmt.Errorf("No Attribute for security provider is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkSecurityLabelExists(txn, objectType, objectName, provider)

		if err != nil {
			return fmt.Errorf("error checking security label%s", err)
		}

		if !exists {
			return fmt.Errorf("Security label not found")
		}

		return nil
	}
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
