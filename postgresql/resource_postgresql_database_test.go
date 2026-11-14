package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPostgresqlDatabase_Basic(t *testing.T) {
	n := newTestAccDatabaseNames()

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgreSQLDatabaseConfig(n),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.mydb"),
					resource.TestCheckResourceAttr(
						"postgresql_database.mydb", "name", n.mydb),
					resource.TestCheckResourceAttr(
						"postgresql_database.mydb", "owner", n.role),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "owner", n.role),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "name", n.defaultOpts),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "template", "template0"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "encoding", "UTF8"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "lc_collate", "C"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "lc_ctype", "C"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "tablespace_name", "pg_default"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "connection_limit", "-1"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "is_template", "false"),
					resource.TestCheckResourceAttr(
						"postgresql_database.default_opts", "alter_object_ownership", "false"),

					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "owner", n.role),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "name", n.modifiedOpts),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "template", "template0"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "encoding", "UTF8"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "lc_collate", "en_US.UTF-8"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "lc_ctype", "en_US.UTF-8"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "tablespace_name", "pg_default"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "connection_limit", "10"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "is_template", "true"),
					resource.TestCheckResourceAttr(
						"postgresql_database.modified_opts", "alter_object_ownership", "true"),

					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "owner", n.role),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "name", n.pathologicalOpts),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "template", "template0"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "encoding", "LATIN1"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "lc_collate", "C"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "lc_ctype", "C"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "tablespace_name", "pg_default"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "connection_limit", "0"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pathological_opts", "is_template", "true"),

					resource.TestCheckResourceAttr(
						"postgresql_database.pg_default_opts", "owner", n.role),
					resource.TestCheckResourceAttr(
						"postgresql_database.pg_default_opts", "name", n.pgDefaultOpts),
					resource.TestCheckResourceAttr(
						"postgresql_database.pg_default_opts", "template", "DEFAULT"),
					// resource.TestCheckResourceAttr(
					// 	"postgresql_database.pg_default_opts", "encoding", "DEFAULT"),
					// resource.TestCheckResourceAttr(
					// 	"postgresql_database.pg_default_opts", "lc_collate", "DEFAULT"),
					// resource.TestCheckResourceAttr(
					//  "postgresql_database.pg_default_opts", "lc_ctype", "DEFAULT"),
					// resource.TestCheckResourceAttr(
					// 	"postgresql_database.pg_default_opts", "tablespace_name", "DEFAULT"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pg_default_opts", "connection_limit", "0"),
					resource.TestCheckResourceAttr(
						"postgresql_database.pg_default_opts", "is_template", "true"),
				),
			},
		},
	})
}

func TestAccPostgresqlDatabase_DefaultOwner(t *testing.T) {
	n := newTestAccDatabaseNames()

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgreSQLDatabaseConfig(n),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.mydb_default_owner"),
					resource.TestCheckResourceAttr(
						"postgresql_database.mydb_default_owner", "name", n.defaultOwner),
					resource.TestCheckResourceAttrSet(
						"postgresql_database.mydb_default_owner", "owner"),
				),
			},
		},
	})
}

func TestAccPostgresqlDatabase_Update(t *testing.T) {

	// Version dependent features values will be set in PreCheck
	// because we need to access database to check Postgres version.

	// Allow connection depends on Postgres version (needs pg >= 9.5)
	var allowConnections bool

	dbName := testUniqueName("test_db")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)

			client := testAccProvider.Meta().(*Client)
			db, err := client.Connect()
			if err != nil {
				t.Fatalf("could not connect to database: %v", err)
			}
			allowConnections = db.featureSupported(featureDBAllowConnections)

		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource postgresql_database test_db {
    name = "%s"
	allow_connections = "%t"
}
`, dbName, allowConnections),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.test_db"),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "name", dbName),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "connection_limit", "-1"),
					resource.TestCheckResourceAttr(
						"postgresql_database.test_db", "allow_connections",
						strconv.FormatBool(allowConnections),
					),
				),
			},
			{
				Config: fmt.Sprintf(`
resource postgresql_database test_db {
	name = "%s"
	connection_limit = 2
	allow_connections = false
}
	`, dbName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.test_db"),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "name", dbName),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "connection_limit", "2"),
					resource.TestCheckResourceAttr(
						"postgresql_database.test_db", "allow_connections", "false",
					),
				),
			},
		},
	})
}

// Test the case where we need to grant the owner to the connected user.
// The owner should be revoked
func TestAccPostgresqlDatabase_GrantOwner(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	dbName := testUniqueName("test_db")
	owner := testUniqueName("test_owner")
	var stateConfig = fmt.Sprintf(`
