package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/lib/pq"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPostgresqlDefaultPrivileges(t *testing.T) {
	skipIfNotAcc(t)

	// We have to create the database outside of resource.Test
	// because we need to create a table to assert that grant are correctly applied
	// and we don't have this resource yet
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	// Set default privileges to the test role then to public (i.e.: everyone)
	for _, role := range []string{roleName, "public"} {
		t.Run(role, func(t *testing.T) {
			withGrant := (role != "public")

			// We set PGUSER as owner as he will create the test table
			var tfConfig = fmt.Sprintf(`
resource "postgresql_default_privileges" "test_ro" {
	database    = "%s"
	owner       = "%s"
	role        = "%s"
	schema      = "test_schema"
	object_type = "table"
	with_grant_option = %t
	privileges   = %%s
}
	`, dbName, config.Username, role, withGrant)

			resource.Test(t, resource.TestCase{
				PreCheck: func() {
					testAccPreCheck(t)
					testCheckCompatibleVersion(t, featurePrivileges)
				},
				Providers: testAccProviders,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(tfConfig, `[]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								tables := []string{"test_schema.test_table"}
								// To test default privileges, we need to create a table
								// after having apply the state.
								dropFunc := createTestTables(t, dbSuffix, tables, "")
								defer dropFunc()

								return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "table"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "with_grant_option", fmt.Sprintf("%t", withGrant)),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "0"),
						),
					},
					{
						Config: fmt.Sprintf(tfConfig, `["SELECT"]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								tables := []string{"test_schema.test_table"}
								// To test default privileges, we need to create a table
								// after having apply the state.
								dropFunc := createTestTables(t, dbSuffix, tables, "")
								defer dropFunc()

								return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{"SELECT"})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "table"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "with_grant_option", fmt.Sprintf("%t", withGrant)),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "1"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "SELECT"),
						),
					},
					{
						Config: fmt.Sprintf(tfConfig, `["SELECT", "UPDATE"]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								tables := []string{"test_schema.test_table"}
								// To test default privileges, we need to create a table
								// after having apply the state.
								dropFunc := createTestTables(t, dbSuffix, tables, "")
								defer dropFunc()

								return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{"SELECT", "UPDATE"})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "2"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "SELECT"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.1", "UPDATE"),
						),
					},
					{
						Config: fmt.Sprintf(tfConfig, `[]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								tables := []string{"test_schema.test_table"}
								// To test default privileges, we need to create a table
								// after having apply the state.
								dropFunc := createTestTables(t, dbSuffix, tables, "")
								defer dropFunc()

								return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "table"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "with_grant_option", fmt.Sprintf("%t", withGrant)),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "0"),
						),
					},
				},
			})
		})
	}
}

// Test the case where we need to grant the owner to the connected user.
// The owner should be revoked
func TestAccPostgresqlDefaultPrivileges_GrantOwner(t *testing.T) {
	skipIfNotAcc(t)

	// We have to create the database outside of resource.Test
	// because we need to create a table to assert that grant are correctly applied
	// and we don't have this resource yet
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)
	ownerName := testUniqueName("test_owner")

	// We set PGUSER as owner as he will create the test table
	var stateConfig = fmt.Sprintf(`

resource postgresql_role "test_owner" {
    name = "%s"
}

// From PostgreSQL 15, schema public is not wild open anymore
resource "postgresql_grant" "public_usage" {
	database          = "%s"
	schema            = "public"
	role              = postgresql_role.test_owner.name
	object_type       = "schema"
	privileges        = ["CREATE", "USAGE"]
}

resource "postgresql_default_privileges" "test_ro" {
	database    = "%s"
	owner       = postgresql_role.test_owner.name
	role        = "%s"
	schema      = "public"
	object_type = "table"
	privileges  = ["SELECT"]
}
	`, ownerName, dbName, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: stateConfig,
				Check: resource.ComposeTestCheckFunc(
					func(*terraform.State) error {
						tables := []string{"public.test_table"}
						// To test default privileges, we need to create a table
						// after having apply the state.
						dropFunc := createTestTables(t, dbSuffix, tables, ownerName)
						defer dropFunc()

						return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{"SELECT"})
					},
					resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "table"),
					resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "SELECT"),

					// check if connected user does not have test_owner granted anymore.
					checkUserMembership(t, config.Username, ownerName, false),
				),
			},
		},
	})
}

