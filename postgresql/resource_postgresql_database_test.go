package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
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
// A database owner change with alter_object_ownership runs REASSIGN OWNED
// while grants on tables of the database run in the same apply. The grants
// must not take the membership of the previous owner and then find the tables
// reassigned to the new one ("permission denied for table", #181).
func TestAccPostgresqlDatabase_AlterObjectOwnershipConcurrentGrants(t *testing.T) {
	skipIfNotAcc(t)

	databaseSuffix := testUniqueName("reassign_grants")
	databaseName := fmt.Sprintf("%s_%s", dbNamePrefix, databaseSuffix)
	previousOwner := testUniqueName("previous_owner")
	newOwner := testUniqueName("new_owner")
	reader := testUniqueName("reader")
	const tableCount = 10
	var tables []string
	for i := range tableCount {
		tables = append(tables, fmt.Sprintf("t%d", i))
	}

	testConfig := getTestConfig(t)
	dsn := testConfig.connStr("postgres")
	for _, role := range []string{previousOwner, newOwner, reader} {
		dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", role))
		defer dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
	}

	config := func(owner string, grants bool) string {
		b := &strings.Builder{}
		fmt.Fprintf(b, `
resource "postgresql_database" "test_db" {
  name                   = "%s"
  owner                  = "%s"
  alter_object_ownership = true
}
`, databaseName, owner)
		if grants {
			// The database is a literal: the grants don't wait for the owner
			// change.
			for _, table := range tables {
				fmt.Fprintf(b, `
resource "postgresql_grant" "%s" {
  database    = "%s"
  role        = "%s"
  schema      = "public"
  object_type = "table"
  objects     = ["%s"]
  privileges  = ["SELECT"]
}
`, table, databaseName, reader, table)
			}
		}
		return b.String()
	}

	checkTables := func(*terraform.State) error {
		db, err := sql.Open("postgres", testConfig.connStr(databaseName))
		if err != nil {
			return err
		}
		defer closeDB(t, db)
		var owned, readable int
		if err := db.QueryRow(
			`SELECT count(*) FILTER (WHERE pg_get_userbyid(relowner) = $1),
			        count(*) FILTER (WHERE has_table_privilege($2, oid, 'SELECT'))
			   FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname = ANY($3)`,
			newOwner, reader, pq.Array(tables),
		).Scan(&owned, &readable); err != nil {
			return err
		}
		if owned != tableCount || readable != tableCount {
			return fmt.Errorf("%d of %d tables owned by %s, %d readable by %s", owned, tableCount, newOwner, readable, reader)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: config(previousOwner, false),
				Check: func(*terraform.State) error {
					_ = createTestTables(t, databaseSuffix, tables, previousOwner)
					return nil
				},
			},
			{
				Config: config(newOwner, true),
				Check:  checkTables,
			},
			// The grants don't depend on the database: remove them before the
			// final destroy drops it.
			{
				Config: config(newOwner, false),
			},
		},
	})
}

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

