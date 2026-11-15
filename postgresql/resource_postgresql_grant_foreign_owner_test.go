package postgresql

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// testFixtureSuperuserDSN returns a DSN for dbName as a real superuser, to set
// up objects the provider user could not create itself (e.g. roles it did not
// create). In the rds profile it is the bootstrap superuser of the test server
// (PGFIXTURE_SUPERUSER / PGFIXTURE_SUPERUSER_PASSWORD, postgres/postgres by
// default, as in tests/ and CI); the test is skipped if it cannot connect.
func testFixtureSuperuserDSN(t *testing.T, dbName string) string {
	t.Helper()
	config := getTestConfig(t)
	if os.Getenv("PGSUPERUSER") != "false" {
		return config.connStr(dbName)
	}

	config.Username = os.Getenv("PGFIXTURE_SUPERUSER")
	if config.Username == "" {
		config.Username = "postgres"
	}
	config.Password = os.Getenv("PGFIXTURE_SUPERUSER_PASSWORD")
	if config.Password == "" {
		config.Password = "postgres"
	}
	dsn := config.connStr(dbName)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("could not create connection pool: %v", err)
	}
	defer closeDB(t, db)
	var superuser bool
	if err := db.QueryRow("SELECT rolsuper FROM pg_roles WHERE rolname = CURRENT_USER").Scan(&superuser); err != nil || !superuser {
		t.Skipf("needs a superuser connection for the fixtures (PGFIXTURE_SUPERUSER): superuser=%v, err=%v", superuser, err)
	}
	return dsn
}

// testQueryBool runs a query returning one boolean.
func testQueryBool(t *testing.T, dsn, query string, args ...any) bool {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("could not create connection pool: %v", err)
	}
	defer closeDB(t, db)
	var b bool
	if err := db.QueryRow(query, args...).Scan(&b); err != nil {
		t.Fatalf("could not run %s: %v", query, err)
	}
	return b
}

func testCheckTablePrivilege(t *testing.T, dsn, role, table, privilege string, want bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := testQueryBool(t, dsn, "SELECT has_table_privilege($1, $2, $3)", role, table, privilege); got != want {
			return fmt.Errorf("has_table_privilege(%s, %s, %s) = %v, want %v", role, table, privilege, got, want)
		}
		return nil
	}
}

func testCheckNotMemberOf(t *testing.T, dsn, member, role string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if testQueryBool(t, dsn, "SELECT pg_has_role($1, $2, 'MEMBER')", member, role) {
			return fmt.Errorf("%s is left a member of %s", member, role)
		}
		return nil
	}
}

// testWrappedErrorRegexp matches the phrases, in order, in an error message
// Terraform may have wrapped at any space.
func testWrappedErrorRegexp(phrases ...string) *regexp.Regexp {
	parts := make([]string, 0, len(phrases))
	for _, phrase := range phrases {
		words := strings.Fields(phrase)
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		parts = append(parts, strings.Join(words, `\s+`))
	}
	return regexp.MustCompile(strings.Join(parts, `(?s:.*)`))
}

func skipIfSuperuserProvider(t *testing.T) {
	if os.Getenv("PGSUPERUSER") != "false" {
		t.Skip("only meaningful for a non-superuser provider (rds profile)")
	}
}

func skipBeforePG16(t *testing.T) {
	client := testAccProvider.Meta().(*Client)
	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect: %v", err)
	}
	if !db.featureSupported(featureAuthMembersSetOption) {
		t.Skipf("PostgreSQL %s lets a CREATEROLE provider grant any role", db.version)
	}
}

// foreignOwnerFixture creates a test database whose objects belong to a role
// created by a superuser, not by the provider user (#132): the table
// public.t, the schema s and the table s.t2.
type foreignOwnerFixture struct {
	dbName, app, owner, provider string
	suDSN                        string
}

