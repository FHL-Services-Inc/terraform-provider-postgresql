package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

// TestAccPostgresqlRoleLocksSharedMembers applies resources that lock two roles
// sharing members in parallel: the owner role, which the app role is a member
// of, and the app role. On PostgreSQL 16+ a non-superuser with CREATEROLE is
// also a member of every role it creates, so the provider's own login role is
// a shared member as well. With unordered lock acquisition this failed with
// "deadlock detected" on almost every run of the rds profile.
func TestAccPostgresqlRoleLocksSharedMembers(t *testing.T) {
	skipIfNotAcc(t)

	owner := testUniqueName("tf_tests_lockorder_owner")
	app := testUniqueName("tf_tests_lockorder_app")
	dbName := testUniqueName("tf_tests_lockorder_db")

	config := fmt.Sprintf(`
resource "postgresql_role" "owner" {
  name = "%s"
}

resource "postgresql_role" "app" {
  name  = "%s"
  login = true

  lifecycle {
    ignore_changes = [roles]
  }
}

resource "postgresql_database" "db" {
  name  = "%s"
  owner = postgresql_role.owner.name
}

resource "postgresql_schema" "s" {
  database = postgresql_database.db.name
  name     = "s1"
  owner    = postgresql_role.owner.name
}

resource "postgresql_grant" "schema" {
  database    = postgresql_database.db.name
  role        = postgresql_role.app.name
  schema      = postgresql_schema.s.name
  object_type = "schema"
  privileges  = ["USAGE"]
}

resource "postgresql_grant" "tables" {
  database    = postgresql_database.db.name
  role        = postgresql_role.app.name
  schema      = postgresql_schema.s.name
  object_type = "table"
  privileges  = ["SELECT"]
}

resource "postgresql_default_privileges" "tables" {
  database    = postgresql_database.db.name
  role        = postgresql_role.app.name
  owner       = postgresql_role.owner.name
  schema      = postgresql_schema.s.name
  object_type = "table"
  privileges  = ["SELECT", "INSERT"]
}

resource "postgresql_grant_role" "member" {
  role       = postgresql_role.app.name
  grant_role = postgresql_role.owner.name
}
`, owner, app, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant_role.member", "grant_role", owner),
					resource.TestCheckResourceAttr("postgresql_default_privileges.tables", "privileges.#", "2"),
				),
			},
		},
	})
}
