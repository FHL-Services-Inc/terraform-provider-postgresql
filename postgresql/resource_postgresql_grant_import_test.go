package postgresql

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

// fakeImportCatalog is an in-memory importCatalog for the import ID parsers.
type fakeImportCatalog struct {
	roles     []string
	databases []string
	// schemas per database
	schemas map[string][]string
	// objects per "database.schema.object_type" (database-level object types
	// use an empty schema)
	objects map[string][]string
}

func pick(have []string, names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		for _, h := range have {
			if n == h {
				out[n] = true
			}
		}
	}
	return out
}

func (c *fakeImportCatalog) existingRoles(names []string) (map[string]bool, error) {
	return pick(append([]string{publicRole}, c.roles...), names), nil
}

func (c *fakeImportCatalog) existingDatabases(names []string) (map[string]bool, error) {
	return pick(c.databases, names), nil
}

func (c *fakeImportCatalog) existingSchemas(database string, names []string) (map[string]bool, error) {
	return pick(c.schemas[database], names), nil
}

func (c *fakeImportCatalog) existingObjects(database, pgSchema, objectType string, names []string) (map[string]bool, error) {
	have := c.objects[database+"."+pgSchema+"."+objectType]
	if objectType != "function" {
		return pick(have, names), nil
	}
	// like the SQL catalog: function objects match by name, with or without
	// an argument signature
	out := map[string]bool{}
	for _, n := range names {
		if pick(have, []string{functionImportBaseName(n)})[functionImportBaseName(n)] {
			out[n] = true
		}
	}
	return out, nil
}

func testImportCatalog() *fakeImportCatalog {
	return &fakeImportCatalog{
		roles:     []string{"app", "app_ro", "app_ro_owner", "owner", "my_owner", "admin"},
		databases: []string{"app", "my_db", "my_database", "db"},
		schemas: map[string][]string{
			"my_db": {"public", "test_schema", "test"},
			"app":   {"public", "noschema"},
		},
		objects: map[string][]string{
			"my_db.test_schema.table":    {"test_table", "test_table2", "test", "table2", "orders"},
			"my_db.test_schema.sequence": {"order_id_seq"},
			"my_db.test_schema.function": {"my_func"},
			"my_db..foreign_server":      {"remote_srv"},
			"my_db..foreign_data_wrapper": {
				"postgres_fdw",
			},
		},
	}
}

func sortedObjects(g grantImportID) grantImportID {
	sort.Strings(g.objects)
	if len(g.objects) == 0 {
		g.objects = nil
	}
	return g
}