resource postgresql_role "test_owner" {
       name = "%s"
}
resource postgresql_database "test_db" {
       name  = "%s"
       owner = "${postgresql_role.test_owner.name}"
}
`, owner, dbName)
	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: stateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.test_db"),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "name", dbName),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "owner", owner),

					// check if connected user does not have test_owner granted anymore.
					checkUserMembership(t, config.Username, owner, false),
				),
			},
		},
	})
}

// Test the case where the connected user is already a member of the owner.
// There were a bug which was revoking the owner anyway.
func TestAccPostgresqlDatabase_GrantOwnerNotNeeded(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	dbName := testUniqueName("test_db")
	owner := testUniqueName("test_owner")

	dbExecute(
		t, dsn,
		fmt.Sprintf("CREATE ROLE %s; GRANT %s TO %s", owner, owner, config.Username),
	)
	defer func() {
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE %s", owner))
	}()

	var stateConfig = fmt.Sprintf(`
resource postgresql_database "test_db" {
       name  = "%s"
       owner = "%s"
}
`, dbName, owner)
	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: stateConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.test_db"),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "name", dbName),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "owner", owner),

					// check if connected user still have test_owner granted.
					checkUserMembership(t, config.Username, owner, true),
				),
			},
		},
	})
}

// Test the case where the owned objects by the previous database owner are altered.
func TestAccPostgresqlDatabase_AlterObjectOwnership(t *testing.T) {
	skipIfNotAcc(t)

	const tableName = "testtable1"

	var (
		databaseSuffix = testUniqueName("ownership")
		previous_owner = testUniqueName("previous_owner")
		new_owner      = testUniqueName("new_owner")
	)

	databaseName := fmt.Sprintf("%s_%s", dbNamePrefix, databaseSuffix)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")

	for _, role := range []string{previous_owner, new_owner} {
		dbExecute(
			t, dsn,
			fmt.Sprintf("CREATE ROLE %s;", role),
		)
		defer func(role string) {
			dbExecute(t, dsn, fmt.Sprintf("DROP ROLE %s", role))
		}(role)

	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource postgresql_database "test_db" {
       name  = "%s"
       owner = "%s"
	   alter_object_ownership = true
}
`, databaseName, previous_owner),
				Check: func(*terraform.State) error {
					// To test default privileges, we need to create a table
					// after having apply the state.
					_ = createTestTables(t, databaseSuffix, []string{tableName}, previous_owner)
					return nil
				},
			},
			{
				Config: fmt.Sprintf(`
resource postgresql_database "test_db" {
       name  = "%s"
       owner = "%s"
	   alter_object_ownership = true
}
`, databaseName, new_owner),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.test_db"),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "name", databaseName),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "owner", new_owner),
					resource.TestCheckResourceAttr("postgresql_database.test_db", "alter_object_ownership", "true"),

					checkTableOwnership(t, config.connStr(databaseName), new_owner, tableName),
				),
			},
		},
	})

}

func checkUserMembership(
	t *testing.T, member, role string, shouldHaveRole bool,
) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Client)
		db, err := client.Connect()
		if err != nil {
			t.Fatalf("could to create connection pool: %v", err)
		}

		var _rez int
		query := "SELECT 1 FROM pg_auth_members WHERE pg_get_userbyid(roleid) = $1 AND pg_get_userbyid(member) = $2"
		if db.featureSupported(featureCreateRoleSelfGrant) {
			query += " AND (set_option OR inherit_option)"
		}
		err = db.QueryRow(query, role, member).Scan(&_rez)

		switch {
		case err == sql.ErrNoRows:
			if shouldHaveRole {
				return fmt.Errorf(
					"User %s is not a member of %s",
					member, role,
				)
			}
			return nil

		case err != nil:
			t.Fatalf("could not check granted role: %v", err)
		}

		if !shouldHaveRole {
			return fmt.Errorf(
				"User (%s) should not be a member of %s",
				member, role,
			)
		}
		return nil
	}
}

func checkTableOwnership(
	t *testing.T, dsn, owner, tableName string,
) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("could not create connection pool: %v", err)
		}
		defer closeDB(t, db)

		var _rez int

		err = db.QueryRow(`
			SELECT 1 FROM pg_tables
			WHERE tablename = $1 AND tableowner = $2
		`, tableName, owner).Scan(&_rez)

		switch {
		case err == sql.ErrNoRows:
			return fmt.Errorf(
				"User %s should be owner of %s but is not", owner, tableName,
			)
		case err != nil:
			t.Fatalf("error checking table ownership. %v", err)

		}

		return nil

	}
}

func testAccCheckPostgresqlDatabaseDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_database" {
			continue
		}

		exists, err := checkDatabaseExists(client, rs.Primary.ID)

		if err != nil {
			return fmt.Errorf("error checking db %s", err)
		}

		if exists {
			return errors.New("Db still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlDatabaseExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return errors.New("No ID is set")
		}

		client := testAccProvider.Meta().(*Client)
		exists, err := checkDatabaseExists(client, rs.Primary.ID)

		if err != nil {
			return fmt.Errorf("error checking db %s", err)
		}

		if !exists {
			return errors.New("Db not found")
		}

		return nil
	}
}

func checkDatabaseExists(client *Client, dbName string) (bool, error) {
	db, err := client.Connect()
	if err != nil {
		return false, err
	}
	var _rez int
	err = db.QueryRow("SELECT 1 from pg_database d WHERE datname=$1", dbName).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about database: %s", err)
	}

	return true, nil
}

// testAccDatabaseNames holds the names used by testAccPostgreSQLDatabaseConfig,
// unique per test so leftovers of an earlier run can't collide.
type testAccDatabaseNames struct {
	role, mydb, mydb2, defaultOpts, modifiedOpts, pathologicalOpts, pgDefaultOpts, defaultOwner string
}

func newTestAccDatabaseNames() testAccDatabaseNames {
	return testAccDatabaseNames{
		role:             testUniqueName("myrole"),
		mydb:             testUniqueName("mydb"),
		mydb2:            testUniqueName("mydb2"),
		defaultOpts:      testUniqueName("default_opts_name"),
		modifiedOpts:     testUniqueName("custom_template_db"),
		pathologicalOpts: testUniqueName("bad_template_db"),
		pgDefaultOpts:    testUniqueName("pg_defaults_db"),
		defaultOwner:     testUniqueName("mydb_default_owner"),
	}
}

func testAccPostgreSQLDatabaseConfig(n testAccDatabaseNames) string {
	return fmt.Sprintf(`
resource "postgresql_role" "myrole" {
  name = "%[1]s"
  login = true
}

resource "postgresql_database" "mydb" {
   name = "%[2]s"
   owner = "${postgresql_role.myrole.name}"
}

resource "postgresql_database" "mydb2" {
   name = "%[3]s"
   owner = "${postgresql_role.myrole.name}"
}

resource "postgresql_database" "default_opts" {
   name = "%[4]s"
   owner = "${postgresql_role.myrole.name}"
   template = "template0"
   encoding = "UTF8"
   lc_collate = "C"
   lc_ctype = "C"
   connection_limit = -1
   is_template = false
   alter_object_ownership = false
}

resource "postgresql_database" "modified_opts" {
   name = "%[5]s"
   owner = "${postgresql_role.myrole.name}"
   template = "template0"
   encoding = "UTF8"
   lc_collate = "en_US.UTF-8"
   lc_ctype = "en_US.UTF-8"
   connection_limit = 10
   is_template = true
   alter_object_ownership = true
}

resource "postgresql_database" "pathological_opts" {
   name = "%[6]s"
   owner = "${postgresql_role.myrole.name}"
   template = "template0"
   encoding = "LATIN1"
   lc_collate = "C"
   lc_ctype = "C"
   connection_limit = 0
   is_template = true
}

resource "postgresql_database" "pg_default_opts" {
  lifecycle {
    ignore_changes = [
      "template",
      "encoding",
      "lc_collate",
      "lc_ctype",
      "tablespace_name",
    ]
  }

  name = "%[7]s"
  owner = "${postgresql_role.myrole.name}"
  template = "DEFAULT"
  encoding = "DEFAULT"
  lc_collate = "DEFAULT"
  lc_ctype = "DEFAULT"
  tablespace_name = "DEFAULT"
  connection_limit = 0
  is_template = true
}

resource "postgresql_database" "mydb_default_owner" {
   name = "%[8]s"
}

`, n.role, n.mydb, n.mydb2, n.defaultOpts, n.modifiedOpts, n.pathologicalOpts, n.pgDefaultOpts, n.defaultOwner)
}

