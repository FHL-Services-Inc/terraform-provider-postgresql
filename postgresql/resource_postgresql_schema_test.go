package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPostgresqlSchema_Basic(t *testing.T) {
	roleWithoutGrant := testUniqueName("role_all_without_grant")
	roleWithGrant := testUniqueName("role_all_with_grant")
	schemaFoo := testUniqueName("foo")
	schemaBar := testUniqueName("bar")
	schemaBaz := testUniqueName("baz")

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSchemaDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSchemaConfig(roleWithoutGrant, roleWithGrant, schemaFoo, schemaBar, schemaBaz),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.test1", schemaFoo),
					resource.TestCheckResourceAttr("postgresql_role.role_all_without_grant", "name", roleWithoutGrant),
					resource.TestCheckResourceAttr("postgresql_role.role_all_without_grant", "login", "true"),

					resource.TestCheckResourceAttr("postgresql_role.role_all_with_grant", "name", roleWithGrant),

					resource.TestCheckResourceAttr("postgresql_schema.test1", "name", schemaFoo),

					resource.TestCheckResourceAttr("postgresql_schema.test2", "name", schemaBar),
					resource.TestCheckResourceAttr("postgresql_schema.test2", "owner", roleWithoutGrant),
					resource.TestCheckResourceAttr("postgresql_schema.test2", "if_not_exists", "false"),
					resource.TestCheckResourceAttr("postgresql_schema.test2", "policy.#", "1"),
					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test2", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"usage":             "true",
						"usage_with_grant":  "false",
						"role":              roleWithoutGrant,
					}),

					resource.TestCheckResourceAttr("postgresql_schema.test3", "name", schemaBaz),
					resource.TestCheckResourceAttr("postgresql_schema.test3", "owner", roleWithoutGrant),
					resource.TestCheckResourceAttr("postgresql_schema.test3", "if_not_exists", "true"),
					resource.TestCheckResourceAttr("postgresql_schema.test3", "policy.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test3", "policy.*", map[string]string{
						"create_with_grant": "true",
						"usage_with_grant":  "true",
						"role":              roleWithGrant,
					}),
					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test3", "policy.*", map[string]string{
						"create": "true",
						"usage":  "true",
						"role":   roleWithoutGrant,
					}),
				),
			},
		},
	})
}

