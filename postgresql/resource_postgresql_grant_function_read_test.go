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

// TestBuildReadFunctionRolePrivilegesQuery locks in the function/procedure/
// routine Read query shape. With an empty objects set the query stays
// unfiltered and binds two arguments ("empty means every routine in the
// schema"). With an explicit objects list the proname filter is pushed into
// SQL as "proname = ANY($3)", binding every entry verbatim: an entry carrying
// an argument signature never equals a proname, exactly as it never matched
// the name-based filter in Go, so the rows kept are the same as before.
// pg_proc is scanned once and no prokind filter is added (the Read has never
// distinguished functions, procedures and aggregates).
func TestBuildReadFunctionRolePrivilegesQuery(t *testing.T) {
	emptyQuery, emptyArgs := buildReadFunctionRolePrivilegesQuery(
		"test_role", "test_schema", schema.NewSet(schema.HashString, nil),
	)
	if strings.Contains(emptyQuery, "ANY(") {
		t.Fatalf("empty objects must not push a proname filter, got: %q", emptyQuery)
	}
	if len(emptyArgs) != 2 {
		t.Fatalf("empty objects must bind 2 args, got %d: %#v", len(emptyArgs), emptyArgs)
	}

	entries := []any{"f", "f(integer)", "g(text, char)"}
	query, args := buildReadFunctionRolePrivilegesQuery(
		"test_role", "test_schema", schema.NewSet(schema.HashString, entries),
	)
	if !strings.Contains(query, "WHERE p.proname = ANY($3)") {
		t.Fatalf("explicit objects must push proname = ANY($3), got: %q", query)
	}
	if len(args) != 3 {
		t.Fatalf("explicit objects must bind 3 args, got %d: %#v", len(args), args)
	}
	bound, ok := args[2].(*pq.StringArray)
	if !ok {
		t.Fatalf("objects must be bound as a text array, got %T", args[2])
	}
	got := append([]string(nil), *bound...)
	sort.Strings(got)
	if want := []string{"f", "f(integer)", "g(text, char)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("objects must be bound verbatim: got %v, want %v", got, want)
	}

	for _, q := range []string{emptyQuery, query} {
		for _, want := range []string{
			"SELECT r.oid IS NOT NULL, EXISTS (SELECT 1 FROM n), o.proname, o.privileges",
			"WHEN $1::name = 'public' THEN 0::oid",
			"SELECT oid FROM pg_namespace WHERE nspname = $2",
			"SELECT p.proname, array_remove(array_agg(a.privilege_type), NULL) AS privileges",
			"LEFT JOIN LATERAL (\n        SELECT privilege_type FROM aclexplode(p.proacl) WHERE grantee = r.oid\n    ) a ON true",
			"GROUP BY p.proname",
		} {
			if !strings.Contains(q, want) {
				t.Fatalf("query missing %q:\n%s", want, q)
			}
		}
		if strings.Count(q, "pg_proc") != 1 {
			t.Fatalf("query must scan pg_proc once: %q", q)
		}
		if strings.Contains(q, "prokind") {
			t.Fatalf("query must not filter on prokind: %q", q)
		}
	}
}

// readFunctionPrivilegesForTest runs the function Read query and applies the
// same name filter as readRolePrivileges. It returns the kept rows (name →
// sorted privileges, duplicates kept) and the names of the rows the query
// returned but the Go filter dropped.
func readFunctionPrivilegesForTest(
	t *testing.T, db *sql.DB, role, schemaName string, objects []any, wantRoleExists, wantSchemaExists bool,
) (map[string][]string, []string) {
	t.Helper()

	objectSet := schema.NewSet(schema.HashString, objects)
	query, args := buildReadFunctionRolePrivilegesQuery(role, schemaName, objectSet)
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("could not run read query: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("could not close rows: %v", err)
		}
	}()

	kept := map[string][]string{}
	var dropped []string
	nRows := 0
	for rows.Next() {
		nRows++
		var roleExists, schemaExists bool
		var objName sql.NullString
		var privileges pq.ByteaArray
		if err := rows.Scan(&roleExists, &schemaExists, &objName, &privileges); err != nil {
			t.Fatalf("could not scan row: %v", err)
		}
		if roleExists != wantRoleExists || schemaExists != wantSchemaExists {
			t.Fatalf("role_exists, schema_exists = %t, %t; want %t, %t",
				roleExists, schemaExists, wantRoleExists, wantSchemaExists)
		}
		if !objName.Valid {
			continue
		}
		if objectSet.Len() > 0 && !objectSet.Contains(objName.String) {
			dropped = append(dropped, objName.String)
			continue
		}
		privs := make([]string, 0, len(privileges))
		for _, p := range privileges {
			privs = append(privs, string(p))
		}
		sort.Strings(privs)
		kept[objName.String] = privs
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("could not read rows: %v", err)
	}
	if nRows == 0 {
		t.Fatalf("query must always return at least one row (the existence flags)")
	}
	return kept, dropped
}

