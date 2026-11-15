package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccPostgresqlDataSourceTables(t *testing.T) {
	skipIfNotAcc(t)

	// Create the database outside of resource.Test
	// because we need to create test schemas.
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	schemas := []string{"test_schema1", "test_schema2"}
	createTestSchemas(t, dbSuffix, schemas, "")

	testTables := []string{"test_schema.test_table", "test_schema1.test_table1", "test_schema1.test_table2", "test_schema2.test_table1"}
	createTestTables(t, dbSuffix, testTables, "")

	dbName, _ := getTestDBNames(dbSuffix)

	testAccPostgresqlDataSourceTablesDatabaseConfig := generateDataSourceTablesConfig(dbName)

	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDataSourceTablesDatabaseConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schema", "tables.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schema", "tables.0.object_name", "test_table"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schema", "tables.0.schema_name", "test_schema"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schema", "tables.0.table_type", "BASE TABLE"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas1and2", "tables.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas1and2_type_base", "tables.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas1and2_type_other", "tables.#", "0"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas1and2_type_base_and_other", "tables.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_like_all_table1", "tables.#", "2"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_like_all_table1and2", "tables.#", "0"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_like_any_table1and2", "tables.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_not_like_all_table1and2", "tables.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_not_like_all_table1and2", "tables.0.object_name", "test_table"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_regex_table1", "tables.#", "2"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_combine_filtering", "tables.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_combine_filtering", "tables.0.object_name", "test_table2"),
					resource.TestCheckResourceAttr("data.postgresql_tables.test_schemas_combine_filtering", "tables.0.schema_name", "test_schema1"),
				),
			},
		},
	})
}

func generateDataSourceTablesConfig(dbName string) string {
	return fmt.Sprintf(`	
	data "postgresql_tables" "test_schemas1and2" {
		database = "%[1]s"
		schemas = ["test_schema1","test_schema2"]
	}

	data "postgresql_tables" "test_schema" {
		database = "%[1]s"
		schemas = ["test_schema"]
	}

	data "postgresql_tables" "test_schemas1and2_type_base" {
		database = "%[1]s"
		schemas = ["test_schema1","test_schema2"]
		table_types = ["BASE TABLE"]
	}

	data "postgresql_tables" "test_schemas1and2_type_other" {
		database = "%[1]s"
		schemas = ["test_schema1","test_schema2"]
		table_types = ["VIEW","LOCAL TEMPORARY"]
	}

	data "postgresql_tables" "test_schemas1and2_type_base_and_other" {
		database = "%[1]s"
		schemas = ["test_schema1","test_schema2"]
		table_types = ["VIEW","LOCAL TEMPORARY","BASE TABLE"]
	}

	data "postgresql_tables" "test_schemas_like_all_table1" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_all_patterns = ["test_table1"]
	}

	data "postgresql_tables" "test_schemas_like_all_table1and2" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_all_patterns = ["test_table1","test_table2"]
	}

	data "postgresql_tables" "test_schemas_like_any_table1and2" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_any_patterns = ["test_table1","test_table2"]
	}

	data "postgresql_tables" "test_schemas_not_like_all_table1and2" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		not_like_all_patterns = ["test_table1","test_table2"]
	}

	data "postgresql_tables" "test_schemas_regex_table1" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		regex_pattern = "^test_table1$"
	}

	data "postgresql_tables" "test_schemas_combine_filtering" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_any_patterns= ["%%2%%"]
		not_like_all_patterns = ["%%1%%"]
		regex_pattern = "^test_.*$"
	}

	# test_basic's output won't be checked as it can return an indeterminate number of system tables
	data "postgresql_tables" "test_basic" {
		database = "%[1]s"
	}

	`, dbName)
}

// Tables owned by a role the provider user has no privileges on, or is not a
// member of, must be listed too (the rds profile is the relevant one: a real
// superuser sees everything in information_schema anyway).
func TestAccPostgresqlDataSourceTables_otherOwner(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, roleName := getTestDBNames(dbSuffix)
	config := getTestConfig(t)

	// The other owner creates its object itself; setupTestDatabase only grants USAGE.
	dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT CREATE ON SCHEMA dev_schema TO %s", roleName))
	createTestTables(t, dbSuffix, []string{"dev_schema.app_mine"}, "")
	createTestTables(t, dbSuffix, []string{"dev_schema.app_users"}, roleName)
	dbExecute(t, config.connStr(dbName), "CREATE VIEW dev_schema.app_view AS SELECT 1 AS one")
	dbExecute(t, config.connStr(dbName), "CREATE MATERIALIZED VIEW dev_schema.app_matview AS SELECT 1 AS one")

	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
				data "postgresql_tables" "all" {
					database = "%[1]s"
					schemas  = ["dev_schema"]
				}

				data "postgresql_tables" "views" {
					database    = "%[1]s"
					schemas     = ["dev_schema"]
					table_types = ["VIEW"]
				}
				`, dbName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.0.object_name", "app_mine"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.0.table_type", "BASE TABLE"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.1.object_name", "app_users"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.1.schema_name", "dev_schema"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.1.table_type", "BASE TABLE"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.2.object_name", "app_view"),
					resource.TestCheckResourceAttr("data.postgresql_tables.all", "tables.2.table_type", "VIEW"),
					resource.TestCheckResourceAttr("data.postgresql_tables.views", "tables.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_tables.views", "tables.0.object_name", "app_view"),
				),
			},
		},
	})
}
