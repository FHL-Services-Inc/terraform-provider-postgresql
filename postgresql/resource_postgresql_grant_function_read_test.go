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

func TestFunctionProkindFilter(t *testing.T) {
	cases := []struct {
		objectType string
		hasProkind bool
		expected   string
	}{
		// GRANT ON FUNCTION / ALL FUNCTIONS covers plain, aggregate and
		// window functions, but not procedures.
		{"function", true, "p.prokind <> 'p'"},
		{"procedure", true, "p.prokind = 'p'"},
		{"routine", true, ""},
		// Before PostgreSQL 11 there are no procedures (and no prokind).
		{"function", false, ""},
	}
	for _, tc := range cases {
		if got := functionProkindFilter(tc.objectType, tc.hasProkind); got != tc.expected {
			t.Errorf("functionProkindFilter(%q, %t) = %q, want %q", tc.objectType, tc.hasProkind, got, tc.expected)
		}
	}
}

func TestQuoteIdentifyIdentKeepsWholeSignature(t *testing.T) {
	cases := map[string]string{
		"f":                            `"f"`,
		"f()":                          `"f"()`,
		"test(text, char)":             `"test"(text, char)`,
		"h(numeric(10,2))":             `"h"(numeric(10,2))`,
		"g(varchar(3), numeric(10,2))": `"g"(varchar(3), numeric(10,2))`,
	}
	for in, want := range cases {
		if got := quoteIdentifyIdent(in); got != want {
			t.Errorf("quoteIdentifyIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildReadFunctionRolePrivilegesQuery locks in the function/procedure/
// routine Read query shape: one row per routine (overload), bare entries
// matched by name through the pg_proc name index, entries carrying an argument
// signature resolved with to_regprocedure on the same rendering as the GRANT,
// and the prokind filter of the object type.
func TestBuildReadFunctionRolePrivilegesQuery(t *testing.T) {
	commonParts := []string{
		"SELECT r.oid IS NOT NULL, EXISTS (SELECT 1 FROM n), o.name, o.privileges",
		"WHEN $1::name = 'public' THEN 0::oid",
		"SELECT oid FROM pg_namespace WHERE nspname = $2",
		"SELECT privilege_type FROM aclexplode(COALESCE(o.proacl, acldefault('f', o.proowner))) WHERE grantee = r.oid",
		"GROUP BY o.name, o.oid",
	}
	checkCommon := func(t *testing.T, q string) {
		t.Helper()
		for _, want := range commonParts {
			if !strings.Contains(q, want) {
				t.Fatalf("query missing %q:\n%s", want, q)
			}
		}
	}

	t.Run("whole schema", func(t *testing.T) {
		query, args := buildReadFunctionRolePrivilegesQuery(
			"test_role", "test_schema", "p.prokind <> 'p'", schema.NewSet(schema.HashString, nil), true,
		)
		checkCommon(t, query)
		if len(args) != 2 {
			t.Fatalf("empty objects must bind 2 args, got %d: %#v", len(args), args)
		}
		for _, unwanted := range []string{"ANY(", "to_regprocedure", "unnest"} {
			if strings.Contains(query, unwanted) {
				t.Fatalf("empty objects must not contain %q:\n%s", unwanted, query)
			}
		}
		if !strings.Contains(query, "WHERE p.prokind <> 'p'") {
			t.Fatalf("whole schema read must apply the prokind filter:\n%s", query)
		}
		if strings.Count(query, "pg_proc") != 1 {
			t.Fatalf("whole schema read must scan pg_proc once:\n%s", query)
		}
	})

	t.Run("no prokind filter", func(t *testing.T) {
		query, _ := buildReadFunctionRolePrivilegesQuery(
			"test_role", "test_schema", "", schema.NewSet(schema.HashString, []any{"f", "f(integer)"}), true,
		)
		checkCommon(t, query)
		if strings.Contains(query, "prokind") {
			t.Fatalf("routine read must not filter on prokind:\n%s", query)
		}
	})

	entries := []any{"f", "f(integer)", "g(text, char)", "h(numeric(10,2))", "k"}
	objects := schema.NewSet(schema.HashString, entries)

	t.Run("listed objects", func(t *testing.T) {
		query, args := buildReadFunctionRolePrivilegesQuery(
			"test_role", "test schema", "p.prokind = 'p'", objects, true,
		)
		checkCommon(t, query)
		for _, want := range []string{
			"p.proname = ANY($3) AND p.prokind = 'p'",
			"FROM unnest($4::text[], $5::text[]) AS e(entry, signature)",
			"JOIN pg_proc p ON p.oid = to_regprocedure(e.signature)",
			"WHERE p.prokind = 'p'",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("query missing %q:\n%s", want, query)
			}
		}
		if len(args) != 5 {
			t.Fatalf("listed objects must bind 5 args, got %d: %#v", len(args), args)
		}
		bare := append([]string(nil), *args[2].(*pq.StringArray)...)
		sort.Strings(bare)
		if want := []string{"f", "k"}; !reflect.DeepEqual(bare, want) {
			t.Fatalf("bare entries: got %v, want %v", bare, want)
		}
		sigEntries := *args[3].(*pq.StringArray)
		signatures := *args[4].(*pq.StringArray)
		if len(sigEntries) != len(signatures) {
			t.Fatalf("entries and signatures must be parallel: %v / %v", sigEntries, signatures)
		}
		got := map[string]string{}
		for i := range sigEntries {
			got[sigEntries[i]] = signatures[i]
		}
		want := map[string]string{
			"f(integer)":       `"test schema"."f"(integer)`,
			"g(text, char)":    `"test schema"."g"(text, char)`,
			"h(numeric(10,2))": `"test schema"."h"(numeric(10,2))`,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("signatures: got %v, want %v", got, want)
		}
		// The signature is rendered exactly like the GRANT renders it.
		for entry, sig := range got {
			if wantSig := setToPgIdentList("test schema", schema.NewSet(schema.HashString, []any{entry})); sig != wantSig {
				t.Fatalf("signature of %q = %q, the GRANT uses %q", entry, sig, wantSig)
			}
		}
	})

	t.Run("signatures skipped", func(t *testing.T) {
		query, args := buildReadFunctionRolePrivilegesQuery(
			"test_role", "test_schema", "p.prokind <> 'p'",
			schema.NewSet(schema.HashString, []any{"f(integer)", "g(text)"}), false,
		)
		checkCommon(t, query)
		if strings.Contains(query, "to_regprocedure") {
			t.Fatalf("signatures must not be resolved:\n%s", query)
		}
		// Only signature entries: the bare filter stays (and selects
		// nothing), it must not turn into a whole schema read.
		if !strings.Contains(query, "p.proname = ANY($3)") || len(args) != 3 {
			t.Fatalf("listed objects must keep the name filter: %d args\n%s", len(args), query)
		}
		if n := len(*args[2].(*pq.StringArray)); n != 0 {
			t.Fatalf("no bare entry expected, got %d", n)
		}
	})
}

// readFunctionPrivilegesForTest runs the function Read query and applies the
// same objects filter as readRolePrivileges. It returns the kept rows as
// entry → sorted list of per-routine privileges (each one sorted and joined
// with ","), and the names of the rows the Go filter dropped.
func readFunctionPrivilegesForTest(
	t *testing.T, db *sql.DB, role, schemaName, objectType string, objects []any, wantRoleExists, wantSchemaExists bool,
) (map[string][]string, []string) {
	t.Helper()

	objectSet := schema.NewSet(schema.HashString, objects)
	query, args := buildReadFunctionRolePrivilegesQuery(
		role, schemaName, functionProkindFilter(objectType, true), objectSet, true,
	)
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
		kept[objName.String] = append(kept[objName.String], strings.Join(privs, ","))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("could not read rows: %v", err)
	}
	if nRows == 0 {
		t.Fatalf("query must always return at least one row (the existence flags)")
	}
	for _, v := range kept {
		sort.Strings(v)
	}
	return kept, dropped
}

// TestAccPostgresqlGrantReadFunctionPrivilegesQuery pins what the function
// Read compares against the state, on a real catalog: one row per routine
// (overload), only the kinds the object type grants on, PUBLIC read as grantee
// 0. Bare entries select every overload of that name; entries with an argument
// signature select that overload, however its types are spelled, and are
// reported under the entry as written; unknown entries yield no row.
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
		"CREATE PROCEDURE test_schema.f(boolean) LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.f2(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.f2(text) RETURNS int LANGUAGE sql AS 'SELECT 2'",
		"CREATE FUNCTION test_schema.g() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.h() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.v(character varying, numeric) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		`CREATE FUNCTION test_schema."q(int)"() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		"CREATE PROCEDURE test_schema.p() LANGUAGE sql AS 'SELECT 1'",
		"CREATE AGGREGATE test_schema.agg(integer) (SFUNC = int4pl, STYPE = integer)",
		"CREATE FUNCTION dev_schema.f(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO %s", roleName),
		fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.f(boolean) TO %s", roleName),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f2(integer), test_schema.f2(text) TO %s", roleName),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.v(varchar, numeric) TO %s", roleName),
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

	const e = "EXECUTE"
	cases := []struct {
		name         string
		objectType   string
		role         string
		schema       string
		objects      []any
		roleExists   bool
		schemaExists bool
		expected     map[string][]string
	}{
		{
			name: "whole schema, functions", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			expected: map[string][]string{
				"f": {"", e}, "f2": {e, e}, "g": {""}, "h": {""}, "v": {e},
				"q(int)": {e}, "agg": {e},
			},
		},
		{
			name: "whole schema, procedures", objectType: "procedure",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			expected: map[string][]string{"f": {e}, "p": {e}},
		},
		{
			name: "whole schema, routines", objectType: "routine",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			expected: map[string][]string{
				"f": {"", e, e}, "f2": {e, e}, "g": {""}, "h": {""}, "v": {e},
				"q(int)": {e}, "agg": {e}, "p": {e},
			},
		},
		{
			name: "bare names select every overload", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f", "f2", "g"},
			expected: map[string][]string{"f": {"", e}, "f2": {e, e}, "g": {""}},
		},
		{
			name: "signature entries select their overload", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f(integer)", "f(text)", "f2(int4)", "agg(integer)", "g()"},
			expected: map[string][]string{"f(integer)": {e}, "f(text)": {""}, "f2(int4)": {e}, "agg(integer)": {e}, "g()": {""}},
		},
		{
			name: "signature spellings", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects: []any{
				"f(int4)", "f(  integer )", "f(pg_catalog.int4)", "f2(TEXT)",
				"v(varchar,numeric)", "v(character varying(10), numeric(10,2))",
			},
			expected: map[string][]string{
				"f(int4)": {e}, "f(  integer )": {e}, "f(pg_catalog.int4)": {e}, "f2(TEXT)": {e},
				"v(varchar,numeric)": {e}, "v(character varying(10), numeric(10,2))": {e},
			},
		},
		{
			name: "mixed bare and signature entries", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f(integer)", "g", "p", "f(boolean)"},
			expected: map[string][]string{"f(integer)": {e}, "g": {""}},
		},
		{
			name: "procedure and function with the same name", objectType: "procedure",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f", "f(boolean)", "f(integer)", "p()", "agg(integer)"},
			expected: map[string][]string{"f": {e}, "f(boolean)": {e}, "p()": {e}},
		},
		{
			name: "routine entries", objectType: "routine",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f", "f(boolean)", "f(text)", "p", "agg(int4)"},
			expected: map[string][]string{"f": {"", e, e}, "f(boolean)": {e}, "f(text)": {""}, "p": {e}, "agg(int4)": {e}},
		},
		{
			// The GRANT renders "q(int)" as "q"(int), so the read does too.
			name: "routine whose name contains parentheses", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"q(int)", "q"},
			expected: map[string][]string{},
		},
		{
			name: "missing entries", objectType: "function",
			role: roleName, schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"missing", "missing(integer)", "agg(text)", "F(integer)", "agg"},
			expected: map[string][]string{"agg": {e}},
		},
		{
			name: "public", objectType: "function",
			role: "public", schema: "test_schema", roleExists: true, schemaExists: true,
			objects:  []any{"h", "g", "f", "h()", "f(integer)"},
			expected: map[string][]string{"h": {e}, "g": {""}, "f": {"", ""}, "h()": {e}, "f(integer)": {""}},
		},
		{
			name: "other schema", objectType: "function",
			role: roleName, schema: "dev_schema", roleExists: true, schemaExists: true,
			objects:  []any{"f", "g", "f(integer)", "f(text)"},
			expected: map[string][]string{"f": {e}, "f(integer)": {e}},
		},
		{
			name: "missing role", objectType: "function",
			role: roleName + "_missing", schema: "test_schema", roleExists: false, schemaExists: true,
			objects:  []any{"f", "f(integer)"},
			expected: map[string][]string{"f": {"", ""}, "f(integer)": {""}},
		},
		{
			name: "missing schema", objectType: "function",
			role: roleName, schema: "missing_schema", roleExists: true, schemaExists: false,
			objects:  []any{"f", "f(integer)"},
			expected: map[string][]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := readFunctionPrivilegesForTest(
				t, db, tc.role, tc.schema, tc.objectType, tc.objects, tc.roleExists, tc.schemaExists,
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

// TestAccPostgresqlGrantFunctionOverloads checks, end to end, how a refresh of
// function/procedure/routine grants reacts to out-of-band changes: entries
// with an argument signature are compared against that overload (whatever the
// spelling), bare names against every overload of the object type, a whole
// schema grant against every routine of its kind, entries whose routine was
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
		"CREATE FUNCTION test_schema.sp(integer, text) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION test_schema.m(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE AGGREGATE test_schema.agg(integer) (SFUNC = int4pl, STYPE = integer)",
		"CREATE PROCEDURE test_schema.p() LANGUAGE sql AS 'SELECT 1'",
		"CREATE PROCEDURE test_schema.p2() LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION dev_schema.df() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE PROCEDURE dev_schema.dp() LANGUAGE sql AS 'SELECT 1'",
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

resource "postgresql_grant" "spelling" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["sp( int4 ,  pg_catalog.text )", "agg(int4)"]
	privileges  = ["EXECUTE"]
}

# The read cannot resolve an entry with argument names; it must not fail.
resource "postgresql_grant" "named_args" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["m(x integer)"]
	privileges  = ["EXECUTE"]
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

# Every function of dev_schema, not its procedure.
resource "postgresql_grant" "all_functions" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "dev_schema"
	object_type = "function"
	objects     = []
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
				// empty: no drift from the signature entries (whatever their
				// spelling), from the entry it cannot resolve, nor from the
				// procedure next to the whole schema function grant.
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.function", "objects.#", "2"),
					resource.TestCheckResourceAttr("postgresql_grant.function", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.spelling", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.named_args", "privileges.#", "1"),
					resource.TestCheckResourceAttr("postgresql_grant.routine", "objects.#", "2"),
					resource.TestCheckResourceAttr("postgresql_grant.all_functions", "privileges.#", "1"),
				),
			},
			{
				// An entry with an argument signature is compared against
				// that overload.
				PreConfig:          exec(fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.f(integer) FROM %s", role)),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// ... whatever its spelling.
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO %s", role),
					fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.sp(integer, text) FROM %s", role),
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Aggregates are granted and read as functions.
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.sp(integer, text) TO %s", role),
					fmt.Sprintf("REVOKE EXECUTE ON FUNCTION test_schema.agg(integer) FROM %s", role),
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// A signature entry whose routine was dropped is not compared
				// (like a dropped bare name below) ...
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.agg(integer) TO %s", role),
					"DROP FUNCTION test_schema.sp(integer, text)",
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
			{
				// ... but once recreated without the privilege, it is.
				PreConfig:          exec("CREATE FUNCTION test_schema.sp(integer, text) RETURNS int LANGUAGE sql AS 'SELECT 1'"),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.sp(integer, text) TO %s", role),
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
				// A function with the same name as a listed procedure is not
				// part of the procedure grant.
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.p() TO %s", role),
					"CREATE FUNCTION test_schema.p(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
			{
				PreConfig: exec(
					"DROP FUNCTION test_schema.p(integer)",
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
				// A bare name covers every overload: a new overload without
				// the privilege is drift ...
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON PROCEDURE test_schema.p2() TO %s", role),
					"CREATE FUNCTION test_schema.g(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'",
				),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// ... until every overload has it.
				PreConfig: exec(
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.g(integer) TO %s, PUBLIC", role),
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
			{
				// Same for a whole schema grant: a new function without the
				// privilege is drift.
				PreConfig:          exec("CREATE FUNCTION dev_schema.df(integer) RETURNS int LANGUAGE sql AS 'SELECT 1'"),
				Config:             tfConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Restore a state the destroy can revoke ("g" must resolve to
				// a single function again).
				PreConfig: exec(
					"DROP FUNCTION dev_schema.df(integer)",
					"DROP FUNCTION test_schema.g(integer)",
					"CREATE FUNCTION test_schema.k() RETURNS int LANGUAGE sql AS 'SELECT 1'",
					fmt.Sprintf("GRANT EXECUTE ON FUNCTION test_schema.k() TO %s", role),
				),
				Config:   tfConfig,
				PlanOnly: true,
			},
		},
	})
}
