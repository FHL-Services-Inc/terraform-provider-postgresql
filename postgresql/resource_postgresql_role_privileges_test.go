package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// rolePrivilegesTestEnv creates a database with a role, two tables and a
// sequence in test_schema and returns the names plus a teardown.
func rolePrivilegesTestEnv(t *testing.T) (dbName, roleName string, teardown func()) {
	dbSuffix, teardownDB := setupTestDatabase(t, true, true)
	teardownTables := createTestTables(t, dbSuffix, []string{"test_schema.test_table", "test_schema.test_table2"}, "")
	teardownSeq := createTestSequences(t, dbSuffix, []string{"test_schema.test_seq"}, "")
	dbName, roleName = getTestDBNames(dbSuffix)
	return dbName, roleName, func() {
		teardownSeq()
		teardownTables()
		teardownDB()
	}
}

func testRolePrivilegesSQL(t *testing.T, dbName, query string) {
	config := getTestConfig(t)
	dbExecute(t, config.connStr(dbName), query)
}

// testCheckRoleHasPrivilege checks with has_<kind>_privilege (kind is "table",
// "sequence" or "schema") whether the role holds privilege on the object.
func testCheckRoleHasPrivilege(t *testing.T, dbName, roleName, kind, object, privilege string, want bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		config := getTestConfig(t)
		db, err := sql.Open("postgres", config.connStr(dbName))
		if err != nil {
			return err
		}
		defer closeDB(t, db)
		var has bool
		query := fmt.Sprintf("SELECT has_%s_privilege($1, $2, $3)", kind)
		if err := db.QueryRow(query, roleName, object, privilege).Scan(&has); err != nil {
			return fmt.Errorf("%s: %w", query, err)
		}
		if has != want {
			return fmt.Errorf("role %s has %s on %s %s: %v, want %v", roleName, privilege, kind, object, has, want)
		}
		return nil
	}
}

func TestAccPostgresqlRolePrivileges_basic(t *testing.T) {
	skipIfNotAcc(t)

	dbName, roleName, teardown := rolePrivilegesTestEnv(t)
	defer teardown()
	testConfig := getTestConfig(t)
	admin := testConfig.getDatabaseUsername()

	config := func(tables string) string {
		return fmt.Sprintf(`
resource "postgresql_role_privileges" "test" {
  database = %[1]q
  role     = %[2]q

  schema {
    name  = "test_schema"
    usage = true
%[3]s
    sequences {
      privileges = ["USAGE"]
    }
    default_privileges {
      owner       = %[4]q
      object_type = "table"
      privileges  = ["SELECT"]
    }
  }
}
`, dbName, roleName, tables, admin)
	}

	selectAll := `
    tables {
      privileges = ["SELECT"]
    }`
	selectAllInsertOne := selectAll + `
    tables {
      privileges = ["INSERT"]
      objects    = ["test_table"]
    }`

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		CheckDestroy: resource.ComposeTestCheckFunc(
			testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "SELECT", false),
			testCheckRoleHasPrivilege(t, dbName, roleName, "sequence", "test_schema.test_seq", "USAGE", false),
		),
		Steps: []resource.TestStep{
			{
				Config: config(selectAll),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "id", dbName+"/"+roleName),
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "SELECT", true),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table2", "SELECT", true),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "INSERT", false),
					testCheckRoleHasPrivilege(t, dbName, roleName, "sequence", "test_schema.test_seq", "USAGE", true),
					testCheckRoleHasPrivilege(t, dbName, roleName, "schema", "test_schema", "USAGE", true),
					testCheckRoleHasPrivilege(t, dbName, roleName, "schema", "test_schema", "CREATE", false),
				),
			},
			{
				Config: config(selectAllInsertOne),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "INSERT", true),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "SELECT", true),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table2", "INSERT", false),
				),
			},
			{
				// Removing the block revokes exactly what it granted.
				Config: config(selectAll),
				Check: resource.ComposeTestCheckFunc(
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "INSERT", false),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "SELECT", true),
				),
			},
		},
	})
}