func TestAccPostgresqlSchema_AddPolicy(t *testing.T) {
	n := schemaPolicyNames{
		schema:    testUniqueName("test4"),
		stay:      testUniqueName("all_without_grant_stay"),
		drop:      testUniqueName("all_without_grant_drop"),
		compose:   testUniqueName("policy_compose"),
		move:      testUniqueName("policy_move"),
		grantStay: testUniqueName("all_with_grantstay"),
		grantDrop: testUniqueName("all_with_grantdrop"),
		policyNew: testUniqueName("policy_new"),
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			// TODO: Need to check if removing policy is buggy
			// because non-superuser fails to drop a role
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSchemaDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSchemaGrant1(n),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.test4", n.schema),

					resource.TestCheckResourceAttr("postgresql_role.all_without_grant_stay", "name", n.stay),
					resource.TestCheckResourceAttr("postgresql_role.all_without_grant_drop", "name", n.drop),
					resource.TestCheckResourceAttr("postgresql_role.policy_compose", "name", n.compose),
					resource.TestCheckResourceAttr("postgresql_role.policy_move", "name", n.move),

					resource.TestCheckResourceAttr("postgresql_role.all_with_grantstay", "name", n.grantStay),
					resource.TestCheckResourceAttr("postgresql_role.all_with_grantdrop", "name", n.grantDrop),

					resource.TestCheckResourceAttr("postgresql_schema.test4", "name", n.schema),
					resource.TestCheckResourceAttr("postgresql_schema.test4", "owner", n.stay),

					resource.TestCheckResourceAttr("postgresql_schema.test4", "policy.#", "7"),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "false",
						"create_with_grant": "true",
						"role":              n.grantDrop,
						"usage":             "false",
						"usage_with_grant":  "true",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "false",
						"create_with_grant": "true",
						"role":              n.grantStay,
						"usage":             "false",
						"usage_with_grant":  "true",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "false",
						"create_with_grant": "true",
						"role":              n.compose,
						"usage":             "false",
						"usage_with_grant":  "true",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.drop,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.stay,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.compose,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.move,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),
				),
			},
			{
				Config: testAccPostgresqlSchemaGrant2(n),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.test4", n.schema),
					resource.TestCheckResourceAttr("postgresql_role.all_without_grant_stay", "name", n.stay),
					resource.TestCheckResourceAttr("postgresql_role.all_without_grant_drop", "name", n.drop),
					resource.TestCheckResourceAttr("postgresql_role.policy_compose", "name", n.compose),
					resource.TestCheckResourceAttr("postgresql_role.policy_move", "name", n.move),

					resource.TestCheckResourceAttr("postgresql_role.all_with_grantstay", "name", n.grantStay),

					resource.TestCheckResourceAttr("postgresql_schema.test4", "name", n.schema),
					resource.TestCheckResourceAttr("postgresql_schema.test4", "owner", n.stay),

					resource.TestCheckResourceAttr("postgresql_schema.test4", "policy.#", "6"),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "false",
						"create_with_grant": "true",
						"role":              n.grantStay,
						"usage":             "false",
						"usage_with_grant":  "true",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "false",
						"create_with_grant": "true",
						"role":              n.compose,
						"usage":             "false",
						"usage_with_grant":  "true",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "false",
						"create_with_grant": "true",
						"role":              n.move,
						"usage":             "false",
						"usage_with_grant":  "true",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.stay,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.compose,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),

					resource.TestCheckTypeSetElemNestedAttrs("postgresql_schema.test4", "policy.*", map[string]string{
						"create":            "true",
						"create_with_grant": "false",
						"role":              n.policyNew,
						"usage":             "true",
						"usage_with_grant":  "false",
					}),
				),
			},
		},
	})
}

func TestAccPostgresqlSchema_Database(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	testAccPostgresqlSchemaDatabaseConfig := fmt.Sprintf(`
	resource "postgresql_schema" "test_database" {
		name     = "test_database"
		database = "%s"
	}
	`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSchemaDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSchemaDatabaseConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.test_database", "test_database"),
					resource.TestCheckResourceAttr(
						"postgresql_schema.test_database", "name", "test_database"),
					resource.TestCheckResourceAttr(
						"postgresql_schema.test_database", "database", dbName),
				),
			},
		},
	})
}

func TestAccPostgresqlSchema_DropCascade(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	var testAccPostgresqlSchemaConfig = fmt.Sprintf(`
resource "postgresql_schema" "test_cascade" {
  name = "foo"
  database = "%s"
  drop_cascade = true
}
`, dbName)
	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSchemaDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSchemaConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.test_cascade", "foo"),
					resource.TestCheckResourceAttr("postgresql_schema.test_cascade", "name", "foo"),

					// This will create a table in the schema to check if the drop will work thanks to the cascade
					testAccCreateSchemaTable(dbName, "foo"),
				),
			},
		},
	})
}

func TestAccPostgresqlSchema_AlreadyExists(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, roleName := getTestDBNames(dbSuffix)

	// Test to create the schema 'public' that already exists
	// to assert it does not fail.
	var testAccPostgresqlSchemaConfig = fmt.Sprintf(`
resource "postgresql_schema" "public" {
  name = "public"
  database = "%s"
  owner = "%s"
}
`, dbName, roleName)
	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSchemaDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSchemaConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.public", "public"),
					testAccCheckSchemaOwner(dbName, "public", roleName),
				),
			},
		},
	})
}

func testAccCheckPostgresqlSchemaDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_schema" {
			continue
		}

		database, ok := rs.Primary.Attributes[schemaDatabaseAttr]
		if !ok {
			return fmt.Errorf("No Attribute for database is set")
		}

		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkSchemaExists(txn, getExtensionNameFromID(rs.Primary.ID))

		if err != nil {
			return fmt.Errorf("error checking schema %s", err)
		}

		if exists {
			return fmt.Errorf("Schema still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlSchemaExists(n string, schemaName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		database, ok := rs.Primary.Attributes[schemaDatabaseAttr]
		if !ok {
			return fmt.Errorf("No Attribute for database is set")
		}

		actualSchemaName := rs.Primary.Attributes["name"]
		if actualSchemaName != schemaName {
			return fmt.Errorf("Wrong value for schema name expected %s got %s", schemaName, actualSchemaName)
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkSchemaExists(txn, schemaName)

		if err != nil {
			return fmt.Errorf("error checking schema %s", err)
		}

		if !exists {
			return fmt.Errorf("Schema not found")
		}

		return nil
	}
}

func checkSchemaExists(txn *sql.Tx, schemaName string) (bool, error) {
	var _rez bool
	err := txn.QueryRow("SELECT TRUE FROM pg_catalog.pg_namespace WHERE nspname=$1", schemaName).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about schema: %w", err)
	}

	return true, nil
}

func testAccCreateSchemaTable(database, schemaName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {

		client := testAccProvider.Meta().(*Client).config.NewClient(database)
		db, err := client.Connect()
		if err != nil {
			return err
		}

		if _, err = db.Exec(fmt.Sprintf("CREATE TABLE %s.test_table (id serial)", schemaName)); err != nil {
			return fmt.Errorf("could not create test table in schema %s: %s", schemaName, err)
		}

		return nil
	}
}

func testAccCheckSchemaOwner(database, schemaName, expectedOwner string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Client).config.NewClient(database)
		db, err := client.Connect()
		if err != nil {
			return err
		}

		var owner string

		query := "SELECT pg_catalog.pg_get_userbyid(n.nspowner)  FROM pg_catalog.pg_namespace n WHERE n.nspname=$1"
		switch err := db.QueryRow(query, schemaName).Scan(&owner); {
		case err == sql.ErrNoRows:
			return fmt.Errorf("could not find schema %s while checking owner", schemaName)
		case err != nil:
			return fmt.Errorf("error reading owner of schema %s: %w", schemaName, err)
		}

		if owner != expectedOwner {
			return fmt.Errorf("expected owner of schema %s to be %s; got %s", schemaName, expectedOwner, owner)
		}

		return nil
	}
}

func testAccPostgresqlSchemaConfig(roleWithoutGrant, roleWithGrant, schemaFoo, schemaBar, schemaBaz string) string {
	return fmt.Sprintf(`
resource "postgresql_role" "role_all_without_grant" {
  name = "%[1]s"
  login = true
}

resource "postgresql_role" "role_all_with_grant" {
  name = "%[2]s"
}

resource "postgresql_schema" "test1" {
  name = "%[3]s"
}

resource "postgresql_schema" "test2" {
  name = "%[4]s"
  owner = "${postgresql_role.role_all_without_grant.name}"
  if_not_exists = false

  policy {
    create = true
    usage = true
    role = "${postgresql_role.role_all_without_grant.name}"
  }
}

resource "postgresql_schema" "test3" {
  name = "%[5]s"
  owner = "${postgresql_role.role_all_without_grant.name}"
  if_not_exists = true

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.role_all_with_grant.name}"
  }

  policy {
    create = true
    usage = true
    role = "${postgresql_role.role_all_without_grant.name}"
  }
}
`, roleWithoutGrant, roleWithGrant, schemaFoo, schemaBar, schemaBaz)
}

// schemaPolicyNames holds the unique role and schema names of
// TestAccPostgresqlSchema_AddPolicy.
type schemaPolicyNames struct {
	schema, stay, drop, compose, move, grantStay, grantDrop, policyNew string
}

