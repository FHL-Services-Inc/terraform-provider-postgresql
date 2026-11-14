package postgresql

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func (c *fakeImportCatalog) existingColumns(database, pgSchema, table string, names []string) (map[string]bool, error) {
	return pick(c.objects[database+"."+pgSchema+".column."+table], names), nil
}

func TestColumnGrantID(t *testing.T) {
	cases := []struct {
		role, database, schema, table string
		columns                       []string
		privilege                     string
		want                          string
	}{
		{
			role: "app_ro", database: "my_db", schema: "test_schema", table: "test_table",
			columns: []string{"col_b", "col_a"}, privilege: "SELECT",
			want: "app_ro/my_db/test_schema/column/test_table/col_a,col_b/SELECT",
		},
		{
			// separators and the escape character itself are escaped
			role: "a/b", database: "db%1", schema: "s", table: "t,1",
			columns: []string{"c/1", "c,2"}, privilege: "UPDATE",
			want: "a%2Fb/db%251/s/column/t%2C1/c%2C2,c%2F1/UPDATE",
		},
	}
	for _, c := range cases {
		got := columnGrantID(c.role, c.database, c.schema, c.table, c.columns, c.privilege)
		if got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestGenerateGrantIDColumn(t *testing.T) {
	d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{
		"role":        "app_ro",
		"database":    "my_db",
		"schema":      "test_schema",
		"object_type": "column",
		"objects":     []any{"test_table"},
		"columns":     []any{"col_b", "col_a"},
		"privileges":  []any{"INSERT"},
	})
	if got, want := generateGrantID(d), "app_ro/my_db/test_schema/column/test_table/col_a,col_b/INSERT"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// other object types keep their ID
	d = schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{
		"role":        "app_ro",
		"database":    "my_db",
		"schema":      "test_schema",
		"object_type": "table",
		"privileges":  []any{"SELECT"},
	})
	if got, want := generateGrantID(d), "app_ro_my_db_test_schema_table"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGrantStateUpgradeV0(t *testing.T) {
	r := resourcePostgreSQLGrant()
	if r.SchemaVersion != 1 || len(r.StateUpgraders) != 1 || r.StateUpgraders[0].Version != 0 {
		t.Fatalf("expected one state upgrader from version 0 to 1, got version %d, %d upgraders", r.SchemaVersion, len(r.StateUpgraders))
	}
	upgrade := r.StateUpgraders[0].Upgrade

	cases := []struct {
		name   string
		state  map[string]any
		wantID string
	}{
		{
			name: "column grant",
			state: map[string]any{
				"id":          "app_ro_my_db_test_schema_column_test_table_col_b_col_a",
				"role":        "app_ro",
				"database":    "my_db",
				"schema":      "test_schema",
				"object_type": "column",
				"objects":     []any{"test_table"},
				"columns":     []any{"col_b", "col_a"},
				"privileges":  []any{"SELECT"},
			},
			wantID: "app_ro/my_db/test_schema/column/test_table/col_a,col_b/SELECT",
		},
		{
			name: "table grant unchanged",
			state: map[string]any{
				"id":          "app_ro_my_db_test_schema_table",
				"role":        "app_ro",
				"database":    "my_db",
				"schema":      "test_schema",
				"object_type": "table",
				"objects":     []any{},
				"privileges":  []any{"SELECT"},
			},
			wantID: "app_ro_my_db_test_schema_table",
		},
		{
			name: "column grant without privileges keeps its ID",
			state: map[string]any{
				"id":          "app_ro_my_db_test_schema_column_test_table_col_a",
				"role":        "app_ro",
				"database":    "my_db",
				"schema":      "test_schema",
				"object_type": "column",
				"objects":     []any{"test_table"},
				"columns":     []any{"col_a"},
				"privileges":  []any{},
			},
			wantID: "app_ro_my_db_test_schema_column_test_table_col_a",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := upgrade(context.Background(), c.state, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got["id"] != c.wantID {
				t.Fatalf("id = %v, want %q", got["id"], c.wantID)
			}
		})
	}
}

func TestParseColumnGrantImportID(t *testing.T) {
	cat := testImportCatalog()
	cat.objects["my_db.test_schema.column.test_table"] = []string{"col_a", "col_b", "c,2"}
	cat.objects["my_db.test_schema.table"] = append(cat.objects["my_db.test_schema.table"], "t,1")
	cat.objects["my_db.test_schema.column.t,1"] = []string{"c,2"}

	cases := []struct {
		id      string
		want    grantImportID
		wantErr string
	}{
		{
			id: "app_ro/my_db/test_schema/column/test_table/col_b,col_a/SELECT",
			want: grantImportID{
				role: "app_ro", database: "my_db", schema: "test_schema", objectType: "column",
				objects: []string{"test_table"}, columns: []string{"col_a", "col_b"}, privilege: "SELECT",
			},
		},
		{
			id: "app_ro/my_db/test_schema/column/t%2C1/c%2C2/update",
			want: grantImportID{
				role: "app_ro", database: "my_db", schema: "test_schema", objectType: "column",
				objects: []string{"t,1"}, columns: []string{"c,2"}, privilege: "UPDATE",
			},
		},
		{id: "nobody/my_db/test_schema/column/test_table/col_a/SELECT", wantErr: "role"},
		{id: "app_ro/nodb/test_schema/column/test_table/col_a/SELECT", wantErr: "database"},
		{id: "app_ro/my_db/noschema/column/test_table/col_a/SELECT", wantErr: "schema"},
		{id: "app_ro/my_db/test_schema/column/missing/col_a/SELECT", wantErr: "table"},
		{id: "app_ro/my_db/test_schema/column/test_table/col_a,nocol/SELECT", wantErr: "nocol"},
		{id: "app_ro/my_db/test_schema/column/test_table/col_a/DELETE", wantErr: "privilege"},
		{id: "app_ro/my_db/test_schema/column/test_table//SELECT", wantErr: "columns"},
		{id: "app_ro/my_db/test_schema/table/test_table/col_a/SELECT", wantErr: "column"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got, err := parseColumnGrantImportID(c.id, cat)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseGrantImportIDOldColumnFormat(t *testing.T) {
	_, err := parseGrantImportID("app_my_db_test_schema_column_test_table_col1", testImportCatalog())
	if err == nil || !strings.Contains(err.Error(), "<role>/<database>/<schema>/column/") {
		t.Fatalf("expected an error pointing to the column grant ID format, got %v", err)
	}
}

// TestAccPostgresqlGrant_ImportColumns imports two column grants on the same
// table and columns that differ only by privilege (they shared one ID before).
func TestAccPostgresqlGrant_ImportColumns(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	createTestTables(t, dbSuffix, []string{"test_schema.test_table"}, "")
	dbName, roleName := getTestDBNames(dbSuffix)

	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "select_columns" {
  database    = "%[1]s"
  role        = "%[2]s"
  schema      = "test_schema"
  object_type = "column"
  objects     = ["test_table"]
  columns     = ["test_column_one", "test_column_two"]
  privileges  = ["SELECT"]
}

resource "postgresql_grant" "update_columns" {
  database          = "%[1]s"
  role              = "%[2]s"
  schema            = "test_schema"
  object_type       = "column"
  objects           = ["test_table"]
  columns           = ["test_column_one", "test_column_two"]
  privileges        = ["UPDATE"]
  with_grant_option = true
}
`, dbName, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.select_columns", "id",
						fmt.Sprintf("%s/%s/test_schema/column/test_table/test_column_one,test_column_two/SELECT", roleName, dbName)),
					resource.TestCheckResourceAttr("postgresql_grant.update_columns", "id",
						fmt.Sprintf("%s/%s/test_schema/column/test_table/test_column_one,test_column_two/UPDATE", roleName, dbName)),
				),
			},
			{ResourceName: "postgresql_grant.select_columns", ImportState: true, ImportStateVerify: true},
			{ResourceName: "postgresql_grant.update_columns", ImportState: true, ImportStateVerify: true},
			{
				// columns in any order
				ResourceName:      "postgresql_grant.select_columns",
				ImportState:       true,
				ImportStateId:     fmt.Sprintf("%s/%s/test_schema/column/test_table/test_column_two,test_column_one/SELECT", roleName, dbName),
				ImportStateVerify: true,
			},
			{
				ResourceName:  "postgresql_grant.select_columns",
				ImportState:   true,
				ImportStateId: fmt.Sprintf("%s/%s/test_schema/column/test_table/test_column_one,missing_column/SELECT", roleName, dbName),
				ExpectError:   regexp.MustCompile("missing_column"),
			},
		},
	})
}
