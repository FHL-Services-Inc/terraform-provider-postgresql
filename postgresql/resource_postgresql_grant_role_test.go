package postgresql

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func TestCreateGrantRoleQuery(t *testing.T) {
	var roleName = "foo"
	var grantRoleName = "bar"

	cases := []struct {
		resource map[string]any
		expected string
	}{
		{
			resource: map[string]any{
				"role":       roleName,
				"grant_role": grantRoleName,
			},
			expected: fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(grantRoleName), pq.QuoteIdentifier(roleName)),
		},
		{
			resource: map[string]any{
				"role":              roleName,
				"grant_role":        grantRoleName,
				"with_admin_option": false,
			},
			expected: fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(grantRoleName), pq.QuoteIdentifier(roleName)),
		},
		{
			resource: map[string]any{
				"role":              roleName,
				"grant_role":        grantRoleName,
				"with_admin_option": true,
			},
			expected: fmt.Sprintf("GRANT %s TO %s WITH ADMIN OPTION", pq.QuoteIdentifier(grantRoleName), pq.QuoteIdentifier(roleName)),
		},
	}

	for _, c := range cases {
		out := createGrantRoleQuery(schema.TestResourceDataRaw(t, resourcePostgreSQLGrantRole().Schema, c.resource))
		if out != c.expected {
			t.Fatalf("error matching output and expected: %#v vs %#v", out, c.expected)
		}
	}
}

// TestGetGrantRoleQuery locks in the OID-join form of the Read query. The
// membership must be filtered by joining pg_auth_members to pg_roles on the
// member/roleid OIDs, NOT by calling pg_get_userbyid() in the WHERE clause
// (which forces a sequential function scan on large installations). This guards
// against an accidental regression to the function-scan form. On PG 16+ the
// pair can have one row per grantor, so the query must prefer the row granted
// by current_user (#138). A superuser's grants are recorded with the bootstrap
// superuser (OID 10) as grantor, so for a superuser that row is preferred
// instead (#151).
func TestGetGrantRoleQuery(t *testing.T) {
	expected := `
SELECT
  ur.rolname as role,
  gr.rolname as grant_role,
  m.admin_option
FROM
  pg_auth_members m
  JOIN pg_roles ur ON ur.oid = m.member
  JOIN pg_roles gr ON gr.oid = m.roleid
WHERE
  ur.rolname = $1 AND
  gr.rolname = $2
ORDER BY
  (m.grantor = (
    SELECT CASE WHEN rolsuper THEN 10::oid ELSE oid END
    FROM pg_roles WHERE rolname = current_user
  )) DESC,
  m.grantor
LIMIT 1;
`

	if strings.Join(strings.Fields(getGrantRoleQuery), " ") != strings.Join(strings.Fields(expected), " ") {
		t.Fatalf("getGrantRoleQuery changed unexpectedly:\n got: %q\nwant: %q", getGrantRoleQuery, expected)
	}

	if strings.Contains(getGrantRoleQuery, "pg_get_userbyid") {
		t.Fatalf("getGrantRoleQuery must not call pg_get_userbyid (regression to function scan): %q", getGrantRoleQuery)
	}
}

func TestRevokeRoleQuery(t *testing.T) {
	var roleName = "foo"
	var grantRoleName = "bar"

	expected := fmt.Sprintf("REVOKE %s FROM %s", pq.QuoteIdentifier(grantRoleName), pq.QuoteIdentifier(roleName))

	cases := []struct {
		resource map[string]any
	}{
		{
			resource: map[string]any{
				"role":       roleName,
				"grant_role": grantRoleName,
			},
		},
		{
			resource: map[string]any{
				"role":              roleName,
				"grant_role":        grantRoleName,
				"with_admin_option": false,
			},
		},
		{
			resource: map[string]any{
				"role":              roleName,
				"grant_role":        grantRoleName,
				"with_admin_option": true,
			},
		},
	}

	for _, c := range cases {
		out := createRevokeRoleQuery(schema.TestResourceDataRaw(t, resourcePostgreSQLGrantRole().Schema, c.resource))
		if out != expected {
			t.Fatalf("error matching output and expected: %#v vs %#v", out, expected)
		}
	}
}

func TestAccPostgresqlGrantRole(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()

	_, roleName := getTestDBNames(dbSuffix)

	grantedRoleName := testUniqueName("foo")

	testAccPostgresqlGrantRoleResources := fmt.Sprintf(`
	resource postgresql_role "grant" {
		name = "%s"
	}
	resource postgresql_grant_role "grant_role" {
		role              = "%s"
		grant_role        = postgresql_role.grant.name
		with_admin_option = true
	}
	`, grantedRoleName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlGrantRoleResources,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "role", roleName),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "grant_role", grantedRoleName),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant_role", "with_admin_option", strconv.FormatBool(true)),
					checkGrantRole(t, dsn, roleName, grantedRoleName, true),
				),
			},
		},
	})
}