func testAccPostgresqlSchemaGrant1(n schemaPolicyNames) string {
	return fmt.Sprintf(`
resource "postgresql_role" "all_without_grant_stay" {
  name = "%[2]s"
}

resource "postgresql_role" "all_without_grant_drop" {
  name = "%[3]s"
}

resource "postgresql_role" "policy_compose" {
  name = "%[4]s"
}

resource "postgresql_role" "policy_move" {
  name = "%[5]s"
}

resource "postgresql_role" "all_with_grantstay" {
  name = "%[6]s"
}

resource "postgresql_role" "all_with_grantdrop" {
  name = "%[7]s"
}

resource "postgresql_schema" "test4" {
  name = "%[1]s"
  owner = "${postgresql_role.all_without_grant_stay.name}"

  policy {
    create = true
    usage = true
    role = "${postgresql_role.all_without_grant_stay.name}"
  }

  policy {
    create = true
    usage = true
    role = "${postgresql_role.all_without_grant_drop.name}"
  }

  policy {
    create = true
    usage = true
    role = "${postgresql_role.policy_compose.name}"
  }

  policy {
    create = true
    usage = true
    role = "${postgresql_role.policy_move.name}"
  }

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.all_with_grantstay.name}"
  }

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.all_with_grantdrop.name}"
  }

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.policy_compose.name}"
  }
}
`, n.schema, n.stay, n.drop, n.compose, n.move, n.grantStay, n.grantDrop, n.policyNew)
}

func testAccPostgresqlSchemaGrant2(n schemaPolicyNames) string {
	return fmt.Sprintf(`
resource "postgresql_role" "all_without_grant_stay" {
  name = "%[2]s"
}

resource "postgresql_role" "all_without_grant_drop" {
  name = "%[3]s"
}

resource "postgresql_role" "policy_compose" {
  name = "%[4]s"
}

resource "postgresql_role" "policy_move" {
  name = "%[5]s"
}

resource "postgresql_role" "all_with_grantstay" {
  name = "%[6]s"
}

resource "postgresql_role" "policy_new" {
  name = "%[8]s"
}

resource "postgresql_schema" "test4" {
  name = "%[1]s"
  owner = "${postgresql_role.all_without_grant_stay.name}"

  policy {
    create = true
    usage = true
    role = "${postgresql_role.all_without_grant_stay.name}"
  }

  policy {
    create = true
    usage = true
    role = "${postgresql_role.policy_compose.name}"
  }

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.all_with_grantstay.name}"
  }

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.policy_compose.name}"
  }

  policy {
    create_with_grant = true
    usage_with_grant = true
    role = "${postgresql_role.policy_move.name}"
  }

  policy {
    create = true
    usage = true
    role = "${postgresql_role.policy_new.name}"
  }
}
`, n.schema, n.stay, n.drop, n.compose, n.move, n.grantStay, n.grantDrop, n.policyNew)
}

// TestAccPostgresqlSchema_DatabaseDropped ensures that when the database holding
// a schema is dropped out-of-band, the read prunes the resource from state
// instead of failing the whole refresh with `database "..." does not exist`
// (SQLSTATE 3D000). Removing the Exists callback moved gone-detection entirely
// into Read, which connects to the target database and therefore has to treat a
// missing database as "resource gone" rather than an error.
func TestAccPostgresqlSchema_DatabaseDropped(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	config := fmt.Sprintf(`
resource "postgresql_schema" "test" {
	name     = "test_dropped_db_schema"
	database = "%s"
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSchemaDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSchemaExists("postgresql_schema.test", "test_dropped_db_schema"),
					testAccDropDatabase(t, dbName),
				),
				// The database (and its schema) is gone out-of-band: the following
				// refresh must succeed and plan to recreate the resource instead of
				// erroring, which is what this change fixes.
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// testAccDropDatabase drops a database out-of-band (see dropTestDatabase).
func testAccDropDatabase(t *testing.T, dbName string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		dropTestDatabase(t, getTestConfig(t), dbName)
		return nil
	}
}
