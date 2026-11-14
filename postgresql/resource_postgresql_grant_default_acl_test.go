package postgresql

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// TestGrantReadQueriesUseDefaultACL checks that every grant Read query
// explodes the default ACL of an object whose ACL is NULL (never granted or
// revoked), not the NULL itself.
func TestGrantReadQueriesUseDefaultACL(t *testing.T) {
	noObjects := schema.NewSet(schema.HashString, nil)

	tableQuery, _ := buildReadTableRolePrivilegesQuery("r", "s", "r", noObjects)
	sequenceQuery, _ := buildReadTableRolePrivilegesQuery("r", "s", "S", noObjects)
	functionQuery, _ := buildReadFunctionRolePrivilegesQuery(
		"r", "s", "", schema.NewSet(schema.HashString, []any{"f", "f(integer)"}), true,
	)
	wholeSchemaFunctionQuery, _ := buildReadFunctionRolePrivilegesQuery("r", "s", "", noObjects, true)

	singleObject := func(objectType string, objects ...any) string {
		d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{
			"role": "r", "database": "db", "schema": "s", "object_type": objectType,
			"objects": objects, "privileges": []any{},
		})
		query, _ := buildReadSingleObjectRolePrivilegesQuery(d)
		return query
	}

	for name, tc := range map[string]struct{ query, want string }{
		"table":     {tableQuery, "aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner)))"},
		"sequence":  {sequenceQuery, "aclexplode(COALESCE(c.relacl, acldefault('s', c.relowner)))"},
		"function":  {functionQuery, "aclexplode(COALESCE(o.proacl, acldefault('f', o.proowner)))"},
		"functions": {wholeSchemaFunctionQuery, "aclexplode(COALESCE(o.proacl, acldefault('f', o.proowner)))"},
		"schema":    {singleObject("schema"), "aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner)))"},
		"database":  {singleObject("database"), "aclexplode(COALESCE(o.datacl, pg_catalog.acldefault('d', o.datdba)))"},
		"fdw":       {singleObject("foreign_data_wrapper", "w"), "aclexplode(COALESCE(o.fdwacl, pg_catalog.acldefault('F', o.fdwowner)))"},
		"server":    {singleObject("foreign_server", "srv"), "aclexplode(COALESCE(o.srvacl, pg_catalog.acldefault('S', o.srvowner)))"},
	} {
		if !strings.Contains(tc.query, tc.want) {
			t.Errorf("%s read query does not contain %q:\n%s", name, tc.want, tc.query)
		}
	}
}

// singleRowPrivilegesForTest runs a Read query returning one row
// (role_exists, schema_exists, privileges) and returns the sorted privileges.
func singleRowPrivilegesForTest(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	var roleExists, schemaExists bool
	var privileges pq.ByteaArray
	if err := db.QueryRow(query, args...).Scan(&roleExists, &schemaExists, &privileges); err != nil {
		t.Fatalf("could not run read query: %v", err)
	}
	if !roleExists || !schemaExists {
		t.Fatalf("role_exists, schema_exists = %t, %t", roleExists, schemaExists)
	}
	got := []string{}
	for _, p := range privileges {
		got = append(got, string(p))
	}
	sort.Strings(got)
	return got
}

// tablePrivilegesForTest runs the table/sequence Read query and returns
// relation name → sorted privileges.
func tablePrivilegesForTest(t *testing.T, db *sql.DB, role, relkind string, objects ...any) map[string][]string {
	t.Helper()
	query, args := buildReadTableRolePrivilegesQuery(role, "test_schema", relkind, schema.NewSet(schema.HashString, objects))
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("could not run read query: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("could not close rows: %v", err)
		}
	}()
	got := map[string][]string{}
	for rows.Next() {
		var roleExists, schemaExists bool
		var name sql.NullString
		var privileges pq.ByteaArray
		if err := rows.Scan(&roleExists, &schemaExists, &name, &privileges); err != nil {
			t.Fatalf("could not scan row: %v", err)
		}
		if !name.Valid {
			continue
		}
		privs := []string{}
		for _, p := range privileges {
			privs = append(privs, string(p))
		}
		sort.Strings(privs)
		got[name.String] = privs
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("could not read rows: %v", err)
	}
	return got
}