// TestAccPostgresqlGrantReadFunctionPrivilegesQuery pins what the function
// Read compares against the state, on a real catalog: one entry per routine
// name (overloads merged, their privileges concatenated), functions,
// procedures and aggregates alike, PUBLIC read as grantee 0. Listed bare names
// select every overload of that name; listed entries with an argument
// signature select nothing (they are matched by name, and no routine is named
// "f(integer)"), unless a routine really carries that exact name; missing
// entries yield no row.
func TestAccPostgresqlGrantReadFunctionPrivilegesQuery(t *testing.T) {
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
		// New routines get an explicit ACL without PUBLIC.
		"ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC",
		"CREATE FUNCTION test_schema.f(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.f(text) RETURNS int LANGUAGE sql AS 'SELECT 2'",
		"CREATE FUNCTION test_schema.f2(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.f2(text) RETURNS int LANGUAGE sql AS 'SELECT 2'",
		"CREATE FUNCTION test_schema.g() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.h() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		`CREATE FUNCTION test_schema."q(int)"() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		"CREATE PROCEDURE test_schema.p() LANGUAGE sql AS 'SELECT 1'",
		"CREATE AGGREGATE test_schema.agg(integer) (SFUNC = int4pl, STYPE = integer)",
		"CREATE FUNCTION dev_schema.f(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO %s", roleName),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f2(integer), test_schema.f2(text) TO %s", roleName),
		fmt.Sprintf(`GRANT EXECUTE ON FUNCTION test_schema."q(int)"() TO %s`, roleName),
		fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.p() TO %s", roleName),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.agg(integer) TO %s", roleName),
		"GRANT EXECUTE ON FUNCTION test_schema.h() TO PUBLIC",
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION dev_schema.f(integer) TO %s", roleName),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("could not execute %q: %v", stmt, err)
		}
	}

	cases := []struct {
		name         string
		role         string
		schema       string
		objects      []any
		roleExists   bool
		schemaExists bool
		expected     map[string][]string
	}{
		{
			name: "whole schema", role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			expected: map[string][]string{
				"f": {"EXECUTE"}, "f2": {"EXECUTE", "EXECUTE"}, "g": {}, "h": {},
				"q(int)": {"EXECUTE"}, "p": {"EXECUTE"}, "agg": {"EXECUTE"},
			},
		},
		{
			name: "bare names select every overload", role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f", "f2", "g"},
			expected: map[string][]string{"f": {"EXECUTE"}, "f2": {"EXECUTE", "EXECUTE"}, "g": {}},
		},
		{
			name: "signature entries select nothing", role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f(integer)", "f(text)", "f2(int4)"},
			expected: map[string][]string{},
		},
		{
			name: "mixed bare and signature entries", role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f(integer)", "g", "p"},
			expected: map[string][]string{"g": {}, "p": {"EXECUTE"}},
		},
		{
			name: "routine whose name contains parentheses", role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"q(int)", "q"},
			expected: map[string][]string{"q(int)": {"EXECUTE"}},
		},
		{
			name: "missing entries", role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"missing", "missing(integer)", "agg"},
			expected: map[string][]string{"agg": {"EXECUTE"}},
		},
		{
			name: "public", role: "public", schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"h", "g", "f"},
			expected: map[string][]string{"h": {"EXECUTE"}, "g": {}, "f": {}},
		},
		{
			name: "other schema", role: roleName, schema: "dev_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f", "g"},
			expected: map[string][]string{"f": {"EXECUTE"}},
		},
		{
			name: "missing role", role: roleName + "_missing", schema: "test_schema", roleExists: false, schemaExists: true,
			objects:  []any{"f"},
			expected: map[string][]string{"f": {}},
		},
		{
			name: "missing schema", role: roleName, schema: "missing_schema", roleExists: true, schemaExists: false,
			objects:  []any{"f"},
			expected: map[string][]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := readFunctionPrivilegesForTest(
				t, db, tc.role, tc.schema, tc.objects, tc.roleExists, tc.schemaExists,
			)
			if !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("unexpected read result:\n got: %v\nwant: %v", got, tc.expected)
			}
			// The objects filter runs in SQL: the Go filter is only a safety
			// net and must not have anything left to drop.
			if len(dropped) > 0 {
				t.Fatalf("query returned rows outside of objects %v: %v", tc.objects, dropped)
			}
		})
	}
}