func TestAccPostgresqlRolePrivileges_drift(t *testing.T) {
	skipIfNotAcc(t)

	dbName, roleName, teardown := rolePrivilegesTestEnv(t)
	defer teardown()

	config := fmt.Sprintf(`
resource "postgresql_role_privileges" "test" {
  database = %[1]q
  role     = %[2]q

  schema {
    name  = "test_schema"
    usage = true
    tables {
      privileges = ["SELECT"]
    }
  }
}
`, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
			},
			{
				// An out-of-band REVOKE shows up as drift naming the table, and
				// plans an update.
				PreConfig: func() {
					testRolePrivilegesSQL(t, dbName, fmt.Sprintf("REVOKE SELECT ON test_schema.test_table FROM %s", roleName))
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "1"),
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.0", "table test_schema.test_table: -SELECT"),
				),
			},
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table", "SELECT", true),
				),
			},
			{
				// An extra privilege on a covered object is drift too (exact set).
				PreConfig: func() {
					testRolePrivilegesSQL(t, dbName, fmt.Sprintf("GRANT INSERT ON test_schema.test_table2 TO %s", roleName))
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.0", "table test_schema.test_table2: +INSERT"),
			},
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
					testCheckRoleHasPrivilege(t, dbName, roleName, "table", "test_schema.test_table2", "INSERT", false),
				),
			},
		},
	})
}

func TestAccPostgresqlRolePrivileges_exclusive(t *testing.T) {
	skipIfNotAcc(t)

	dbName, roleName, teardown := rolePrivilegesTestEnv(t)
	defer teardown()

	config := func(exclusive bool) string {
		return fmt.Sprintf(`
resource "postgresql_role_privileges" "test" {
  database  = %[1]q
  role      = %[2]q
  exclusive = %[3]t

  schema {
    name  = "test_schema"
    usage = true
    tables {
      privileges = ["SELECT"]
    }
  }
}
`, dbName, roleName, exclusive)
	}

	grantSequence := func() {
		testRolePrivilegesSQL(t, dbName, fmt.Sprintf("GRANT USAGE ON SEQUENCE test_schema.test_seq TO %s", roleName))
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: config(false),
			},
			{
				// exclusive = false: a sequence the config does not mention is
				// not drift and stays granted.
				PreConfig:    grantSequence,
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
			},
			{
				Config: config(false),
				Check:  testCheckRoleHasPrivilege(t, dbName, roleName, "sequence", "test_schema.test_seq", "USAGE", true),
			},
			{
				// exclusive = true: it is drift, and the apply revokes it.
				Config:             config(true),
				ExpectNonEmptyPlan: false,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.#", "0"),
					testCheckRoleHasPrivilege(t, dbName, roleName, "sequence", "test_schema.test_seq", "USAGE", false),
				),
			},
			{
				PreConfig:          grantSequence,
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              resource.TestCheckResourceAttr("postgresql_role_privileges.test", "drift.0", "sequence test_schema.test_seq: +USAGE"),
			},
			{
				Config: config(true),
				Check:  testCheckRoleHasPrivilege(t, dbName, roleName, "sequence", "test_schema.test_seq", "USAGE", false),
			},
		},
	})
}

func TestAccPostgresqlRolePrivileges_import(t *testing.T) {
	skipIfNotAcc(t)

	dbName, roleName, teardown := rolePrivilegesTestEnv(t)
	defer teardown()
	testConfig := getTestConfig(t)
	admin := testConfig.getDatabaseUsername()

	// Written the way the importer lays the state out: explicit object lists,
	// one block per distinct privilege set (sorted by privileges), every schema the role has an ACL
	// entry in (setupTestDatabase grants USAGE on dev_schema too).
	config := fmt.Sprintf(`
resource "postgresql_role_privileges" "test" {
  database = %[1]q
  role     = %[2]q

  schema {
    name  = "dev_schema"
    usage = true
  }

  schema {
    name  = "test_schema"
    usage = true
    tables {
      privileges = ["INSERT", "SELECT"]
      objects    = ["test_table"]
    }
    tables {
      privileges = ["SELECT"]
      objects    = ["test_table2"]
    }
    sequences {
      privileges = ["USAGE"]
      objects    = ["test_seq"]
    }
    default_privileges {
      owner       = %[3]q
      object_type = "table"
      privileges  = ["SELECT"]
    }
  }
}
`, dbName, roleName, admin)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: config,
			},
			{
				ResourceName:      "postgresql_role_privileges.test",
				ImportState:       true,
				ImportStateId:     dbName + "/" + roleName,
				ImportStateVerify: true,
			},
		},
	})
}