// Test the case where we define default privileges without specifying a schema. These
// privileges should apply to newly created resources for the named role in all schema.
func TestAccPostgresqlDefaultPrivileges_NoSchema(t *testing.T) {
	skipIfNotAcc(t)

	// We have to create the database outside of resource.Test
	// because we need to create a table to assert that grant are correctly applied
	// and we don't have this resource yet
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	// Set default privileges to the test role then to public (i.e.: everyone)
	for _, role := range []string{roleName, "public"} {
		t.Run(role, func(t *testing.T) {

			hclText := `
resource "postgresql_default_privileges" "test_ro" {
	database    = "%s"
	owner       = "%s"
	role        = "%s"
	object_type = "table"
	privileges  = %%s
}
`
			// We set PGUSER as owner as he will create the test table
			var tfConfig = fmt.Sprintf(hclText, dbName, config.Username, role)

			resource.Test(t, resource.TestCase{
				PreCheck: func() {
					testAccPreCheck(t)
					testCheckCompatibleVersion(t, featurePrivileges)
				},
				Providers: testAccProviders,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(tfConfig, `["SELECT"]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								tables := []string{"test_schema.test_table", "dev_schema.test_table"}
								// To test default privileges, we need to create tables
								// in both dev and test schema after having applied the state.
								dropFunc := createTestTables(t, dbSuffix, tables, "")
								defer dropFunc()

								return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{"SELECT"})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "table"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "1"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "SELECT"),
						),
					},
					{
						Config: fmt.Sprintf(tfConfig, `["SELECT", "UPDATE"]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								tables := []string{"test_schema.test_table", "dev_schema.test_table"}
								// To test default privileges, we need to create tables
								// in both dev and test schema after having applied the state.
								dropFunc := createTestTables(t, dbSuffix, tables, "")
								defer dropFunc()

								return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{"SELECT", "UPDATE"})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "2"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "SELECT"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.1", "UPDATE"),
						),
					},
				},
			})
		})
	}
}