func setupForeignOwnerFixture(t *testing.T, grantOption bool) (foreignOwnerFixture, func()) {
	t.Helper()
	config := getTestConfig(t)
	f := foreignOwnerFixture{provider: config.Username, owner: testUniqueName("tf_tests_foreign_owner")}

	suPostgres := testFixtureSuperuserDSN(t, "postgres")
	dbExecute(t, suPostgres, fmt.Sprintf("CREATE ROLE %s", pq.QuoteIdentifier(f.owner)))
	dropOwner := func() { dbExecute(t, suPostgres, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(f.owner))) }

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	f.dbName, f.app = getTestDBNames(dbSuffix)
	f.suDSN = testFixtureSuperuserDSN(t, f.dbName)

	owner, provider := pq.QuoteIdentifier(f.owner), pq.QuoteIdentifier(f.provider)
	for _, q := range []string{
		"CREATE TABLE public.t (a int)",
		"ALTER TABLE public.t OWNER TO " + owner,
		"CREATE SCHEMA s AUTHORIZATION " + owner,
		"CREATE TABLE s.t2 (a int)",
		"ALTER TABLE s.t2 OWNER TO " + owner,
	} {
		dbExecute(t, f.suDSN, q)
	}
	if grantOption {
		dbExecute(t, f.suDSN, fmt.Sprintf("SET ROLE %s; GRANT SELECT ON public.t, s.t2 TO %s WITH GRANT OPTION; GRANT USAGE ON SCHEMA s TO %s WITH GRANT OPTION", owner, provider, provider))
	}

	return f, func() {
		teardown()
		dropOwner()
	}
}

func (f foreignOwnerFixture) tfConfig() string {
	return fmt.Sprintf(`
resource "postgresql_grant" "table" {
  database    = %[1]q
  role        = %[2]q
  schema      = "public"
  object_type = "table"
  objects     = ["t"]
  privileges  = ["SELECT"]
}

resource "postgresql_grant" "schema" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "schema"
  privileges  = ["USAGE"]
}

resource "postgresql_grant" "schema_table" {
  database    = %[1]q
  role        = %[2]q
  schema      = "s"
  object_type = "table"
  objects     = ["t2"]
  privileges  = ["SELECT"]
}
`, f.dbName, f.app)
}

// TestAccPostgresqlGrant_ForeignOwnerGrantOption: on PostgreSQL 16+ a
// non-superuser provider cannot take membership in a role it did not create,
// but it holds the privileges WITH GRANT OPTION, so it grants (and revokes)
// them directly (#132). Up to 15 it still goes through the membership.
func TestAccPostgresqlGrant_ForeignOwnerGrantOption(t *testing.T) {
	skipIfNotAcc(t)

	f, teardown := setupForeignOwnerFixture(t, true)
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
					resource.TestCheckResourceAttr("postgresql_grant.table", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.schema", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.schema_table", "privileges.#", "1"),
					testCheckTablePrivilege(t, f.suDSN, f.app, "public.t", "SELECT", true),
					testCheckTablePrivilege(t, f.suDSN, f.app, "s.t2", "SELECT", true),
					func(*terraform.State) error {
						if !testQueryBool(t, f.suDSN, "SELECT has_schema_privilege($1, 's', 'USAGE')", f.app) {
							return fmt.Errorf("%s has no USAGE on schema s", f.app)
						}
						return nil
					},
					testCheckNotMemberOf(t, f.suDSN, f.provider, f.owner),
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
					testCheckTablePrivilege(t, f.suDSN, f.app, "public.t", "SELECT", false),
					testCheckTablePrivilege(t, f.suDSN, f.app, "s.t2", "SELECT", false),
					testCheckNotMemberOf(t, f.suDSN, f.provider, f.owner),
				),
			},
		},
	})
}

