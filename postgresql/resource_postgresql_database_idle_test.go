package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// Tests in this file run the provider with max_idle_connections > 0, where the
// provider keeps idle connections to the databases it touched. Each test sets
// a connect_timeout no other test uses: dbRegistry is process-wide and keyed by
// DSN, and a pool keeps the pool settings (max_idle_connections) of the
// configuration that opened it, so a pool opened earlier with
// max_idle_connections = 0 would be reused as is.

func idleProviderFactories() map[string]func() (*schema.Provider, error) {
	return map[string]func() (*schema.Provider, error){
		"postgresql": func() (*schema.Provider, error) { return testAccProvider, nil },
	}
}

// TestAccPostgresqlDatabase_RenameWithIdleConnections renames a database the
// provider holds idle pooled connections to. ALTER DATABASE ... RENAME fails
// with "database is being accessed by other users" unless the pool to the old
// name is evicted first.
func TestAccPostgresqlDatabase_RenameWithIdleConnections(t *testing.T) {
	const (
		oldName = "tf_tests_idle_rename_old"
		newName = "tf_tests_idle_rename_new"
	)

	providerBlock := `
provider "postgresql" {
  max_idle_connections = 2
  connect_timeout      = 170
}
`
	dbOnly := func(name string) string {
		return providerBlock + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name = "%s"
}
`, name)
	}
	withSchema := dbOnly(oldName) + `
resource "postgresql_schema" "schema" {
  name     = "idle_rename"
  database = postgresql_database.db.name
}
`

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: idleProviderFactories(),
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				// The schema makes the provider open a pool to the database.
				Config: withSchema,
				Check:  testAccCheckDBConnectionRegistered(oldName, true),
			},
			{
				// Removing the schema still refreshes it first, so the pool to
				// the database holds idle connections after this step.
				Config: dbOnly(oldName),
				Check:  testAccCheckDBConnectionRegistered(oldName, true),
			},
			{
				Config: dbOnly(newName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "name", newName),
					testAccCheckDBConnectionRegistered(oldName, false),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabase_TemplateWithIdleConnections creates a database
// from a managed database the provider holds idle pooled connections to.
// CREATE DATABASE ... TEMPLATE fails with "source database is being accessed
// by other users" unless the template's pool is evicted first.
func TestAccPostgresqlDatabase_TemplateWithIdleConnections(t *testing.T) {
	const (
		srcName = "tf_tests_idle_tpl_src"
		dstName = "tf_tests_idle_tpl_dst"
	)

	src := fmt.Sprintf(`
provider "postgresql" {
  max_idle_connections = 2
  connect_timeout      = 171
}

resource "postgresql_database" "src" {
  name = "%s"
}

resource "postgresql_schema" "schema" {
  name     = "idle_template"
  database = postgresql_database.src.name
}
`, srcName)
	withCopy := src + fmt.Sprintf(`
resource "postgresql_database" "dst" {
  name     = "%s"
  template = postgresql_database.src.name

  depends_on = [postgresql_schema.schema]
}
`, dstName)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: idleProviderFactories(),
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: src,
				Check:  testAccCheckDBConnectionRegistered(srcName, true),
			},
			{
				// The refresh of the schema leaves idle connections to src.
				Config: withCopy,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.dst"),
					resource.TestCheckResourceAttr("postgresql_database.dst", "template", srcName),
				),
			},
		},
	})
}
