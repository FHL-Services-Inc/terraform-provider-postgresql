package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// extensionMembersFixture creates a test database with the schema s owned by
// the provider, the table s.t and the function s.fn(integer) owned by the
// provider, and the extension
// pg_stat_statements installed in s by a superuser, so its views and
// functions belong to the superuser, as on Amazon RDS where they belong to
// rdsadmin.
type extensionMembersFixture struct {
	dbName, app, provider string
	suDSN                 string
}

func setupExtensionMembersFixture(t *testing.T) (extensionMembersFixture, func()) {
	t.Helper()
	config := getTestConfig(t)
	f := extensionMembersFixture{provider: config.Username}

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	f.dbName, f.app = getTestDBNames(dbSuffix)
	f.suDSN = testFixtureSuperuserDSN(t, f.dbName)

	provider := pq.QuoteIdentifier(f.provider)
	for _, q := range []string{
		"CREATE SCHEMA s AUTHORIZATION " + provider,
		"CREATE EXTENSION pg_stat_statements SCHEMA s",
		"CREATE TABLE s.t (a int)",
		"ALTER TABLE s.t OWNER TO " + provider,
		"CREATE FUNCTION s.fn(a integer) RETURNS integer LANGUAGE sql AS 'SELECT a'",
		"ALTER FUNCTION s.fn(integer) OWNER TO " + provider,
	} {
		dbExecute(t, f.suDSN, q)
	}

	return f, teardown
}

// checkGrantedOnView checks whether the ACL of s.pg_stat_statements holds
// SELECT for the test role itself (PUBLIC can read it by default).
func (f extensionMembersFixture) checkGrantedOnView(t *testing.T, want bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := testQueryBool(t, f.suDSN, `SELECT EXISTS (
  SELECT 1 FROM pg_class c, aclexplode(c.relacl) a
  WHERE c.oid = 's.pg_stat_statements'::regclass AND a.privilege_type = 'SELECT'
    AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1))`, f.app)
		if got != want {
			return fmt.Errorf("SELECT on s.pg_stat_statements granted to %s = %v, want %v", f.app, got, want)
		}
		return nil
	}
}

// checkGrantedOnFunction checks whether the ACL of s.fn(integer) holds EXECUTE
// for the test role itself (PUBLIC can execute it by default).
func (f extensionMembersFixture) checkGrantedOnFunction(t *testing.T, want bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := testQueryBool(t, f.suDSN, `SELECT EXISTS (
  SELECT 1 FROM pg_proc p, aclexplode(p.proacl) a
  WHERE p.oid = 's.fn(integer)'::regprocedure AND a.privilege_type = 'EXECUTE'
    AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1))`, f.app)
		if got != want {
			return fmt.Errorf("EXECUTE on s.fn(integer) granted to %s = %v, want %v", f.app, got, want)
		}
		return nil
	}
}

func (f extensionMembersFixture) tfConfig() string {
	return fmt.Sprintf(`
resource "postgresql_grant" "tables" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "table"
  objects     = []
  privileges  = ["SELECT"]
}

resource "postgresql_grant" "functions" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "function"
  objects     = []
  privileges  = ["EXECUTE"]
}
`, f.dbName, f.app)
}

func (f extensionMembersFixture) tfConfigListed() string {
	return fmt.Sprintf(`
resource "postgresql_grant" "listed" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "table"
  objects     = ["pg_stat_statements"]
  privileges  = ["SELECT"]
}
`, f.dbName, f.app)
}

// TestAccPostgresqlGrant_ExtensionMembers: grants on all tables and all
// functions of a schema leave the members of an extension out. With a
// non-superuser provider they belong to a superuser it cannot act as, and the
// apply used to fail ("is a superuser and cannot be granted"); a superuser
// provider could grant on them, but they are not the schema's own objects
// either way. A second plan must be empty: no drift is reported for them.
func TestAccPostgresqlGrant_ExtensionMembers(t *testing.T) {
	skipIfNotAcc(t)

	f, teardown := setupExtensionMembersFixture(t)
	defer teardown()

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			testCheckCompatibleVersion(t, featureExtension)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: f.tfConfig(),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.tables", "privileges.#", "1"),
					resource.TestCheckTypeSetElemAttr("postgresql_grant.tables", "privileges.*", "SELECT"),
					resource.TestCheckResourceAttr("postgresql_grant.functions", "privileges.#", "1"),
					resource.TestCheckTypeSetElemAttr("postgresql_grant.functions", "privileges.*", "EXECUTE"),
					testCheckTablePrivilege(t, f.suDSN, f.app, "s.t", "SELECT", true),
					f.checkGrantedOnFunction(t, true),
				),
			},
			{
				Config:   f.tfConfig(),
				PlanOnly: true,
			},
			{
				Config:  f.tfConfig(),
				Destroy: true,
				Check: resource.ComposeTestCheckFunc(
					testCheckTablePrivilege(t, f.suDSN, f.app, "s.t", "SELECT", false),
					f.checkGrantedOnFunction(t, false),
				),
			},
		},
	})
}

// TestAccPostgresqlGrant_ExtensionMemberListed: an extension member listed in
// objects is managed like any other object. Its owner is a superuser, so a
// non-superuser provider without the privilege WITH GRANT OPTION cannot grant
// on it and the apply fails, as for any superuser-owned object (#114).
func TestAccPostgresqlGrant_ExtensionMemberListed(t *testing.T) {
	skipIfNotAcc(t)
	skipIfSuperuserProvider(t)

	f, teardown := setupExtensionMembersFixture(t)
	defer teardown()

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			testCheckCompatibleVersion(t, featureExtension)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: f.tfConfigListed(),
				ExpectError: testWrappedErrorRegexp(
					"is a superuser and cannot be granted to "+pq.QuoteIdentifier(f.provider),
					"grant the managed privileges to "+pq.QuoteIdentifier(f.provider)+" WITH GRANT OPTION",
				),
			},
		},
	})
}

// TestAccPostgresqlGrant_ExtensionMemberListedSuperuser: a superuser provider
// grants on a listed extension member and reads it back.
func TestAccPostgresqlGrant_ExtensionMemberListedSuperuser(t *testing.T) {
	skipIfNotAcc(t)
	skipIfNotSuperuser(t)

	f, teardown := setupExtensionMembersFixture(t)
	defer teardown()

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			testCheckCompatibleVersion(t, featureExtension)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: f.tfConfigListed(),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.listed", "privileges.#", "1"),
					// PUBLIC can read the view anyway: look for the role's
					// own ACL entry.
					f.checkGrantedOnView(t, true),
				),
			},
			{
				Config:   f.tfConfigListed(),
				PlanOnly: true,
			},
		},
	})
}