func TestParseGrantImportID(t *testing.T) {
	cat := testImportCatalog()

	cases := []struct {
		id      string
		want    grantImportID
		wantErr string
	}{
		{
			id:   "app_ro_my_db_database",
			want: grantImportID{role: "app_ro", database: "my_db", objectType: "database"},
		},
		{
			// database name ends with the "database" token
			id:   "app_my_database_database",
			want: grantImportID{role: "app", database: "my_database", objectType: "database"},
		},
		{
			id:   "public_my_db_test_schema_schema",
			want: grantImportID{role: "public", database: "my_db", schema: "test_schema", objectType: "schema"},
		},
		{
			// whole-schema table grant: no objects
			id:   "app_ro_my_db_test_schema_table",
			want: grantImportID{role: "app_ro", database: "my_db", schema: "test_schema", objectType: "table"},
		},
		{
			id: "app_ro_my_db_test_schema_table_orders_test_table",
			want: grantImportID{
				role: "app_ro", database: "my_db", schema: "test_schema", objectType: "table",
				objects: []string{"orders", "test_table"},
			},
		},
		{
			id: "app_ro_my_db_test_schema_sequence_order_id_seq",
			want: grantImportID{
				role: "app_ro", database: "my_db", schema: "test_schema", objectType: "sequence",
				objects: []string{"order_id_seq"},
			},
		},
		{
			// function objects may carry an argument signature
			id: "app_my_db_test_schema_function_my_func(text, char)",
			want: grantImportID{
				role: "app", database: "my_db", schema: "test_schema", objectType: "function",
				objects: []string{"my_func(text, char)"},
			},
		},
		{
			id: "app_my_db_foreign_server_remote_srv",
			want: grantImportID{
				role: "app", database: "my_db", objectType: "foreign_server",
				objects: []string{"remote_srv"},
			},
		},
		{
			id: "app_my_db_foreign_data_wrapper_postgres_fdw",
			want: grantImportID{
				role: "app", database: "my_db", objectType: "foreign_data_wrapper",
				objects: []string{"postgres_fdw"},
			},
		},
		{
			// "test" + "table2" and "test_table2" are both relations
			id:      "app_my_db_test_schema_table_test_table2",
			wantErr: "ambiguous",
		},
		{
			id:      "nobody_my_db_database",
			wantErr: "does not match",
		},
		{
			id:      "app_my_db_test_schema_table_missing_table",
			wantErr: "does not match",
		},
		{
			id:      "app_my_db_test_schema_column_test_table_col1",
			wantErr: "column",
		},
		{
			id:      "app",
			wantErr: "does not match",
		},
	}

	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got, err := parseGrantImportID(c.id, cat)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error containing %q, got %v (result %+v)", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(sortedObjects(got), sortedObjects(c.want)) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseDefaultPrivilegesImportID(t *testing.T) {
	cat := testImportCatalog()

	cases := []struct {
		id      string
		want    defaultPrivilegesImportID
		wantErr string
	}{
		{
			id:   "app_ro_my_db_test_schema_my_owner_table",
			want: defaultPrivilegesImportID{role: "app_ro", database: "my_db", schema: "test_schema", owner: "my_owner", objectType: "table"},
		},
		{
			id:   "public_my_db_noschema_owner_sequence",
			want: defaultPrivilegesImportID{role: "public", database: "my_db", owner: "owner", objectType: "sequence"},
		},
		{
			id:   "app_ro_my_db_noschema_app_ro_owner_schema",
			want: defaultPrivilegesImportID{role: "app_ro", database: "my_db", owner: "app_ro_owner", objectType: "schema"},
		},
		{
			// "noschema" always means "no schema", even if a schema of that
			// name exists, as that is what the resource writes into the ID.
			id:   "admin_app_noschema_owner_function",
			want: defaultPrivilegesImportID{role: "admin", database: "app", owner: "owner", objectType: "function"},
		},
		{
			// role "app" + owner "ro_owner"? no such owner; "app_ro" + "owner" only
			id:   "app_ro_my_db_test_owner_type",
			want: defaultPrivilegesImportID{role: "app_ro", database: "my_db", schema: "test", owner: "owner", objectType: "type"},
		},
		{
			id:      "app_ro_my_db_test_schema_my_owner_view",
			wantErr: "object type",
		},
		{
			id:      "app_ro_my_db_missing_owner_table",
			wantErr: "does not match",
		},
		{
			// database and owner share tokens with role names
			id:   "app_app_noschema_app_ro_owner_table",
			want: defaultPrivilegesImportID{role: "app", database: "app", owner: "app_ro_owner", objectType: "table"},
		},
	}

	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got, err := parseDefaultPrivilegesImportID(c.id, cat)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error containing %q, got %v (result %+v)", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseDefaultPrivilegesImportIDAmbiguous(t *testing.T) {
	// role "a" in database "b_db", or role "a_b" in database "db"
	cat := &fakeImportCatalog{
		roles:     []string{"a", "a_b", "c"},
		databases: []string{"b_db", "db"},
	}
	_, err := parseDefaultPrivilegesImportID("a_b_db_noschema_c_table", cat)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected an ambiguity error, got %v", err)
	}
}

func TestParseGrantRoleImportID(t *testing.T) {
	cat := testImportCatalog()

	cases := []struct {
		id      string
		want    grantRoleImportID
		wantErr string
	}{
		{
			id:   "app_ro_admin_true",
			want: grantRoleImportID{role: "app_ro", grantRole: "admin", withAdminOption: true},
		},
		{
			id:   "my_owner_app_ro_owner_false",
			want: grantRoleImportID{role: "my_owner", grantRole: "app_ro_owner"},
		},
		{
			// app + ro_owner (missing) or app_ro + owner
			id:   "app_ro_owner_false",
			want: grantRoleImportID{role: "app_ro", grantRole: "owner"},
		},
		{
			id:      "app_ro_admin",
			wantErr: "true or false",
		},
		{
			id:      "app_nobody_true",
			wantErr: "does not match",
		},
		{
			// grant_role cannot be public
			id:      "app_public_true",
			wantErr: "does not match",
		},
	}

	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got, err := parseGrantRoleImportID(c.id, cat)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error containing %q, got %v (result %+v)", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseGrantRoleImportIDAmbiguous(t *testing.T) {
	cat := &fakeImportCatalog{roles: []string{"a", "a_b", "b_c", "c"}}
	_, err := parseGrantRoleImportID("a_b_c_true", cat)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected an ambiguity error, got %v", err)
	}
}