// TestAccPostgresqlGrantReadDefaultACL checks, on a real catalog, that objects
// with a NULL ACL report their default privileges: EXECUTE for PUBLIC on
// functions, CONNECT/TEMPORARY for PUBLIC on databases, everything for the
// owner, nothing for anybody else.
func TestAccPostgresqlGrantReadDefaultACL(t *testing.T) {
	skipIfNotAcc(t)

	config := getTestConfig(t)
	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, roleName := getTestDBNames(dbSuffix)

	db, err := sql.Open("postgres", config.connStr(dbName))
	if err != nil {
		t.Fatalf("could not open connection pool for db %s: %v", dbName, err)
	}
	defer closeDB(t, db)

	for _, stmt := range []string{
		"CREATE FUNCTION test_schema.f() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.f_revoked() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"REVOKE EXECUTE ON FUNCTION test_schema.f_revoked() FROM PUBLIC",
		"CREATE TABLE test_schema.t (id int)",
		"CREATE SEQUENCE test_schema.seq",
		"CREATE SCHEMA owned_schema",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("could not execute %q: %v", stmt, err)
		}
	}
	var owner string
	if err := db.QueryRow("SELECT current_user").Scan(&owner); err != nil {
		t.Fatal(err)
	}

	read := func(role, objectType string, objects ...any) map[string][]string {
		got, _ := readFunctionPrivilegesForTest(
			t, db, role, "test_schema", objectType, objects, true, true,
		)
		return got
	}
	const e = "EXECUTE"
	if got, want := read("public", "function", "f", "f()", "f_revoked"), map[string][]string{
		"f": {e}, "f()": {e}, "f_revoked": {""},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("PUBLIC on functions: got %v, want %v", got, want)
	}
	if got, want := read(roleName, "function", "f"), map[string][]string{"f": {""}}; !reflect.DeepEqual(got, want) {
		t.Errorf("other role on functions: got %v, want %v", got, want)
	}
	if got, want := read(owner, "function", "f", "f_revoked"), map[string][]string{"f": {e}, "f_revoked": {e}}; !reflect.DeepEqual(got, want) {
		t.Errorf("owner on functions: got %v, want %v", got, want)
	}
	if got, want := read("public", "function"), map[string][]string{"f": {e}, "f_revoked": {""}}; !reflect.DeepEqual(got, want) {
		t.Errorf("PUBLIC on all functions: got %v, want %v", got, want)
	}

	allTable := []string{"DELETE", "INSERT", "REFERENCES", "SELECT", "TRIGGER", "TRUNCATE", "UPDATE"}
	if got := tablePrivilegesForTest(t, db, owner, "r", "t")["t"]; !reflect.DeepEqual(got, allTable) {
		t.Errorf("owner on table: got %v, want %v", got, allTable)
	}
	if got := tablePrivilegesForTest(t, db, "public", "r", "t")["t"]; len(got) != 0 {
		t.Errorf("PUBLIC on table: got %v, want none", got)
	}
	if got, want := tablePrivilegesForTest(t, db, owner, "S")["seq"], []string{"SELECT", "UPDATE", "USAGE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("owner on sequence: got %v, want %v", got, want)
	}

	singleObject := func(role, objectType, schemaName string) []string {
		d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{
			"role": role, "database": dbName, "schema": schemaName, "object_type": objectType,
			"privileges": []any{},
		})
		query, args := buildReadSingleObjectRolePrivilegesQuery(d)
		return singleRowPrivilegesForTest(t, db, query, args)
	}
	if got, want := singleObject("public", "database", ""), []string{"CONNECT", "TEMPORARY"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PUBLIC on database: got %v, want %v", got, want)
	}
	if got, want := singleObject(owner, "schema", "owned_schema"), []string{"CREATE", "USAGE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("owner on schema: got %v, want %v", got, want)
	}
	if got := singleObject("public", "schema", "owned_schema"); len(got) != 0 {
		t.Errorf("PUBLIC on schema: got %v, want none", got)
	}
}

// TestAccPostgresqlGrantPublicFunctionDefaultACL checks end to end that a
// function whose ACL is NULL counts as granted to PUBLIC: a PUBLIC EXECUTE
// grant does not drift when the function is recreated, and a config revoking
// it from PUBLIC does.
func TestAccPostgresqlGrantPublicFunctionDefaultACL(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, _ := getTestDBNames(dbSuffix)
	dsn := config.connStr(dbName)

	for _, stmt := range []string{
		"CREATE FUNCTION test_schema.g() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.h() RETURNS int LANGUAGE sql AS 'SELECT 1'",
	} {
		dbExecute(t, dsn, stmt)
	}

	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "public_execute" {
	database    = "%[1]s"
	role        = "public"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["g"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_grant" "public_revoked" {
	database    = "%[1]s"
	role        = "public"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["h"]
	privileges  = []
}
`, dbName)

	recreate := func(name string) func() {
		return func() {
			dbExecute(t, dsn, fmt.Sprintf("DROP FUNCTION test_schema.%s()", name))
			dbExecute(t, dsn, fmt.Sprintf("CREATE FUNCTION test_schema.%s() RETURNS int LANGUAGE sql AS 'SELECT 1'", name))
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{Config: tfConfig},
			{
				// g() recreated: NULL ACL, PUBLIC has EXECUTE by default.
				PreConfig: recreate("g"),
				Config:    tfConfig,
				PlanOnly:  true,
			},
			{
				// h() recreated: PUBLIC has EXECUTE again, the config revokes it.
				PreConfig:          recreate("h"),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { dbExecute(t, dsn, "REVOKE EXECUTE ON FUNCTION test_schema.h() FROM PUBLIC") },
				Config:    tfConfig,
				PlanOnly:  true,
			},
		},
	})
}
