package postgresql

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"

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

	// We set PGUSER as owner as he will create the test table
	var stateConfig = fmt.Sprintf(`

resource postgresql_role "test_owner" {
    name = "test_owner"
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
	`, dbName, dbName, roleName)

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
						dropFunc := createTestTables(t, dbSuffix, tables, "test_owner")
						defer dropFunc()

						return testCheckTablesPrivileges(t, dbName, roleName, tables, []string{"SELECT"})
					},
					resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "object_type", "table"),
					resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_default_privileges.test_ro", "privileges.0", "SELECT"),

					// check if connected user does not have test_owner granted anymore.
					checkUserMembership(t, config.Username, "test_owner", false),
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

func TestDefaultPrivilegesDelta(t *testing.T) {
	cases := []struct {
		name            string
		old, new        []string
		wantIncremental bool
		wantAdded       []string
		wantRemoved     []string
	}{
		{name: "add one", old: []string{"SELECT"}, new: []string{"SELECT", "INSERT"}, wantIncremental: true, wantAdded: []string{"INSERT"}},
		{name: "remove one", old: []string{"SELECT", "INSERT"}, new: []string{"SELECT"}, wantIncremental: true, wantRemoved: []string{"INSERT"}},
		{name: "add and remove", old: []string{"SELECT", "INSERT"}, new: []string{"SELECT", "UPDATE"}, wantIncremental: true, wantAdded: []string{"UPDATE"}, wantRemoved: []string{"INSERT"}},
		{name: "to empty", old: []string{"SELECT", "INSERT"}, new: []string{}, wantIncremental: true, wantRemoved: []string{"SELECT", "INSERT"}},
		{name: "from empty", old: []string{}, new: []string{"SELECT"}, wantIncremental: true, wantAdded: []string{"SELECT"}},
		{name: "identical", old: []string{"SELECT"}, new: []string{"SELECT"}, wantIncremental: true},
		{name: "ALL on old side", old: []string{"ALL"}, new: []string{"SELECT"}},
		{name: "ALL on new side", old: []string{"SELECT"}, new: []string{"ALL"}},
		{name: "ALL PRIVILEGES any case", old: []string{"SELECT"}, new: []string{"all privileges"}},
		{name: "all lower case", old: []string{"all"}, new: []string{"USAGE"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			added, removed, incremental := defaultPrivilegesDelta(stringSliceToSet(c.old), stringSliceToSet(c.new))
			if incremental != c.wantIncremental {
				t.Fatalf("incremental = %t, want %t", incremental, c.wantIncremental)
			}
			if !incremental {
				return
			}
			if !sameStringSet(added, c.wantAdded) {
				t.Errorf("added = %v, want %v", added.List(), c.wantAdded)
			}
			if !sameStringSet(removed, c.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed.List(), c.wantRemoved)
			}
		})
	}
}

func sameStringSet(got *schema.Set, want []string) bool {
	if got.Len() != len(want) {
		return false
	}
	for _, w := range want {
		if !got.Contains(w) {
			return false
		}
	}
	return true
}

