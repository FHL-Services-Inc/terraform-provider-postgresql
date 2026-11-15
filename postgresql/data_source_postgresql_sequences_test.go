package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccPostgresqlDataSourceSequences(t *testing.T) {
	skipIfNotAcc(t)

	// Create the database outside of resource.Test
	// because we need to create test schemas.
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	schemas := []string{"test_schema1", "test_schema2"}
	createTestSchemas(t, dbSuffix, schemas, "")

	testSequences := []string{"test_schema.test_sequence", "test_schema1.test_sequence1", "test_schema1.test_sequence2", "test_schema2.test_sequence1"}
	createTestSequences(t, dbSuffix, testSequences, "")

	dbName, _ := getTestDBNames(dbSuffix)

	testAccPostgresqlDataSourceSequencesDatabaseConfig := generateDataSourceSequencesConfig(dbName)

	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDataSourceSequencesDatabaseConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schema", "sequences.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schema", "sequences.0.object_name", "test_sequence"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schema", "sequences.0.schema_name", "test_schema"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas1and2", "sequences.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_like_all_sequence1", "sequences.#", "2"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_like_all_sequence1and2", "sequences.#", "0"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_like_any_sequence1and2", "sequences.#", "3"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_not_like_all_sequence1and2", "sequences.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_not_like_all_sequence1and2", "sequences.0.object_name", "test_sequence"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_regex_sequence1", "sequences.#", "2"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_combine_filtering", "sequences.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_combine_filtering", "sequences.0.object_name", "test_sequence2"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.test_schemas_combine_filtering", "sequences.0.schema_name", "test_schema1"),
				),
			},
		},
	})
}

func generateDataSourceSequencesConfig(dbName string) string {
	return fmt.Sprintf(`	
	data "postgresql_sequences" "test_schemas1and2" {
		database = "%[1]s"
		schemas = ["test_schema1","test_schema2"]
	}

	data "postgresql_sequences" "test_schema" {
		database = "%[1]s"
		schemas = ["test_schema"]
	}

	data "postgresql_sequences" "test_schemas_like_all_sequence1" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_all_patterns = ["test_sequence1"]
	}

	data "postgresql_sequences" "test_schemas_like_all_sequence1and2" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_all_patterns = ["test_sequence1","test_sequence2"]
	}

	data "postgresql_sequences" "test_schemas_like_any_sequence1and2" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_any_patterns = ["test_sequence1","test_sequence2"]
	}

	data "postgresql_sequences" "test_schemas_not_like_all_sequence1and2" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		not_like_all_patterns = ["test_sequence1","test_sequence2"]
	}

	data "postgresql_sequences" "test_schemas_regex_sequence1" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		regex_pattern = "^test_sequence1$"
	}

	data "postgresql_sequences" "test_schemas_combine_filtering" {
		database = "%[1]s"
		schemas = ["test_schema","test_schema1","test_schema2"]
		like_any_patterns= ["%%2%%"]
		not_like_all_patterns = ["%%1%%"]
		regex_pattern = "^test_.*$"
	}

	# test_basic's output won't be checked as it can return an indeterminate number of system sequences
	data "postgresql_sequences" "test_basic" {
		database = "%[1]s"
	}

	`, dbName)
}

// Identity column sequences and sequences owned by a role the provider user
// has no privileges on must be listed too.
func TestAccPostgresqlDataSourceSequences_identityAndOtherOwner(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, roleName := getTestDBNames(dbSuffix)
	config := getTestConfig(t)

	// The other owner creates its object itself; setupTestDatabase only grants USAGE.
	dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT CREATE ON SCHEMA dev_schema TO %s", roleName))
	dbExecute(t, config.connStr(dbName), "CREATE TABLE dev_schema.t (id int GENERATED ALWAYS AS IDENTITY, x serial)")
	createTestSequences(t, dbSuffix, []string{"dev_schema.plain_seq"}, "")
	createTestSequences(t, dbSuffix, []string{"dev_schema.other_seq"}, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
				data "postgresql_sequences" "all" {
					database = "%[1]s"
					schemas  = ["dev_schema"]
				}

				data "postgresql_sequences" "identity" {
					database          = "%[1]s"
					schemas           = ["dev_schema"]
					like_any_patterns = ["t_id%%"]
				}
				`, dbName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.#", "4"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.0.object_name", "other_seq"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.0.data_type", "bigint"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.1.object_name", "plain_seq"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.2.object_name", "t_id_seq"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.2.schema_name", "dev_schema"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.2.data_type", "integer"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.all", "sequences.3.object_name", "t_x_seq"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.identity", "sequences.#", "1"),
					resource.TestCheckResourceAttr("data.postgresql_sequences.identity", "sequences.0.object_name", "t_id_seq"),
				),
			},
		},
	})
}
