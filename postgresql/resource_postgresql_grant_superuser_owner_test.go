package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// superuserOwnerFixture creates a test database with the schema s and the
// table s.t owned by a superuser (#114). A non-superuser provider cannot be
// granted a superuser role, so it can only grant on them directly, which
// needs the privileges WITH GRANT OPTION: without it PostgreSQL only raises a
// WARNING and grants nothing.
type superuserOwnerFixture struct {
	dbName, app, provider string
	suDSN                 string
}

func setupSuperuserOwnerFixture(t *testing.T, grantOption bool) (superuserOwnerFixture, func()) {
	t.Helper()
	config := getTestConfig(t)
	f := superuserOwnerFixture{provider: config.Username}

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	f.dbName, f.app = getTestDBNames(dbSuffix)
	f.suDSN = testFixtureSuperuserDSN(t, f.dbName)

	withGrantOption := ""
	if grantOption {
		withGrantOption = " WITH GRANT OPTION"
	}
	provider := pq.QuoteIdentifier(f.provider)
	for _, q := range []string{
		"CREATE SCHEMA s AUTHORIZATION CURRENT_USER",
		"CREATE TABLE s.t (a int)",
		"GRANT USAGE ON SCHEMA s TO " + provider + withGrantOption,
		"GRANT SELECT ON s.t TO " + provider + withGrantOption,
	} {
		dbExecute(t, f.suDSN, q)
	}

	return f, teardown
}

func (f superuserOwnerFixture) tfConfig() string {
	return fmt.Sprintf(`
resource "postgresql_grant" "schema" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "schema"
  privileges  = ["USAGE"]
}

resource "postgresql_grant" "table" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "table"
  objects     = ["t"]
  privileges  = ["SELECT"]
}
`, f.dbName, f.app)
}

func (f superuserOwnerFixture) checkSchemaUsage(t *testing.T, want bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := testQueryBool(t, f.suDSN, "SELECT has_schema_privilege($1, 's', 'USAGE')", f.app); got != want {
			return fmt.Errorf("has_schema_privilege(%s, s, USAGE) = %v, want %v", f.app, got, want)
		}
		return nil
	}
}

// TestAccPostgresqlGrant_SuperuserOwnerGrantOption: the objects belong to a
// superuser and the provider holds the privileges WITH GRANT OPTION, so it
// grants and revokes them directly (#114).
func TestAccPostgresqlGrant_SuperuserOwnerGrantOption(t *testing.T) {
	skipIfNotAcc(t)

	f, teardown := setupSuperuserOwnerFixture(t, true)
	defer teardown()

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: f.tfConfig(),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.schema", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.table", "privileges.#", "1"),
					f.checkSchemaUsage(t, true),
					testCheckTablePrivilege(t, f.suDSN, f.app, "s.t", "SELECT", true),
				),
			},
			{
				// A second plan must be empty.
				Config:   f.tfConfig(),
				PlanOnly: true,
			},
			{
				Config:  f.tfConfig(),
				Destroy: true,
				Check: resource.ComposeTestCheckFunc(
					f.checkSchemaUsage(t, false),
					testCheckTablePrivilege(t, f.suDSN, f.app, "s.t", "SELECT", false),
				),
			},
		},
	})
}

// TestAccPostgresqlGrant_SuperuserOwnerNoGrantOption: the objects belong to a
// superuser and the provider holds the privileges without GRANT OPTION. The
// GRANT would only raise a WARNING and change nothing, so the apply must fail
// and say what to do instead of succeeding silently (#114).
func TestAccPostgresqlGrant_SuperuserOwnerNoGrantOption(t *testing.T) {
	skipIfNotAcc(t)
	skipIfSuperuserProvider(t)

	f, teardown := setupSuperuserOwnerFixture(t, false)
	defer teardown()

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: f.tfConfig(),
				ExpectError: testWrappedErrorRegexp(
					"is a superuser and cannot be granted to "+pq.QuoteIdentifier(f.provider),
					"grant the managed privileges to "+pq.QuoteIdentifier(f.provider)+" WITH GRANT OPTION",
				),
			},
		},
	})
}