// Since PostgreSQL 16 a role created by a CREATEROLE non-superuser is granted
// to its creator automatically (WITH ADMIN, grantor = bootstrap superuser), so
// granting that role to the provider user leaves two pg_auth_members rows for
// the pair. The read must report the provider's own grant, not the implicit
// one, or with_admin_option flips to true and the resource is replaced right
// after create (#138).
func TestAccPostgresqlGrantRoleToProviderUser(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	providerUser := config.getDatabaseUsername()
	grantedRoleName := testUniqueName("test_grant_role_self")

	resourceConfig := fmt.Sprintf(`
	resource postgresql_role "grp" {
		name = "%s"
	}
	resource postgresql_grant_role "self" {
		role       = "%s"
		grant_role = postgresql_role.grp.name
	}
	`, grantedRoleName, providerUser)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: resourceConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.self", "role", providerUser),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.self", "grant_role", grantedRoleName),
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.self", "with_admin_option", strconv.FormatBool(false)),
				),
			},
			{
				// The plan right after apply must be empty.
				Config:   resourceConfig,
				PlanOnly: true,
			},
		},
	})
}

// On PostgreSQL 16+ a grant made by any superuser is recorded with the
// bootstrap superuser (OID 10) as grantor, not with current_user. With a
// superuser provider that is not the bootstrap superuser, the read must still
// pick the row the provider created when the pair also has a row from another
// grantor, here one WITH ADMIN OPTION granted by a CREATEROLE role (#151).
func TestAccPostgresqlGrantRoleNonBootstrapSuperuser(t *testing.T) {
	skipIfNotAcc(t)
	skipIfNotSuperuser(t)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")

	var versionNum int
	func() {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("could not create connection pool: %v", err)
		}
		defer closeDB(t, db)
		if err := db.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&versionNum); err != nil {
			t.Fatalf("could not read server version: %v", err)
		}
	}()
	if versionNum < 160000 {
		t.Skip("one membership row per grantor exists only on PostgreSQL 16+")
	}

	admin := testUniqueName("tf_tests_superuser")
	other := testUniqueName("tf_tests_grantor")
	grp := testUniqueName("tf_tests_grp")
	member := testUniqueName("tf_tests_member")
	const adminPassword = "admin"

	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s LOGIN SUPERUSER PASSWORD %s",
		pq.QuoteIdentifier(admin), pq.QuoteLiteral(adminPassword)))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s CREATEROLE", pq.QuoteIdentifier(other)))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", pq.QuoteIdentifier(grp)))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", pq.QuoteIdentifier(member)))
	defer func() {
		for _, role := range []string{member, grp, other, admin} {
			dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(role)))
		}
	}()
	dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s WITH ADMIN OPTION",
		pq.QuoteIdentifier(grp), pq.QuoteIdentifier(other)))
	// A second row for (member, grp), granted by other WITH ADMIN OPTION.
	dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s WITH ADMIN OPTION GRANTED BY %s",
		pq.QuoteIdentifier(grp), pq.QuoteIdentifier(member), pq.QuoteIdentifier(other)))

	resourceConfig := fmt.Sprintf(`
provider "postgresql" {
  username  = %q
  password  = %q
  superuser = true
}

resource "postgresql_grant_role" "grant" {
  role       = %q
  grant_role = %q
}
`, admin, adminPassword, member, grp)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProviderFactories: map[string]func() (*schema.Provider, error){
			"postgresql": func() (*schema.Provider, error) { return Provider(), nil },
		},
		Steps: []resource.TestStep{
			{
				Config: resourceConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(
						"postgresql_grant_role.grant", "with_admin_option", strconv.FormatBool(false)),
					checkGrantRole(t, dsn, member, grp, false),
				),
			},
			{
				// The plan right after apply must be empty.
				Config:   resourceConfig,
				PlanOnly: true,
			},
		},
	})
}

func checkGrantRole(t *testing.T, dsn, role string, grantRole string, withAdmin bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("could to create connection pool: %v", err)
		}
		defer closeDB(t, db)

		var _rez int
		err = db.QueryRow(`
		SELECT 1
		FROM pg_auth_members
		WHERE pg_get_userbyid(member) = $1
		AND pg_get_userbyid(roleid) = $2
		AND admin_option = $3;
		`, role, grantRole, withAdmin).Scan(&_rez)

		switch {
		case err == sql.ErrNoRows:
			return fmt.Errorf(
				"Role %s is not a member of %s",
				role, grantRole,
			)

		case err != nil:
			t.Fatalf("could not check granted role: %v", err)
		}

		return nil
	}
}