// TestAccPostgresqlGrantFunctionOverloads pins, end to end, how a refresh of
// function/procedure/routine grants with explicit objects reacts to
// out-of-band changes: bare names are compared over all their overloads
// (merged), entries with an argument signature and entries whose routine was
// dropped are not compared, PUBLIC grants are read, and a plan right after
// apply is empty.
func TestAccPostgresqlGrantFunctionOverloads(t *testing.T) {
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
		"CREATE FUNCTION test_schema.k() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE PROCEDURE test_schema.p() LANGUAGE sql AS 'SELECT 1'",
		"CREATE PROCEDURE test_schema.p2() LANGUAGE sql AS 'SELECT 1'",
	} {
		dbExecute(t, dsn, stmt)
	}

	role := pq.QuoteIdentifier(roleName)
	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "function" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["f(integer)", "g"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_grant" "public" {
	database    = "%[1]s"
	role        = "public"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["g"]
	privileges  = ["EXECUTE"]

	# Both grants rewrite the ACL of g().
	depends_on = [postgresql_grant.function]
}

resource "postgresql_grant" "procedure" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "procedure"
	objects     = ["p"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_grant" "routine" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "routine"
	objects     = ["k", "p2"]
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
			testCheckCompatibleVersion(t, featureRoutine)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				// The framework also checks that a plan right after apply is
				// empty (no drift from the signature entry).
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.function", "objects.#", "2"),
					resource.TestCheckResourceAttr("postgresql_grant.function", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.routine", "objects.#", "2"),
				),
			},
			{
				// Current behavior, not a goal: an entry with an argument
				// signature is not compared on refresh, so revoking it
				// out-of-band is not seen.
				PreConfig: exec(fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.f(integer) FROM %s", role)),
				Config:    tfConfig,
				PlanOnly:  true,
			},
			{
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO %s", role),
					fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.g() FROM %s", role),
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.g() TO %s", role),
					fmt.Sprintf("REVOKE EXECUTE ON PROCEDURE test_schema.p() FROM %s", role),
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.p() TO %s", role),
					"REVOKE EXECUTE ON FUNCTION test_schema.g() FROM PUBLIC",
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// A listed routine that was dropped is not compared; the
				// remaining one still is (and still matches).
				PreConfig: exec(
					"GRANT EXECUTE ON FUNCTION test_schema.g() TO PUBLIC",
					"DROP FUNCTION test_schema.k()",
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
			{
				PreConfig:          exec(fmt.Sprintf("REVOKE EXECUTE ON PROCEDURE test_schema.p2() FROM %s", role)),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// A bare name covers every overload, merged: a new overload
				// without the privilege does not show as drift ...
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.p2() TO %s", role),
					"CREATE FUNCTION test_schema.g(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
			{
				// ... until no overload of the name has it any more.
				PreConfig:          exec(fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.g() FROM %s", role)),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Restore a state the destroy can revoke ("g" must resolve to
				// a single function again).
				PreConfig: exec(
					"DROP FUNCTION test_schema.g(integer)",
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.g() TO %s", role),
					"CREATE FUNCTION test_schema.k() RETURNS int LANGUAGE sql AS 'SELECT 1'",
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.k() TO %s", role),
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
		},
	})
}
