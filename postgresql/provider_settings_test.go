package postgresql

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// TestAccPostgresqlProvider_SettingsPerConfiguration reconfigures the provider
// between steps with settings that are not part of the connection string. The
// pool to the provider database opened in the first step (or by an earlier
// test) is reused, but each step must run with its own settings: the second
// step sets database_username to a role that does not exist, so creating a
// database without an explicit owner has to fail with that role name.
func TestAccPostgresqlProvider_SettingsPerConfiguration(t *testing.T) {
	const (
		dbA         = "tf_tests_settings_a"
		dbB         = "tf_tests_settings_b"
		missingRole = "tf_tests_settings_no_such_role"
	)

	dbAConfig := `
resource "postgresql_database" "a" {
  name = "` + dbA + `"
}
`
	resource.Test(t, resource.TestCase{
		PreCheck: func() { testAccPreCheck(t) },
		// ProviderFactories (unlike Providers) lets each step carry its own
		// provider block.
		ProviderFactories: map[string]func() (*schema.Provider, error){
			"postgresql": func() (*schema.Provider, error) { return testAccProvider, nil },
		},
		CheckDestroy: testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: `provider "postgresql" {}` + dbAConfig,
				Check:  testAccCheckPostgresqlDatabaseExists("postgresql_database.a"),
			},
			{
				Config: `
provider "postgresql" {
  database_username = "` + missingRole + `"
}
` + dbAConfig + `
resource "postgresql_database" "b" {
  name = "` + dbB + `"
}
`,
				ExpectError: regexp.MustCompile(`role "` + missingRole + `" does not exist`),
			},
			{
				Config: `provider "postgresql" {}` + dbAConfig,
				Check:  testAccCheckPostgresqlDatabaseExists("postgresql_database.a"),
			},
		},
	})
}
