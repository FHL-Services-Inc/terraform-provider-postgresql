package postgresql

import (
	"reflect"
	"strings"
	"testing"
)

// allowedForTest is the privilege table used to expand ALL in the tests
// (PostgreSQL < 17: no MAINTAIN).
func allowedForTest(kind string) []string {
	switch kind {
	case "table", "default:table":
		return []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	case "sequence", "default:sequence":
		return []string{"USAGE", "SELECT", "UPDATE"}
	case "schema":
		return []string{"CREATE", "USAGE"}
	case "default:function":
		return []string{"EXECUTE"}
	case "default:type":
		return []string{"USAGE"}
	}
	return nil
}

func privs(entries ...string) privilegeSet {
	return parsePrivileges(entries)
}

func TestParsePrivileges(t *testing.T) {
	got := parsePrivileges([]string{"SELECT", "INSERT*"})
	want := privilegeSet{"SELECT": false, "INSERT": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(parsePrivileges(nil)) != 0 {
		t.Fatal("nil must parse to an empty set")
	}
}

func TestExpandDesired_AllTablesCoversCurrentObjectsOnly(t *testing.T) {
	cfg := rolePrivilegesConfig{schemas: []schemaPrivilegesConfig{{
		name: "public", usage: true,
		tables: []objectPrivilegesConfig{{privileges: []string{"SELECT"}}},
	}}}
	actual := aclState{
		{kind: "schema", schema: "public"}:               privs(),
		{kind: "table", schema: "public", name: "a"}:     privs(),
		{kind: "table", schema: "public", name: "b"}:     privs("SELECT"),
		{kind: "sequence", schema: "public", name: "s1"}: privs("USAGE"),
	}
	desired, scope, problems := expandDesired(cfg, actual, allowedForTest)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	want := aclState{
		{kind: "schema", schema: "public"}:           privs("USAGE"),
		{kind: "table", schema: "public", name: "a"}: privs("SELECT"),
		{kind: "table", schema: "public", name: "b"}: privs("SELECT"),
	}
	if !reflect.DeepEqual(desired, want) {
		t.Fatalf("desired = %v, want %v", desired, want)
	}
	// The sequence is not covered by the config, so it is out of scope.
	if scope[aclObject{kind: "sequence", schema: "public", name: "s1"}] {
		t.Fatal("sequence must not be in scope")
	}
	for obj := range want {
		if !scope[obj] {
			t.Fatalf("%v must be in scope", obj)
		}
	}
}

func TestExpandDesired_UnionOfOverlappingBlocksAndALL(t *testing.T) {
	cfg := rolePrivilegesConfig{schemas: []schemaPrivilegesConfig{{
		name: "public",
		tables: []objectPrivilegesConfig{
			{privileges: []string{"SELECT"}},
			{privileges: []string{"INSERT", "UPDATE"}, objects: []string{"orders"}},
		},
		sequences: []objectPrivilegesConfig{{privileges: []string{"ALL"}, objects: []string{"s1"}, grantOption: true}},
	}}}
	actual := aclState{
		{kind: "schema", schema: "public"}:                privs(),
		{kind: "table", schema: "public", name: "orders"}: privs(),
		{kind: "table", schema: "public", name: "items"}:  privs(),
		{kind: "sequence", schema: "public", name: "s1"}:  privs(),
	}
	desired, _, problems := expandDesired(cfg, actual, allowedForTest)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	want := aclState{
		{kind: "schema", schema: "public"}:                privs(),
		{kind: "table", schema: "public", name: "orders"}: privs("SELECT", "INSERT", "UPDATE"),
		{kind: "table", schema: "public", name: "items"}:  privs("SELECT"),
		{kind: "sequence", schema: "public", name: "s1"}:  privs("USAGE*", "SELECT*", "UPDATE*"),
	}
	if !reflect.DeepEqual(desired, want) {
		t.Fatalf("desired = %v, want %v", desired, want)
	}
}

func TestExpandDesired_ReportsMissingSchemaAndObject(t *testing.T) {
	cfg := rolePrivilegesConfig{schemas: []schemaPrivilegesConfig{
		{name: "gone", usage: true},
		{name: "public", tables: []objectPrivilegesConfig{{privileges: []string{"SELECT"}, objects: []string{"nope"}}}},
	}}
	actual := aclState{{kind: "schema", schema: "public"}: privs()}
	_, _, problems := expandDesired(cfg, actual, allowedForTest)
	want := []string{"schema gone: does not exist", "table public.nope: does not exist"}
	if !reflect.DeepEqual(problems, want) {
		t.Fatalf("problems = %v, want %v", problems, want)
	}
}

func TestExpandDesired_DefaultPrivilegesAndConflictingGrantOption(t *testing.T) {
	cfg := rolePrivilegesConfig{schemas: []schemaPrivilegesConfig{{
		name: "public",
		tables: []objectPrivilegesConfig{
			{privileges: []string{"SELECT"}, objects: []string{"a"}},
			{privileges: []string{"SELECT"}, objects: []string{"a"}, grantOption: true},
		},
		defaults: []defaultPrivilegesConfig{{owner: "own", objectType: "table", privileges: []string{"SELECT"}}},
	}}}
	actual := aclState{
		{kind: "schema", schema: "public"}:           privs(),
		{kind: "table", schema: "public", name: "a"}: privs(),
	}
	desired, scope, problems := expandDesired(cfg, actual, allowedForTest)
	if want := []string{"table public.a: with_grant_option differs between blocks"}; !reflect.DeepEqual(problems, want) {
		t.Fatalf("problems = %v, want %v", problems, want)
	}
	def := aclObject{kind: "default:table", schema: "public", name: "own"}
	if !reflect.DeepEqual(desired[def], privs("SELECT")) || !scope[def] {
		t.Fatalf("default privileges entry missing: desired=%v scope=%v", desired[def], scope[def])
	}
}

func TestDiffACL_GrantRevokeAndGrantOption(t *testing.T) {
	a := aclObject{kind: "table", schema: "public", name: "a"}
	b := aclObject{kind: "table", schema: "public", name: "b"}
	c := aclObject{kind: "table", schema: "public", name: "c"}
	desired := aclState{a: privs("SELECT", "INSERT*"), b: privs("SELECT")}
	actual := aclState{a: privs("SELECT*", "DELETE"), b: privs("SELECT"), c: privs("SELECT")}
	scope := map[aclObject]bool{a: true, b: true}

	changes := diffACL(desired, actual, scope)
	want := []aclChange{{
		obj:          a,
		grant:        privilegeSet{"INSERT": true},
		revoke:       privilegeSet{"DELETE": false},
		revokeOption: privilegeSet{"SELECT": false},
	}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %#v, want %#v", changes, want)
	}
	// c is out of scope: untouched.
	if drift := driftEntries(changes, nil); !reflect.DeepEqual(drift, []string{"table public.a: +DELETE +SELECT* -INSERT*"}) {
		t.Fatalf("drift = %v", drift)
	}
}

func TestDiffACL_ExclusiveRevokesUnlistedObjects(t *testing.T) {
	a := aclObject{kind: "table", schema: "public", name: "a"}
	c := aclObject{kind: "table", schema: "public", name: "c"}
	def := aclObject{kind: "default:sequence", schema: "public", name: "own"}
	desired := aclState{a: privs("SELECT")}
	actual := aclState{a: privs("SELECT"), c: privs("SELECT"), def: privs("USAGE")}

	changes := diffACL(desired, actual, exclusiveScope(desired, actual))
	want := []aclChange{
		{obj: def, revoke: privilegeSet{"USAGE": false}},
		{obj: c, revoke: privilegeSet{"SELECT": false}},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %#v, want %#v", changes, want)
	}
}

func TestRenderACLStatements_GroupsAndOrders(t *testing.T) {
	role := "app_ro"
	actual := aclState{
		{kind: "schema", schema: "public"}:                     privs(),
		{kind: "table", schema: "public", name: "a"}:           privs("DELETE"),
		{kind: "table", schema: "public", name: "b"}:           privs(),
		{kind: "table", schema: "public", name: "c"}:           privs(),
		{kind: "sequence", schema: "public", name: "s1"}:       privs("USAGE*"),
		{kind: "default:table", schema: "public", name: "own"}: privs("INSERT"),
	}
	desired := aclState{
		{kind: "schema", schema: "public"}:                     privs("USAGE"),
		{kind: "table", schema: "public", name: "a"}:           privs("SELECT"),
		{kind: "table", schema: "public", name: "b"}:           privs("SELECT"),
		{kind: "table", schema: "public", name: "c"}:           privs("SELECT", "INSERT"),
		{kind: "sequence", schema: "public", name: "s1"}:       privs("USAGE"),
		{kind: "default:table", schema: "public", name: "own"}: privs("SELECT"),
	}
	changes := diffACL(desired, actual, exclusiveScope(desired, actual))
	got := renderACLStatements(role, changes, actual)
	want := []string{
		// revokes first, sorted by kind/schema/object
		`ALTER DEFAULT PRIVILEGES FOR ROLE "own" IN SCHEMA "public" REVOKE INSERT ON TABLES FROM "app_ro"`,
		`REVOKE GRANT OPTION FOR USAGE ON SEQUENCE "public"."s1" FROM "app_ro"`,
		`REVOKE DELETE ON TABLE "public"."a" FROM "app_ro"`,
		// then grants; every table of the schema needs SELECT -> ALL TABLES IN SCHEMA
		`ALTER DEFAULT PRIVILEGES FOR ROLE "own" IN SCHEMA "public" GRANT SELECT ON TABLES TO "app_ro"`,
		`GRANT USAGE ON SCHEMA "public" TO "app_ro"`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA "public" TO "app_ro"`,
		`GRANT INSERT ON TABLE "public"."c" TO "app_ro"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statements:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRenderACLStatements_GrantOptionAndNamedObjects(t *testing.T) {
	actual := aclState{
		{kind: "table", schema: "s", name: "a"}: privs(),
		{kind: "table", schema: "s", name: "b"}: privs(),
		{kind: "table", schema: "s", name: "c"}: privs(),
	}
	desired := aclState{
		{kind: "table", schema: "s", name: "a"}: privs("SELECT*", "INSERT*"),
		{kind: "table", schema: "s", name: "b"}: privs("SELECT*", "INSERT*"),
	}
	changes := diffACL(desired, actual, scopeOf(desired))
	got := renderACLStatements("r", changes, actual)
	want := []string{`GRANT INSERT,SELECT ON TABLE "s"."a","s"."b" TO "r" WITH GRANT OPTION`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDriftEntries_CapAndProblems(t *testing.T) {
	var changes []aclChange
	for i := 0; i < 60; i++ {
		changes = append(changes, aclChange{
			obj:   aclObject{kind: "table", schema: "s", name: strings.Repeat("t", i+1)},
			grant: privilegeSet{"SELECT": false},
		})
	}
	got := driftEntries(changes, []string{"schema gone: does not exist"})
	if len(got) != maxDriftEntries+1 {
		t.Fatalf("got %d entries, want %d", len(got), maxDriftEntries+1)
	}
	if got[0] != "schema gone: does not exist" {
		t.Fatalf("problems must come first, got %q", got[0])
	}
	if got[len(got)-1] != "... and 11 more" {
		t.Fatalf("last entry = %q", got[len(got)-1])
	}
}
