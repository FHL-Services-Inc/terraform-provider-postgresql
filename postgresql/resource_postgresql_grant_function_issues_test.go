package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// serverVersionNum returns server_version_num of the server behind dsn.
func serverVersionNum(t *testing.T, dsn string) int {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("could not create connection pool: %v", err)
	}
	defer closeDB(t, db)
	var v int
	if err := db.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("could not read server version: %v", err)
	}
	return v
}

// TestAccPostgresqlGrantFunctionSignatureRevokes is the reproduction from
// #63: an out-of-band revoke on a signature entry ("f(integer)") and a
// function revoke hidden by a same-name procedure grant were not detected.
func TestAccPostgresqlGrantFunctionSignatureRevokes(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)
	dsn := config.connStr(dbName)

	for _, stmt := range []string{
		"ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC",
		"CREATE FUNCTION test_schema.f(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.f(text) RETURNS int LANGUAGE sql AS 'SELECT 2'",
		"CREATE FUNCTION test_schema.g() RETURNS int LANGUAGE sql AS 'SELECT 1'",
	} {
		dbExecute(t, dsn, stmt)
	}

	role := pq.QuoteIdentifier(roleName)
	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "sig" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["f(integer)"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_grant" "fn_only" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["g"]
	privileges  = ["EXECUTE"]
}
`, dbName, roleName)

	exec := func(stmts ...string) func() {
		return func() {
			for _, stmt := range stmts {
				dbExecute(t, dsn, stmt)
			}
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			testCheckCompatibleVersion(t, featureProcedure)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{Config: tfConfig},
			{
				// Revoke on the signature entry.
				PreConfig:          exec(fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.f(integer) FROM %s", role)),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// The other overload of f is not part of the grant.
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO %s", role),
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(text) TO %s", role),
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
			{
				// Revoke on the function g(), with a procedure g(int) granted:
				// the procedure is not covered by a function grant and does
				// not hide the revoke.
				PreConfig: exec(
					"CREATE PROCEDURE test_schema.g(int) LANGUAGE sql AS 'SELECT 1'",
					fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.g(int) TO %s", role),
					fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.g() FROM %s", role),
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Restore; "g" must name a single routine again for the
				// destroy.
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.g() TO %s", role),
					"DROP PROCEDURE test_schema.g(int)",
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
		},
	})
}

// TestAccPostgresqlGrantFunctionArgNamesAndModesRevokes is the reproduction
// from #78: entries with argument names ("f(x integer)") or modes
// ("p(IN a text, OUT b integer)"), as GRANT accepts them, did not detect
// out-of-band revokes. OUT arguments are not part of a routine's identity.
func TestAccPostgresqlGrantFunctionArgNamesAndModesRevokes(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)
	dsn := config.connStr(dbName)

	// Procedures with OUT arguments need PostgreSQL 14.
	if serverVersionNum(t, dsn) < 140000 {
		t.Skip("procedures with OUT arguments need PostgreSQL 14+")
	}

	for _, stmt := range []string{
		"ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC",
		"CREATE FUNCTION test_schema.f(x integer) RETURNS int LANGUAGE sql AS 'SELECT x'",
		"CREATE PROCEDURE test_schema.p(IN a text, OUT b integer) LANGUAGE sql AS 'SELECT 1'",
	} {
		dbExecute(t, dsn, stmt)
	}

	role := pq.QuoteIdentifier(roleName)
	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "named" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["f(x integer)"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_grant" "modes" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "procedure"
	objects     = ["p(IN a text, OUT b integer)"]
	privileges  = ["EXECUTE"]
}
`, dbName, roleName)

	exec := func(stmts ...string) func() {
		return func() {
			for _, stmt := range stmts {
				dbExecute(t, dsn, stmt)
			}
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			testCheckCompatibleVersion(t, featureProcedure)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{Config: tfConfig},
			{
				PreConfig:          exec(fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.f(integer) FROM %s", role)),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO %s", role),
					fmt.Sprintf("REVOKE EXECUTE ON PROCEDURE test_schema.p(text) FROM %s", role),
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Both revoked, as in the issue; apply grants them again.
				PreConfig: exec(fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.f(integer) FROM %s", role)),
				Config:    tfConfig,
				Check: func(*terraform.State) error {
					db, err := sql.Open("postgres", dsn)
					if err != nil {
						return err
					}
					defer closeDB(t, db)
					var ok bool
					if err := db.QueryRow(
						"SELECT has_function_privilege($1, 'test_schema.f(integer)', 'EXECUTE') AND has_function_privilege($1, 'test_schema.p(text)', 'EXECUTE')",
						roleName,
					).Scan(&ok); err != nil {
						return err
					}
					if !ok {
						return fmt.Errorf("EXECUTE on f(integer) and p(text) not granted again to %s", roleName)
					}
					return nil
				},
			},
		},
	})
}