// TestAccPostgresqlGrant_ForeignOwnerNoGrantOption: without ADMIN OPTION on
// the owner nor GRANT OPTION on the objects, the error says why and what to
// do instead of "permission denied to grant role" (#132).
func TestAccPostgresqlGrant_ForeignOwnerNoGrantOption(t *testing.T) {
	skipIfNotAcc(t)
	skipIfSuperuserProvider(t)

	f, teardown := setupForeignOwnerFixture(t, false)
	defer teardown()

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			skipBeforePG16(t)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config:      f.tfConfig(),
				ExpectError: testWrappedErrorRegexp("cannot act as "+pq.QuoteIdentifier(f.owner), "WITH GRANT OPTION"),
			},
		},
	})
}

// TestAccPostgresqlGrant_IndirectReverseMembership: the owner of the table is
// a member of the provider user through another role (#133), so the provider
// cannot become a member of the owner (circular membership).
func TestAccPostgresqlGrant_IndirectReverseMembership(t *testing.T) {
	skipIfNotAcc(t)
	skipIfSuperuserProvider(t)

	for _, tc := range []struct {
		name        string
		grantOption bool
	}{
		{"with grant option", true},
		{"without grant option", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := getTestConfig(t)
			provider := config.Username
			a, b := testUniqueName("tf_tests_chain_a"), testUniqueName("tf_tests_chain_b")

			suPostgres := testFixtureSuperuserDSN(t, "postgres")
			for _, q := range []string{
				"CREATE ROLE " + pq.QuoteIdentifier(b),
				"CREATE ROLE " + pq.QuoteIdentifier(a),
				fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(provider), pq.QuoteIdentifier(b)),
				fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(b), pq.QuoteIdentifier(a)),
			} {
				dbExecute(t, suPostgres, q)
			}
			defer dbExecute(t, suPostgres, fmt.Sprintf("DROP ROLE IF EXISTS %s, %s", pq.QuoteIdentifier(a), pq.QuoteIdentifier(b)))

			dbSuffix, teardown := setupTestDatabase(t, true, true)
			defer teardown()
			dbName, app := getTestDBNames(dbSuffix)
			suDSN := testFixtureSuperuserDSN(t, dbName)
			dbExecute(t, suDSN, "CREATE TABLE public.t (a int)")
			dbExecute(t, suDSN, "ALTER TABLE public.t OWNER TO "+pq.QuoteIdentifier(a))
			if tc.grantOption {
				dbExecute(t, suDSN, fmt.Sprintf("SET ROLE %s; GRANT SELECT ON public.t TO %s WITH GRANT OPTION", pq.QuoteIdentifier(a), pq.QuoteIdentifier(provider)))
			}

			tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "test" {
  database    = %q
  role        = %q
  schema      = "public"
  object_type = "table"
  objects     = ["t"]
  privileges  = ["SELECT"]
}
`, dbName, app)

			chainIntact := func(*terraform.State) error {
				if !testQueryBool(t, suDSN, "SELECT pg_has_role($1, $2, 'MEMBER') AND pg_has_role($2, $3, 'MEMBER')", a, b, provider) {
					return fmt.Errorf("membership chain %s -> %s -> %s changed", a, b, provider)
				}
				return nil
			}

			var steps []resource.TestStep
			if tc.grantOption {
				steps = []resource.TestStep{
					{
						Config: tfConfig,
						Check: resource.ComposeTestCheckFunc(
							testCheckTablePrivilege(t, suDSN, app, "public.t", "SELECT", true),
							chainIntact,
						),
					},
					{Config: tfConfig, PlanOnly: true},
					{
						Config:  tfConfig,
						Destroy: true,
						Check: resource.ComposeTestCheckFunc(
							testCheckTablePrivilege(t, suDSN, app, "public.t", "SELECT", false),
							chainIntact,
						),
					},
				}
			} else {
				steps = []resource.TestStep{{
					Config:      tfConfig,
					ExpectError: testWrappedErrorRegexp(fmt.Sprintf("through %s -> %s -> %s", a, b, provider)),
				}}
			}

			resource.Test(t, resource.TestCase{
				PreCheck: func() {
					testAccPreCheck(t)
					testCheckCompatibleVersion(t, featurePrivileges)
				},
				Providers: testAccProviders,
				Steps:     steps,
			})
		})
	}
}