// TestAccPostgresqlDatabase_DropWithIdleConnections drops and recreates a
// database that the provider holds pooled connections to (because a schema
// lives in it) with max_idle_connections > 0. The pool for the dropped
// database must be evicted, otherwise the recreated database would be reached
// through stale, server-terminated connections.
func TestAccPostgresqlDatabase_DropWithIdleConnections(t *testing.T) {
	dbName := testUniqueName("tf_tests_idle_conns_db")

	// dbRegistry is process-wide and keyed by DSN, and a pool keeps the pool
	// settings of the configuration that opened it, so a pool opened by an
	// earlier test (with max_idle_connections = 0) would be reused here.
	// A connect_timeout unique to this test gives the provider its own DSN,
	// hence its own pools.
	providerOnly := `
provider "postgresql" {
  max_idle_connections = 2
  connect_timeout      = 175
}
`

	config := providerOnly + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name = "%[1]s"
}

resource "postgresql_schema" "schema" {
  name     = "idle_conns"
  database = postgresql_database.db.name
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) },
		// ProviderFactories (unlike Providers) lets the step config carry its
		// own provider block with max_idle_connections set.
		ProviderFactories: map[string]func() (*schema.Provider, error){
			"postgresql": func() (*schema.Provider, error) { return testAccProvider, nil },
		},
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_schema.schema", "database", dbName),
					testAccCheckDBConnectionRegistered(dbName, true),
				),
			},
			{
				// Drop the database (and the schema in it).
				Config: providerOnly,
				Check:  testAccCheckDBConnectionRegistered(dbName, false),
			},
			{
				// Recreate it under the same name and connect into it again.
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_schema.schema", "database", dbName),
					testAccCheckDBConnectionRegistered(dbName, true),
				),
			},
		},
	})
}

// testAccCheckDBConnectionRegistered checks whether the provider currently
// holds a pooled connection to the given database in dbRegistry.
func testAccCheckDBConnectionRegistered(dbName string, want bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Client)
		key := client.config.registryKey(dbName)

		dbRegistryLock.Lock()
		_, found := dbRegistry[key]
		dbRegistryLock.Unlock()

		if found != want {
			return fmt.Errorf("connection to database %q registered = %t, want %t", dbName, found, want)
		}
		return nil
	}
}

// singleConnProviderFactories lets a step config carry its own provider block
// (max_connections), which Providers does not.
var singleConnProviderFactories = map[string]func() (*schema.Provider, error){
	"postgresql": func() (*schema.Provider, error) { return testAccProvider, nil },
}

// TestAccPostgresqlDatabase_OwnerMaxConnectionsOne creates, re-owns and drops
// a database with an owner while the provider may open a single connection.
// Owner handling takes the role advisory lock and grants/revokes the owner
// membership; all of that has to run on one connection, otherwise the
// operation waits forever for a second connection from an exhausted pool.
func TestAccPostgresqlDatabase_OwnerMaxConnectionsOne(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)

	// dbRegistry is process-wide and keyed by DSN, so a pool opened by an
	// earlier test (with the default max_connections) would be reused here.
	// A connect_timeout unique to this test gives the provider its own pools.
	const providerConfig = `
provider "postgresql" {
  max_connections = 1
  connect_timeout = 178
}

resource "postgresql_role" "owner1" {
  name = "tf_tests_single_conn_owner1"
}

resource "postgresql_role" "owner2" {
  name = "tf_tests_single_conn_owner2"
}
`
	dbConfig := func(owner string) string {
		return providerConfig + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name  = "tf_tests_single_conn_db"
  owner = postgresql_role.%s.name
}
`, owner)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: singleConnProviderFactories,
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig("owner1"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", "tf_tests_single_conn_owner1"),
					checkUserMembership(t, config.Username, "tf_tests_single_conn_owner1", false),
				),
			},
			{
				// ALTER DATABASE OWNER also needs membership in the current
				// owner, which setDBOwner does not grant, so a non-superuser
				// cannot change the owner here (same without max_connections).
				SkipFunc: func() (bool, error) { return os.Getenv("PGSUPERUSER") == "false", nil },
				Config:   dbConfig("owner2"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", "tf_tests_single_conn_owner2"),
					checkUserMembership(t, config.Username, "tf_tests_single_conn_owner2", false),
				),
			},
			{
				// Drop the database but keep the roles, so the membership
				// check still has something to look at.
				Config: providerConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckDatabaseGone("tf_tests_single_conn_db"),
					checkUserMembership(t, config.Username, "tf_tests_single_conn_owner2", false),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabase_AlterObjectOwnershipMaxConnectionsOne is the
// alter_object_ownership variant of the test above (REASSIGN OWNED needs a
// superuser here, like TestAccPostgresqlDatabase_AlterObjectOwnership).
func TestAccPostgresqlDatabase_AlterObjectOwnershipMaxConnectionsOne(t *testing.T) {
	skipIfNotAcc(t)

	const providerConfig = `