func TestDefaultPrivilegesStatements(t *testing.T) {
	getter := func(pgSchema string, withGrant bool, privileges ...string) ResourceSchemeGetter {
		values := map[string]any{
			"role":              "reader",
			"owner":             "app_owner",
			"schema":            pgSchema,
			"object_type":       "table",
			"with_grant_option": withGrant,
			"privileges":        stringSliceToSet(privileges),
		}
		return func(key string) any { return values[key] }
	}

	cases := []struct {
		name   string
		getter ResourceSchemeGetter
		old    *schema.Set
		want   []string
	}{
		{
			name:   "create grants after revoking everything",
			getter: getter("app", false, "SELECT"),
			old:    nil,
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" REVOKE ALL ON TABLES FROM "reader"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reader"`,
			},
		},
		{
			name:   "create with empty set only revokes",
			getter: getter("app", false),
			old:    nil,
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" REVOKE ALL ON TABLES FROM "reader"`,
			},
		},
		{
			name:   "update adding a single privilege only grants it",
			getter: getter("app", true, "SELECT", "INSERT"),
			old:    stringSliceToSet([]string{"SELECT"}),
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" GRANT INSERT ON TABLES TO "reader" WITH GRANT OPTION`,
			},
		},
		{
			name:   "update removing a single privilege only revokes it",
			getter: getter("", false, "SELECT"),
			old:    stringSliceToSet([]string{"SELECT", "INSERT"}),
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner"  REVOKE INSERT ON TABLES FROM "reader"`,
			},
		},
		{
			name:   "update swapping privileges revokes before granting",
			getter: getter("app", false, "SELECT", "UPDATE"),
			old:    stringSliceToSet([]string{"SELECT", "INSERT"}),
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" REVOKE INSERT ON TABLES FROM "reader"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" GRANT UPDATE ON TABLES TO "reader"`,
			},
		},
		{
			name:   "update to empty set revokes the old privileges",
			getter: getter("app", false),
			old:    stringSliceToSet([]string{"SELECT"}),
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" REVOKE SELECT ON TABLES FROM "reader"`,
			},
		},
		{
			name:   "update from ALL falls back to full revoke and regrant",
			getter: getter("app", false, "SELECT"),
			old:    stringSliceToSet([]string{"ALL"}),
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" REVOKE ALL ON TABLES FROM "reader"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reader"`,
			},
		},
		{
			name:   "update to ALL falls back to full revoke and regrant",
			getter: getter("app", false, "ALL"),
			old:    stringSliceToSet([]string{"SELECT"}),
			want: []string{
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" REVOKE ALL ON TABLES FROM "reader"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA "app" GRANT ALL ON TABLES TO "reader"`,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := defaultPrivilegesStatements(c.getter, c.old)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("statements mismatch\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// testCheckDefaultACL asserts the exact set of default privileges recorded in
// pg_default_acl for owner/role on tables in the given schema, and that every
// entry carries the expected grant option. A nil want means "at least the
// privileges in atLeast" (used for ALL, whose expansion depends on the server version).
func testCheckDefaultACL(t *testing.T, dbName, owner, role, pgSchema string, want []string, atLeast []string, grantable bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		config := getTestConfig(t)
		db, err := sql.Open("postgres", config.connStr(dbName))
		if err != nil {
			return err
		}
		defer closeDB(t, db)

		rows, err := db.Query(`
SELECT a.privilege_type, a.is_grantable
FROM pg_default_acl d
JOIN pg_namespace n ON n.oid = d.defaclnamespace
CROSS JOIN LATERAL aclexplode(d.defaclacl) a
WHERE d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = $1)
  AND n.nspname = $2
  AND d.defaclobjtype = 'r'
  AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $3)`, owner, pgSchema, role)
		if err != nil {
			return fmt.Errorf("could not read pg_default_acl: %w", err)
		}
		defer rows.Close()

		got := map[string]bool{}
		for rows.Next() {
			var priv string
			var isGrantable bool
			if err := rows.Scan(&priv, &isGrantable); err != nil {
				return err
			}
			got[priv] = isGrantable
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for priv, g := range got {
			if g != grantable {
				return fmt.Errorf("default privilege %s: is_grantable = %t, want %t", priv, g, grantable)
			}
		}

		if want != nil {
			if len(got) != len(want) {
				return fmt.Errorf("default privileges = %v, want %v", got, want)
			}
			for _, priv := range want {
				if _, ok := got[priv]; !ok {
					return fmt.Errorf("default privileges = %v, want %v", got, want)
				}
			}
			return nil
		}

		for _, priv := range atLeast {
			if _, ok := got[priv]; !ok {
				return fmt.Errorf("default privileges = %v, want at least %v", got, atLeast)
			}
		}
		return nil
	}
}

// Updates of privileges are applied as a diff (GRANT added / REVOKE removed),
// except when ALL is involved. Check the resulting catalog state at every step.
func TestAccPostgresqlDefaultPrivileges_IncrementalUpdate(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	tfConfig := fmt.Sprintf(`
resource "postgresql_default_privileges" "test" {
	database          = "%s"
	owner             = "%s"
	role              = "%s"
	schema            = "test_schema"
	object_type       = "table"
	with_grant_option = true
	privileges        = %%s
}
`, dbName, config.Username, roleName)

	step := func(privileges string, want, atLeast []string) resource.TestStep {
		return resource.TestStep{
			Config: fmt.Sprintf(tfConfig, privileges),
			Check:  testCheckDefaultACL(t, dbName, config.Username, roleName, "test_schema", want, atLeast, true),
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			step(`["SELECT"]`, []string{"SELECT"}, nil),
			// add a single privilege
			step(`["SELECT", "INSERT"]`, []string{"SELECT", "INSERT"}, nil),
			// add and remove in the same update
			step(`["SELECT", "UPDATE"]`, []string{"SELECT", "UPDATE"}, nil),
			// remove a single privilege
			step(`["SELECT"]`, []string{"SELECT"}, nil),
			// explicit list -> ALL
			step(`["ALL"]`, nil, []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}),
			// ALL -> explicit list
			step(`["SELECT", "DELETE"]`, []string{"SELECT", "DELETE"}, nil),
			// transition to an empty set
			step(`[]`, []string{}, nil),
			// and back from the empty set
			step(`["INSERT"]`, []string{"INSERT"}, nil),
		},
	})
}

// with_grant_option is ForceNew: toggling it replaces the resource, which must
// leave the privileges with the requested grant option.
func TestAccPostgresqlDefaultPrivileges_WithGrantOptionToggle(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	tfConfig := fmt.Sprintf(`
resource "postgresql_default_privileges" "test" {
	database          = "%s"
	owner             = "%s"
	role              = "%s"
	schema            = "test_schema"
	object_type       = "table"
	with_grant_option = %%t
	privileges        = ["SELECT", "INSERT"]
}
`, dbName, config.Username, roleName)

	want := []string{"SELECT", "INSERT"}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(tfConfig, false),
				Check:  testCheckDefaultACL(t, dbName, config.Username, roleName, "test_schema", want, nil, false),
			},
			{
				Config: fmt.Sprintf(tfConfig, true),
				Check:  testCheckDefaultACL(t, dbName, config.Username, roleName, "test_schema", want, nil, true),
			},
			{
				Config: fmt.Sprintf(tfConfig, false),
				Check:  testCheckDefaultACL(t, dbName, config.Username, roleName, "test_schema", want, nil, false),
			},
		},
	})
}