// Test defaults privileges on schemas
func TestAccPostgresqlDefaultPrivilegesOnSchemas(t *testing.T) {
	skipIfNotAcc(t)

	// We have to create the database outside of resource.Test
	// because we need to create schemas to assert that grant are correctly applied
	// and we don't have this resource yet
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	// Set default privileges to the test role then to public (i.e.: everyone)
	for _, role := range []string{roleName, "public"} {
		t.Run(role, func(t *testing.T) {

			hclText := `
resource "postgresql_default_privileges" "test_ro" {
	database    = "%s"
	owner       = "%s"
	role        = "%s"
	object_type = "schema"
	privileges  = %%s
}
`
			// We set PGUSER as owner as he will create the test schemas
			var tfConfig = fmt.Sprintf(hclText, dbName, config.Username, role)

			resource.Test(t, resource.TestCase{
				PreCheck: func() {
					testAccPreCheck(t)
					testCheckCompatibleVersion(t, featurePrivileges)
					testCheckCompatibleVersion(t, featurePrivilegesOnSchemas)
				},
				Providers: testAccProviders,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(tfConfig, `[]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								schemas := []string{"test_schema2", "dev_schema2"}
								// To test default privileges, we need to create a schema
								// after having apply the state.
								dropFunc := createTestSchemas(t, dbSuffix, schemas, "")
								defer dropFunc()

								return testCheckSchemasPrivileges(t, dbName, roleName, schemas, []string{})
							},
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "schema"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "0"),
						),
					},
					{
						Config: fmt.Sprintf(tfConfig, `["CREATE", "USAGE"]`),
						Check: resource.ComposeTestCheckFunc(
							func(*terraform.State) error {
								schemas := []string{"test_schema2", "dev_schema2"}
								// To test default privileges, we need to create a schema
								// after having apply the state.
								dropFunc := createTestSchemas(t, dbSuffix, schemas, "")
								defer dropFunc()

								return testCheckSchemasPrivileges(t, dbName, roleName, schemas, []string{"CREATE", "USAGE"})
							},
							resource.TestCheckResourceAttr(
								"postgresql_default_privileges.test_ro", "id", fmt.Sprintf("%s_%s_noschema_%s_schema", role, dbName, config.Username),
							),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "2"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "CREATE"),
							resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.1", "USAGE"),
						),
					},
				},
			})
		})
	}
}

func TestAccPostgresqlDefaultPrivileges_Routines(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	resourceConfig := fmt.Sprintf(`
resource "postgresql_default_privileges" "test" {
	database          = "%s"
	schema            = "test_schema"
	owner             = "%s"
	role              = "%s"
	object_type       = "routine"
	privileges        = ["EXECUTE"]
}
`, dbName, config.Username, roleName)

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
					// Create a test function to check if default privileges are applied
					func(*terraform.State) error {
						dbExecute(
							t, config.connStr(dbName),
							"CREATE FUNCTION test_schema.test_function() RETURNS int AS $$ SELECT 1; $$ LANGUAGE sql;",
						)

						db := connectAsTestRole(t, roleName, dbName)
						defer closeDB(t, db)

						if _, err := db.Exec("SELECT test_schema.test_function();"); err != nil {
							t.Fatalf("Expected test role to be able to execute function, got error: %s", err)
						}

						return nil
					},
				),
			},
		},
	})
}

// TestAccPostgresqlDefaultPrivileges_ReverseMembershipManyDatabases manages
// default privileges of the same owner in several databases at once, where
// the owner is a member of the provider role (#25). For a non-superuser
// provider, withRolesGranted then revokes that membership, grants the owner to
// the provider role, and restores both afterwards, in each database's
// transaction. The membership rows are cluster-wide while the owner lock is
// per database, so without serializing them the transactions revoke the same
// row at the same time: "tuple concurrently deleted" (PostgreSQL 15 and
// older).
//
// The reverse membership is set up by the provider role itself, which a
// non-superuser can only do up to PostgreSQL 15 (since 16 it needs ADMIN on
// itself), so the rds profile skips this test on 16+.
func TestAccPostgresqlDefaultPrivileges_ReverseMembershipManyDatabases(t *testing.T) {
	skipIfNotAcc(t)
	testAccPreCheck(t)

	client := testAccProvider.Meta().(*Client)
	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect: %v", err)
	}
	if !client.config.Superuser && db.featureSupported(featureCreateRoleSelfGrant) {
		t.Skip("a non-superuser cannot grant its own role to another role since PostgreSQL 16")
	}

	const databases = 3
	owner := testUniqueName("tf_tests_dp_reverse_owner")
	reader := testUniqueName("tf_tests_dp_reverse_reader")
	dbSuffix := testUniqueName("dp_reverse")

	config := getTestConfig(t)
	dsn := config.connStr("postgres")

	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", owner))
	defer dbExecute(t, dsn, fmt.Sprintf("DROP ROLE %s", owner))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", reader))
	defer dbExecute(t, dsn, fmt.Sprintf("DROP ROLE %s", reader))
	// The owner is a member of the provider role: the reverse of the
	// membership withRolesGranted needs.
	dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(config.Username), owner))

	dbNames := make([]string, databases)
	for i := range databases {
		dbNames[i], _ = getTestDBNames(fmt.Sprintf("%s_%d", dbSuffix, i))
		dbExecute(t, dsn, fmt.Sprintf("CREATE DATABASE %s", dbNames[i]))
		defer dropTestDatabase(t, config, dbNames[i])
	}

	tfConfig := func(tablePrivileges, sequencePrivileges string) string {
		return fmt.Sprintf(`
locals {
  databases = ["%s"]
}

resource "postgresql_default_privileges" "tables" {
  count       = length(local.databases)
  database    = local.databases[count.index]
  owner       = %q
  role        = %q
  schema      = "public"
  object_type = "table"
  privileges  = %s
}

resource "postgresql_default_privileges" "sequences" {
  count       = length(local.databases)
  database    = local.databases[count.index]
  owner       = %q
  role        = %q
  schema      = "public"
  object_type = "sequence"
  privileges  = %s
}
`, strings.Join(dbNames, `", "`), owner, reader, tablePrivileges, owner, reader, sequencePrivileges)
	}

	membershipChecks := resource.ComposeTestCheckFunc(
		// The reverse membership is restored and the temporary one is gone.
		checkUserMembership(t, owner, config.Username, true),
		checkUserMembership(t, config.Username, owner, false),
	)

	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testCheckCompatibleVersion(t, featurePrivileges) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: tfConfig(`["SELECT"]`, `["SELECT"]`),
				Check:  membershipChecks,
			},
			{
				Config: tfConfig(`["INSERT", "SELECT"]`, `["SELECT", "USAGE"]`),
				Check:  membershipChecks,
			},
			{
				// Destroy the default privileges, keep the databases and
				// roles for the membership check.
				Config: `# empty`,
				Check:  membershipChecks,
			},
		},
	})
}

// TestAccPostgresqlDefaultPrivileges_BuiltinDefaults applies global default
// privileges (no schema) that equal the built-in defaults: everything for the
// owner itself, EXECUTE on functions and USAGE on types for PUBLIC. PostgreSQL
// stores no pg_default_acl row for such an entry, so the Read must take the
// missing row as acldefault instead of "resource gone" (#117), directly and
// with catalog_snapshot.
func TestAccPostgresqlDefaultPrivileges_BuiltinDefaults(t *testing.T) {
	skipIfNotAcc(t)

	for _, tc := range []struct{ name, snapshot string }{
		{"direct", ""},
		{"catalog_snapshot", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGCATALOGSNAPSHOT", tc.snapshot)
			isolateDBRegistry(t)
			testAccDefaultPrivilegesBuiltinDefaults(t, tc.snapshot != "")
		})
	}
}

func testAccDefaultPrivilegesBuiltinDefaults(t *testing.T, snapshotOn bool) {
	config := getTestConfig(t)
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	dbName, _ := getTestDBNames(dbSuffix)
	ownerName := testUniqueName("tf_dp_builtin_owner")

	// The owner is created out of band: destroying these resources revokes
	// the owner's own defaults, which leaves pg_default_acl rows in the test
	// database that only dropping the database removes.
	dbExecute(t, config.connStr("postgres"), fmt.Sprintf("CREATE ROLE %s", pq.QuoteIdentifier(ownerName)))
	defer func() {
		teardown()
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(ownerName)))
	}()

	stateConfig := fmt.Sprintf(`
resource "postgresql_default_privileges" "owner_tables" {
	database    = "%[1]s"
	role        = "%[2]s"
	owner       = "%[2]s"
	object_type = "table"
	privileges  = ["ALL"]
}

resource "postgresql_default_privileges" "owner_sequences" {
	database    = "%[1]s"
	role        = "%[2]s"
	owner       = "%[2]s"
	object_type = "sequence"
	privileges  = ["ALL"]
}

resource "postgresql_default_privileges" "owner_schemas" {
	database    = "%[1]s"
	role        = "%[2]s"
	owner       = "%[2]s"
	object_type = "schema"
	privileges  = ["USAGE", "CREATE"]
}

resource "postgresql_default_privileges" "public_functions" {
	database    = "%[1]s"
	role        = "public"
	owner       = "%[2]s"
	object_type = "function"
	privileges  = ["EXECUTE"]
}

resource "postgresql_default_privileges" "public_types" {
	database    = "%[1]s"
	role        = "public"
	owner       = "%[2]s"
	object_type = "type"
	privileges  = ["USAGE"]
}
`, dbName, ownerName)

	clearCatalogSnapshots()
	builds := catalogSnapshotBuilds()
	checkSnapshotUsed := func(*terraform.State) error {
		now := catalogSnapshotBuilds()
		if snapshotOn && now == builds {
			return fmt.Errorf("no catalog snapshot built, the Reads did not use it")
		}
		if !snapshotOn && now != builds {
			return fmt.Errorf("%d catalog snapshot(s) built with catalog_snapshot off", now-builds)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivilegesOnSchemas)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: stateConfig,
				Check: resource.ComposeTestCheckFunc(
					// PostgreSQL removed every entry: they equal acldefault.
					testCheckDefaultACLCount(t, dbName, ownerName, 0),
					resource.TestCheckResourceAttr("postgresql_default_privileges.owner_tables", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_default_privileges.public_functions", "privileges.#", "1"),
				),
			},
			{
				// The Reads of the next plan see the built-in defaults.
				Config:   stateConfig,
				PlanOnly: true,
			},
			{
				// A change out of band is still drift. The connected role
				// needs the owner's membership for it (rds profile).
				PreConfig: func() {
					dbExecute(t, config.connStr(dbName), fmt.Sprintf(
						"GRANT %[1]s TO CURRENT_USER; "+
							"ALTER DEFAULT PRIVILEGES FOR ROLE %[1]s REVOKE INSERT ON TABLES FROM %[1]s; "+
							"REVOKE %[1]s FROM CURRENT_USER", pq.QuoteIdentifier(ownerName)))
				},
				Config:             stateConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: stateConfig,
				Check: resource.ComposeTestCheckFunc(
					testCheckDefaultACLCount(t, dbName, ownerName, 0),
					checkSnapshotUsed,
				),
			},
		},
	})
}