provider "postgresql" {
  max_connections = 1
  connect_timeout = 177
}

resource "postgresql_role" "owner1" {
  name = "tf_tests_single_conn_reassign1"
}

resource "postgresql_role" "owner2" {
  name = "tf_tests_single_conn_reassign2"
}
`
	dbConfig := func(owner string) string {
		return providerConfig + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name                   = "tf_tests_single_conn_reassign_db"
  owner                  = postgresql_role.%s.name
  alter_object_ownership = true
}
`, owner)
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		ProviderFactories: singleConnProviderFactories,
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig("owner1"),
				Check:  testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
			},
			{
				Config: dbConfig("owner2"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", "tf_tests_single_conn_reassign2"),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabase_OwnerMaxConnectionsTwo creates and drops two
// databases with an owner concurrently with max_connections = 2: both
// operations compete for the same role advisory lock, and the holder must not
// need a connection the waiter is sitting on.
func TestAccPostgresqlDatabase_OwnerMaxConnectionsTwo(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)

	const providerConfig = `
provider "postgresql" {
  max_connections = 2
  connect_timeout = 176
}

resource "postgresql_role" "owner" {
  name = "tf_tests_two_conns_owner"
}
`
	dbConfig := providerConfig + `
resource "postgresql_database" "a" {
  name  = "tf_tests_two_conns_db_a"
  owner = postgresql_role.owner.name
}

resource "postgresql_database" "b" {
  name  = "tf_tests_two_conns_db_b"
  owner = postgresql_role.owner.name
}
`

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: singleConnProviderFactories,
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.a"),
					testAccCheckPostgresqlDatabaseExists("postgresql_database.b"),
					checkUserMembership(t, config.Username, "tf_tests_two_conns_owner", false),
				),
			},
			{
				Config: providerConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckDatabaseGone("tf_tests_two_conns_db_a"),
					testAccCheckDatabaseGone("tf_tests_two_conns_db_b"),
					checkUserMembership(t, config.Username, "tf_tests_two_conns_owner", false),
				),
			},
		},
	})
}

// testAccCheckDatabaseGone checks that a database dropped by a previous step
// no longer exists on the server.
func testAccCheckDatabaseGone(dbName string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		exists, err := checkDatabaseExists(testAccProvider.Meta().(*Client), dbName)
		if err != nil {
			return fmt.Errorf("error checking db %s", err)
		}
		if exists {
			return fmt.Errorf("database %q still exists after destroy", dbName)
		}
		return nil
	}
}

// TestAccPostgresqlDatabase_OwnerChange changes the owner of an existing
// database. ALTER DATABASE ... OWNER TO needs the provider role to be a member
// of both the current and the new owner, so a non-superuser provider role has
// to be granted both temporarily, and neither membership may be left behind.
func TestAccPostgresqlDatabase_OwnerChange(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)

	const rolesConfig = `
resource "postgresql_role" "owner1" {
  name = "tf_tests_owner_change_owner1"
}

resource "postgresql_role" "owner2" {
  name = "tf_tests_owner_change_owner2"
}
`
	dbConfig := func(owner string) string {
		return rolesConfig + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name  = "tf_tests_owner_change_db"
  owner = postgresql_role.%s.name
}
`, owner)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig("owner1"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", "tf_tests_owner_change_owner1"),
				),
			},
			{
				Config: dbConfig("owner2"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", "tf_tests_owner_change_owner2"),
					checkUserMembership(t, config.Username, "tf_tests_owner_change_owner1", false),
					checkUserMembership(t, config.Username, "tf_tests_owner_change_owner2", false),
				),
			},
			{
				// And back, so the previous owner is again a role the
				// provider is not a member of.
				Config: dbConfig("owner1"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", "tf_tests_owner_change_owner1"),
					checkUserMembership(t, config.Username, "tf_tests_owner_change_owner1", false),
					checkUserMembership(t, config.Username, "tf_tests_owner_change_owner2", false),
				),
			},
		},
	})
}

// A database whose name contains a single quote and a backslash must be
// created and dropped (dropping terminates its connections by name).
func TestAccPostgresqlDatabase_QuotedName(t *testing.T) {
	const dbName = `tf_tests_we'ird\db`

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "postgresql_database" "quoted" {
  name = %q
}
`, dbName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.quoted"),
					resource.TestCheckResourceAttr("postgresql_database.quoted", "name", dbName),
				),
			},
		},
	})
}