func TestImportSegmentations(t *testing.T) {
	ok := map[string]bool{"a": true, "a_b": true, "b": true, "c": true}
	got := importSegmentations(strings.Split("a_b_c", "_"), func(s string) bool { return ok[s] }, 10)
	want := [][]string{{"a", "b", "c"}, {"a_b", "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := importSegmentations(strings.Split("a_x", "_"), func(s string) bool { return ok[s] }, 10); len(got) != 0 {
		t.Fatalf("expected no segmentation, got %v", got)
	}
}

func importSteps(resourceNames ...string) []resource.TestStep {
	steps := make([]resource.TestStep, 0, len(resourceNames))
	for _, name := range resourceNames {
		steps = append(steps, resource.TestStep{
			ResourceName:      name,
			ImportState:       true,
			ImportStateVerify: true,
		})
	}
	return steps
}

func TestAccPostgresqlGrant_Import(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	createTestTables(t, dbSuffix, []string{"test_schema.test_table", "test_schema.test_table2", "dev_schema.test_table", "dev_schema.other_table"}, "")
	dbName, roleName := getTestDBNames(dbSuffix)

	// Role and database names contain "_", like the schema and table names,
	// so the import has to resolve the split of the ID against the catalog.
	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "table_all" {
  database    = "%[1]s"
  role        = "%[2]s"
  schema      = "test_schema"
  object_type = "table"
  privileges  = ["SELECT", "INSERT"]
}

resource "postgresql_grant" "table_objects" {
  database    = "%[1]s"
  role        = "%[2]s"
  schema      = "dev_schema"
  object_type = "table"
  objects     = ["test_table", "other_table"]
  privileges  = ["SELECT"]
}

resource "postgresql_grant" "database" {
  database    = "%[1]s"
  role        = "%[2]s"
  object_type = "database"
  privileges  = ["CONNECT", "TEMPORARY"]
}

resource "postgresql_grant" "schema" {
  database          = "%[1]s"
  role              = "%[2]s"
  schema            = "test_schema"
  object_type       = "schema"
  privileges        = ["USAGE", "CREATE"]
  with_grant_option = true
}

resource "postgresql_grant" "public_schema" {
  database    = "%[1]s"
  role        = "public"
  schema      = "dev_schema"
  object_type = "schema"
  privileges  = ["USAGE"]
}
`, dbName, roleName)

	steps := []resource.TestStep{{Config: tfConfig}}
	steps = append(steps, importSteps(
		"postgresql_grant.table_all",
		"postgresql_grant.table_objects",
		"postgresql_grant.database",
		"postgresql_grant.schema",
		"postgresql_grant.public_schema",
	)...)
	steps = append(steps,
		resource.TestStep{
			// Objects may be listed in any order in a hand-written ID.
			ResourceName:      "postgresql_grant.table_objects",
			ImportState:       true,
			ImportStateId:     fmt.Sprintf("%s_%s_dev_schema_table_test_table_other_table", roleName, dbName),
			ImportStateVerify: true,
		},
		resource.TestStep{
			ResourceName:  "postgresql_grant.table_all",
			ImportState:   true,
			ImportStateId: fmt.Sprintf("%s_%s_missing_schema_table", roleName, dbName),
			ExpectError:   regexp.MustCompile("does not match existing"),
		},
	)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps:     steps,
	})
}

func TestAccPostgresqlDefaultPrivileges_Import(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	tfConfig := fmt.Sprintf(`
resource "postgresql_default_privileges" "in_schema" {
  database          = "%[1]s"
  owner             = "%[2]s"
  role              = "%[3]s"
  schema            = "test_schema"
  object_type       = "table"
  privileges        = ["SELECT", "UPDATE"]
  with_grant_option = true
}

resource "postgresql_default_privileges" "no_schema" {
  database    = "%[1]s"
  owner       = "%[2]s"
  role        = "%[3]s"
  object_type = "sequence"
  privileges  = ["USAGE"]
}

resource "postgresql_default_privileges" "public_functions" {
  database    = "%[1]s"
  owner       = "%[2]s"
  role        = "public"
  schema      = "dev_schema"
  object_type = "function"
  privileges  = ["EXECUTE"]
}
`, dbName, config.Username, roleName)

	steps := []resource.TestStep{{Config: tfConfig}}
	steps = append(steps, importSteps(
		"postgresql_default_privileges.in_schema",
		"postgresql_default_privileges.no_schema",
		"postgresql_default_privileges.public_functions",
	)...)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps:     steps,
	})
}

func TestAccPostgresqlGrantRole_Import(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()

	_, roleName := getTestDBNames(dbSuffix)

	tfConfig := fmt.Sprintf(`
resource "postgresql_role" "group_admin" {
  name = "%[1]s_group_admin"
}

resource "postgresql_role" "group" {
  name = "%[1]s_group"
}

resource "postgresql_grant_role" "with_admin" {
  role              = "%[1]s"
  grant_role        = postgresql_role.group_admin.name
  with_admin_option = true
}

resource "postgresql_grant_role" "plain" {
  role       = "%[1]s"
  grant_role = postgresql_role.group.name
}
`, roleName)

	steps := []resource.TestStep{{Config: tfConfig}}
	steps = append(steps, importSteps(
		"postgresql_grant_role.with_admin",
		"postgresql_grant_role.plain",
	)...)
	steps = append(steps, resource.TestStep{
		// The ID's admin flag is only a hint, Read refreshes it.
		ResourceName:      "postgresql_grant_role.with_admin",
		ImportState:       true,
		ImportStateId:     fmt.Sprintf("%[1]s_%[1]s_group_admin_false", roleName),
		ImportStateVerify: true,
	})

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps:     steps,
	})
}