// testCheckDefaultACLCount checks the number of pg_default_acl entries of
// owner in dbName.
func testCheckDefaultACLCount(t *testing.T, dbName, owner string, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		config := getTestConfig(t)
		db, err := sql.Open("postgres", config.connStr(dbName))
		if err != nil {
			return err
		}
		defer closeDB(t, db)

		var got int
		if err := db.QueryRow(
			"SELECT count(*) FROM pg_default_acl WHERE defaclrole = (SELECT oid FROM pg_roles WHERE rolname = $1)", owner,
		).Scan(&got); err != nil {
			return fmt.Errorf("could not count default ACLs: %w", err)
		}
		if got != want {
			return fmt.Errorf("pg_default_acl entries of %s: got %d, want %d", owner, got, want)
		}
		return nil
	}
}

// isolateDBRegistry gives the test an empty dbRegistry and restores the
// previous one afterwards. A pooled DBConnection keeps the Client, and so the
// provider settings, of the configure that opened it, so a test that changes
// a setting such as catalog_snapshot between runs needs pools of its own.
func isolateDBRegistry(t *testing.T) {
	t.Helper()
	dbRegistryLock.Lock()
	saved := dbRegistry
	dbRegistry = make(map[string]*DBConnection)
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		own := dbRegistry
		dbRegistry = saved
		dbRegistryLock.Unlock()
		for _, conn := range own {
			_ = conn.Close()
		}
	})
}

