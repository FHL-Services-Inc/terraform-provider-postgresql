package postgresql

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func TestBuildObjectsOwnersQuery(t *testing.T) {
	const relQuery = `
SELECT DISTINCT pg_roles.rolname
FROM pg_class
JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
JOIN pg_roles ON pg_roles.oid = pg_class.relowner
WHERE pg_namespace.nspname = $1 AND pg_class.relkind = ANY($2)`
	const procQuery = `
SELECT DISTINCT pg_roles.rolname
FROM pg_proc
JOIN pg_namespace ON pg_namespace.oid = pg_proc.pronamespace
JOIN pg_roles ON pg_roles.oid = pg_proc.proowner
WHERE pg_namespace.nspname = $1 AND pg_proc.prokind = ANY($2)`
	tableKinds := []string{"r", "p", "v", "m", "f"}

	cases := []struct {
		name       string
		objectType string
		objects    []string
		wantQuery  string
		wantArgs   []any
	}{
		{
			name:       "all tables",
			objectType: "table",
			wantQuery:  relQuery,
			wantArgs:   []any{"test_schema", pq.Array(tableKinds)},
		},
		{
			name:       "selected tables",
			objectType: "table",
			objects:    []string{"t1", "t2"},
			wantQuery:  relQuery + " AND pg_class.relname = ANY($3)",
			wantArgs:   []any{"test_schema", pq.Array(tableKinds), pq.Array([]string{"t1", "t2"})},
		},
		{
			name:       "column grant looks at its table",
			objectType: "column",
			objects:    []string{"t1"},
			wantQuery:  relQuery + " AND pg_class.relname = ANY($3)",
			wantArgs:   []any{"test_schema", pq.Array(tableKinds), pq.Array([]string{"t1"})},
		},
		{
			name:       "selected sequences",
			objectType: "sequence",
			objects:    []string{"s1"},
			wantQuery:  relQuery + " AND pg_class.relname = ANY($3)",
			wantArgs:   []any{"test_schema", pq.Array([]string{"S"}), pq.Array([]string{"s1"})},
		},
		{
			name:       "all sequences",
			objectType: "sequence",
			wantQuery:  relQuery,
			wantArgs:   []any{"test_schema", pq.Array([]string{"S"})},
		},
		{
			name:       "functions with signatures",
			objectType: "function",
			objects:    []string{"f1(integer, text)", "f2"},
			wantQuery:  procQuery + " AND pg_proc.proname = ANY($3)",
			wantArgs:   []any{"test_schema", pq.Array([]string{"f", "a", "w"}), pq.Array([]string{"f1", "f2"})},
		},
		{
			name:       "all procedures",
			objectType: "procedure",
			wantQuery:  procQuery,
			wantArgs:   []any{"test_schema", pq.Array([]string{"p"})},
		},
		{
			name:       "routines",
			objectType: "routine",
			objects:    []string{"r1()"},
			wantQuery:  procQuery + " AND pg_proc.proname = ANY($3)",
			wantArgs:   []any{"test_schema", pq.Array([]string{"f", "a", "w", "p"}), pq.Array([]string{"r1"})},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, args := buildObjectsOwnersQuery("test_schema", tc.objectType, tc.objects)
			if query != tc.wantQuery {
				t.Errorf("query mismatch\n got: %q\nwant: %q", query, tc.wantQuery)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args mismatch\n got: %#v\nwant: %#v", args, tc.wantArgs)
			}
		})
	}
}

// TestAccPostgresqlGrantSequenceOwnerWithoutTables grants on a sequence whose
// owner owns no table in the schema. For a non-superuser provider role the
// grant only works if the provider temporarily becomes a member of the
// sequence owner, so the owner must be looked up among sequences, not tables.
func TestAccPostgresqlGrantSequenceOwnerWithoutTables(t *testing.T) {
	skipIfNotAcc(t)

	// The owner role is created first so it is dropped after the database.
	seqOwner := fmt.Sprintf("%s_seq_owner_%d", roleNamePrefix, time.Now().UnixNano())
	defer createTestRole(t, seqOwner)()

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, roleName := getTestDBNames(dbSuffix)
	config := getTestConfig(t)
	dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA test_schema TO %s", seqOwner))

	sequences := []string{"test_schema.owned_seq"}
	defer createTestSequences(t, dbSuffix, sequences, seqOwner)()

	for _, objects := range []string{`["owned_seq"]`, `[]`} {
		tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "test" {
	database    = "%s"
	role        = "%s"
	schema      = "test_schema"
	object_type = "sequence"
	objects     = %s
	privileges  = ["USAGE", "SELECT"]
}`, dbName, roleName, objects)

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
						resource.TestCheckResourceAttr("postgresql_grant.test", "privileges.#", "2"),
						func(*terraform.State) error {
							return testCheckSequencePrivileges(t, dbName, roleName, "test_schema.owned_seq", true)
						},
					),
				},
				{
					Config:  tfConfig,
					Destroy: true,
					Check: func(*terraform.State) error {
						return testCheckSequencePrivileges(t, dbName, roleName, "test_schema.owned_seq", false)
					},
				},
			},
		})
	}
}

func testCheckSequencePrivileges(t *testing.T, dbName, roleName, sequence string, allowed bool) error {
	db := connectAsTestRole(t, roleName, dbName)
	defer closeDB(t, db)

	return testHasGrantForQuery(db, fmt.Sprintf("SELECT nextval('%s')", sequence), allowed)
}