// TestAccPostgresqlDatabase_AlterObjectOwnershipOwnerChange changes the owner
// of a database holding a table owned by the previous owner, with
// alter_object_ownership. REASSIGN OWNED needs the provider role to have the
// privileges of both the old and the new owner (and, since PostgreSQL 16, to
// be able to SET ROLE to the new one), so a non-superuser provider role has to
// be granted both temporarily, and neither membership may be left behind.
// Runs in both profiles; the rds profile is the one that needs the grants.
func TestAccPostgresqlDatabase_AlterObjectOwnershipOwnerChange(t *testing.T) {
	skipIfNotAcc(t)

	databaseSuffix := testUniqueName("reassign_owner_change")
	const tableName = "reassign_owner_change_table"
	databaseName, _ := getTestDBNames(databaseSuffix)
	oldOwner := testUniqueName("tf_tests_reassign_old")
	newOwner := testUniqueName("tf_tests_reassign_new")

	config := getTestConfig(t)

	dbConfig := func(owner string) string {
		return fmt.Sprintf(`
resource "postgresql_role" "old" {
  name = %q
}

resource "postgresql_role" "new" {
  name = %q
}

resource "postgresql_database" "db" {
  name                   = %q
  owner                  = postgresql_role.%s.name
  alter_object_ownership = true
}
`, oldOwner, newOwner, databaseName, owner)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig("old"),
				Check: func(*terraform.State) error {
					// The table has to exist before the owner change.
					_ = createTestTables(t, databaseSuffix, []string{tableName}, oldOwner)
					return nil
				},
			},
			{
				Config: dbConfig("new"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", newOwner),
					checkTableOwnership(t, config.connStr(databaseName), newOwner, tableName),
					checkUserMembership(t, config.Username, oldOwner, false),
					checkUserMembership(t, config.Username, newOwner, false),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabase_AlterObjectOwnershipManyDatabases changes the
// owner of several databases at once with alter_object_ownership (#25). Each
// owner change temporarily grants the provider role membership in the old and
// the new owner; these memberships are cluster-wide, so the owner changes have
// to serialize on a lock that is cluster-wide too (the role lock on the
// maintenance database), not one taken in each target database. Without it a
// non-superuser provider fails with "duplicate key value violates unique
// constraint pg_auth_members_role_member_index" or "tuple concurrently
// updated".
func TestAccPostgresqlDatabase_AlterObjectOwnershipManyDatabases(t *testing.T) {
	skipIfNotAcc(t)

	const databases = 6
	const tableName = "multi_owner_table"
	suffixBase := testUniqueName("multi_owner")
	suffix := func(i int) string { return fmt.Sprintf("%s_%d", suffixBase, i) }
	oldOwner := testUniqueName("tf_tests_multi_owner_old")
	newOwner := testUniqueName("tf_tests_multi_owner_new")

	config := getTestConfig(t)

	dbConfig := func(owner string) string {
		return fmt.Sprintf(`
resource "postgresql_role" "old" {
  name = %q
}

resource "postgresql_role" "new" {
  name = %q
}

resource "postgresql_database" "db" {
  count                  = %d
  name                   = "%s_%s_${count.index}"
  owner                  = postgresql_role.%s.name
  alter_object_ownership = true
}
`, oldOwner, newOwner, databases, dbNamePrefix, suffixBase, owner)
	}

	newOwnerChecks := []resource.TestCheckFunc{
		checkUserMembership(t, config.Username, oldOwner, false),
		checkUserMembership(t, config.Username, newOwner, false),
	}
	for i := range databases {
		dbName, _ := getTestDBNames(suffix(i))
		newOwnerChecks = append(newOwnerChecks,
			resource.TestCheckResourceAttr(fmt.Sprintf("postgresql_database.db.%d", i), "owner", newOwner),
			checkTableOwnership(t, config.connStr(dbName), newOwner, tableName),
		)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig("old"),
				Check: func(*terraform.State) error {
					for i := range databases {
						_ = createTestTables(t, suffix(i), []string{tableName}, oldOwner)
					}
					return nil
				},
			},
			{
				Config: dbConfig("new"),
				Check:  resource.ComposeTestCheckFunc(newOwnerChecks...),
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
	owner1 := testUniqueName("tf_tests_single_conn_owner1")
	owner2 := testUniqueName("tf_tests_single_conn_owner2")
	dbName := testUniqueName("tf_tests_single_conn_db")

	providerConfig := fmt.Sprintf(`
provider "postgresql" {
  max_connections = 1
  connect_timeout = 178
}

resource "postgresql_role" "owner1" {
  name = %q
}

resource "postgresql_role" "owner2" {
  name = %q
}
`, owner1, owner2)
	dbConfig := func(owner string) string {
		return providerConfig + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name  = %q
  owner = postgresql_role.%s.name
}
`, dbName, owner)
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
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner1),
					checkUserMembership(t, config.Username, owner1, false),
				),
			},
			{
				Config: dbConfig("owner2"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner2),
					checkUserMembership(t, config.Username, owner2, false),
				),
			},
			{
				// Drop the database but keep the roles, so the membership
				// check still has something to look at.
				Config: providerConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckDatabaseGone(dbName),
					checkUserMembership(t, config.Username, owner2, false),
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

	owner1 := testUniqueName("tf_tests_single_conn_reassign1")
	owner2 := testUniqueName("tf_tests_single_conn_reassign2")
	dbName := testUniqueName("tf_tests_single_conn_reassign_db")

	providerConfig := fmt.Sprintf(`
provider "postgresql" {
  max_connections = 1
  connect_timeout = 177
}

resource "postgresql_role" "owner1" {
  name = %q
}

resource "postgresql_role" "owner2" {
  name = %q
}
`, owner1, owner2)
	dbConfig := func(owner string) string {
		return providerConfig + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name                   = %q
  owner                  = postgresql_role.%s.name
  alter_object_ownership = true
}
`, dbName, owner)
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
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner2),
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

	owner := testUniqueName("tf_tests_two_conns_owner")
	dbA := testUniqueName("tf_tests_two_conns_db_a")
	dbB := testUniqueName("tf_tests_two_conns_db_b")

	providerConfig := fmt.Sprintf(`
provider "postgresql" {
  max_connections = 2
  connect_timeout = 176
}

resource "postgresql_role" "owner" {
  name = %q
}
`, owner)
	dbConfig := providerConfig + fmt.Sprintf(`
resource "postgresql_database" "a" {
  name  = %q
  owner = postgresql_role.owner.name
}

resource "postgresql_database" "b" {
  name  = %q
  owner = postgresql_role.owner.name
}
`, dbA, dbB)

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
					checkUserMembership(t, config.Username, owner, false),
				),
			},
			{
				Config: providerConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckDatabaseGone(dbA),
					testAccCheckDatabaseGone(dbB),
					checkUserMembership(t, config.Username, owner, false),
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

	owner1 := testUniqueName("tf_tests_owner_change_owner1")
	owner2 := testUniqueName("tf_tests_owner_change_owner2")
	dbName := testUniqueName("tf_tests_owner_change_db")

	rolesConfig := fmt.Sprintf(`
resource "postgresql_role" "owner1" {
  name = %q
}

resource "postgresql_role" "owner2" {
  name = %q
}
`, owner1, owner2)
	dbConfig := func(owner string) string {
		return rolesConfig + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name  = %q
  owner = postgresql_role.%s.name
}
`, dbName, owner)
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
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner1),
				),
			},
			{
				Config: dbConfig("owner2"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner2),
					checkUserMembership(t, config.Username, owner1, false),
					checkUserMembership(t, config.Username, owner2, false),
				),
			},
			{
				// And back, so the previous owner is again a role the
				// provider is not a member of.
				Config: dbConfig("owner1"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner1),
					checkUserMembership(t, config.Username, owner1, false),
					checkUserMembership(t, config.Username, owner2, false),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabase_DropWithIdleConnections drops and recreates a
// database that the provider holds pooled connections to (because a schema
// lives in it) with max_idle_connections > 0. The pool for the dropped
// database must be evicted, otherwise the recreated database would be reached
// through stale, server-terminated connections.
func TestAccPostgresqlDatabase_DropWithIdleConnections(t *testing.T) {
	dbName := testUniqueName("tf_tests_idle_conns_db")

	// dbRegistry is process-wide and keyed by DSN only, so a pool opened by
	// an earlier test (with other pool settings) would be reused here.
	// A connect_timeout unique to this test gives the provider its own DSN,
	// hence its own pools.
	providerOnly := `
provider "postgresql" {
  max_idle_connections = 2
  connect_timeout      = 174
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
		dsn := client.config.connStr(dbName)

		dbRegistryLock.Lock()
		_, found := dbRegistry[dsn]
		dbRegistryLock.Unlock()

		if found != want {
			return fmt.Errorf("connection to database %q registered = %t, want %t", dbName, found, want)
		}
		return nil
	}
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

// TestAccPostgresqlDatabase_DefaultLocale creates a database with encoding,
// lc_collate, lc_ctype and tablespace_name set to "DEFAULT". Read stores the
// real values the server picked, so without diff suppression the next plan
// replaces the database (encoding/lc_* are ForceNew) and tries to update the
// tablespace. The test framework fails the step if the plan after apply is
// not empty; the second step re-plans the same config explicitly.
func TestAccPostgresqlDatabase_DefaultLocale(t *testing.T) {
	const dbName = "tf_tests_db_default_locale"
	config := fmt.Sprintf(`
resource "postgresql_database" "d" {
  name            = %q
  template        = "DEFAULT"
  encoding        = "DEFAULT"
  lc_collate      = "DEFAULT"
  lc_ctype        = "default"
  tablespace_name = "DEFAULT"
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.d"),
					// State keeps what the server actually picked.
					resource.TestCheckResourceAttrSet("postgresql_database.d", "encoding"),
					resource.TestCheckResourceAttrSet("postgresql_database.d", "lc_collate"),
					resource.TestCheckResourceAttrSet("postgresql_database.d", "lc_ctype"),
					resource.TestCheckResourceAttr("postgresql_database.d", "tablespace_name", "pg_default"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestSuppressDatabaseDefault(t *testing.T) {
	cases := []struct {
		old, new string
		want     bool
	}{
		{"UTF8", "DEFAULT", true},
		{"en_US.utf8", "default", true},
		{"", "DEFAULT", false}, // create: nothing to compare with yet
		{"UTF8", "LATIN1", false},
		{"UTF8", "UTF8", false}, // equal values have no diff to suppress
	}
	for _, c := range cases {
		if got := suppressDatabaseDefault("", c.old, c.new, nil); got != c.want {
			t.Errorf("suppressDatabaseDefault(%q, %q) = %v, want %v", c.old, c.new, got, c.want)
		}
	}

	tsCases := []struct {
		old, new string
		want     bool
	}{
		{"pg_default", "DEFAULT", true},
		{"pg_default", "default", true},
		{"", "DEFAULT", false},
		{"my_ts", "DEFAULT", false}, // a real move back to pg_default
		{"pg_default", "my_ts", false},
	}
	for _, c := range tsCases {
		if got := suppressDatabaseDefaultTablespace("", c.old, c.new, nil); got != c.want {
			t.Errorf("suppressDatabaseDefaultTablespace(%q, %q) = %v, want %v", c.old, c.new, got, c.want)
		}
	}
}