// TestBuildReadRoleDefaultPrivilegesQueryBuiltin checks that only a global
// entry falls back to acldefault, with the acldefault type of the object type.
func TestBuildReadRoleDefaultPrivilegesQueryBuiltin(t *testing.T) {
	for _, tc := range []struct {
		schema, objectType, want string
	}{
		{"", "table", "acldefault('r', "},
		{"", "sequence", "acldefault('s', "},
		{"", "function", "acldefault('f', "},
		{"", "routine", "acldefault('f', "},
		{"", "type", "acldefault('T', "},
		{"", "schema", "acldefault('n', "},
		{"test_schema", "table", ""},
	} {
		d := schema.TestResourceDataRaw(t, resourcePostgreSQLDefaultPrivileges().Schema, map[string]any{
			"database": "db", "role": "r", "owner": "o", "schema": tc.schema,
			"object_type": tc.objectType, "privileges": []any{},
		})
		query, _ := buildReadRoleDefaultPrivilegesQuery(d)
		if tc.want == "" {
			if strings.Contains(query, "acldefault") {
				t.Errorf("schema %q, %s: a per-schema entry must not fall back to acldefault:\n%s", tc.schema, tc.objectType, query)
			}
		} else if !strings.Contains(query, tc.want) {
			t.Errorf("schema %q, %s: query does not contain %q:\n%s", tc.schema, tc.objectType, tc.want, query)
		}
	}
}
