package postgresql

import (
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestRoleMembershipStateAction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state roleMembershipState
		want  membershipAction
	}{
		{"already a member", roleMembershipState{member: true}, membershipSkip},
		{"member wins over a missing admin option", roleMembershipState{member: true, canAdmin: false}, membershipSkip},
		{"grantable", roleMembershipState{canAdmin: true}, membershipGrant},
		{"no admin option (PG 16+, role created by someone else)", roleMembershipState{}, membershipTryGrant},
		{"direct reverse membership", roleMembershipState{canAdmin: true, reverseDirect: true}, membershipTryGrant},
		{"indirect reverse membership", roleMembershipState{canAdmin: true, reverseIndirect: true}, membershipBlocked},
		{"direct and indirect reverse membership", roleMembershipState{canAdmin: true, reverseDirect: true, reverseIndirect: true}, membershipBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.action(); got != tc.want {
				t.Errorf("action() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRoleMembershipStatesQuery(t *testing.T) {
	pre16 := roleMembershipStatesQuery(false)
	pg16 := roleMembershipStatesQuery(true)

	// 'SET' is only a valid pg_has_role privilege since PostgreSQL 16, and
	// CREATEROLE only allows granting any role before 16.
	if strings.Contains(pre16, "'SET'") || strings.Contains(pre16, "set_option") {
		t.Errorf("pre-16 query uses PostgreSQL 16 features:\n%s", pre16)
	}
	if !strings.Contains(pre16, "rolcreaterole") {
		t.Errorf("pre-16 query ignores CREATEROLE:\n%s", pre16)
	}
	if !strings.Contains(pg16, "'SET'") || !strings.Contains(pg16, "set_option") {
		t.Errorf("16+ query does not check SET:\n%s", pg16)
	}
	if strings.Contains(pg16, "rolcreaterole") {
		t.Errorf("16+ query treats CREATEROLE as ADMIN OPTION:\n%s", pg16)
	}
	for _, q := range []string{pre16, pg16} {
		if !strings.Contains(q, "MEMBER WITH ADMIN OPTION") || !strings.Contains(q, "reverse_indirect") {
			t.Errorf("query misses a column:\n%s", q)
		}
	}
}

func TestBlockedRolesError(t *testing.T) {
	circular := blockedRole{role: "a", reason: `"a" is a member of "rds" through a -> b -> rds`, circular: true}
	denied := blockedRole{role: "o", reason: `granting "o" to it failed: permission denied`}

	err := blockedRolesError("rds", []blockedRole{circular}, true).Error()
	for _, want := range []string{`cannot act as "a"`, "a -> b -> rds", "remove the circular membership", "WITH GRANT OPTION"} {
		if !strings.Contains(err, want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err, `grant "a" to`) {
		t.Errorf("error %q suggests granting a role that is a member of the current user", err)
	}

	err = blockedRolesError("rds", []blockedRole{denied}, false).Error()
	if !strings.Contains(err, `grant "o" to "rds"`) {
		t.Errorf("error %q does not suggest granting the owner", err)
	}
	if strings.Contains(err, "WITH GRANT OPTION") {
		t.Errorf("error %q suggests a grant option to a caller that cannot use it", err)
	}
}

func TestGrantableDirectlyQuery(t *testing.T) {
	db := &DBConnection{version: semver.MustParse("17.0.0")}

	for _, tc := range []struct {
		name     string
		values   map[string]any
		ok       bool
		contains []string
		nargs    int
		privs    string
	}{
		{
			name:     "listed tables",
			values:   map[string]any{"schema": "s1", "object_type": "table", "objects": []any{"t1"}, "privileges": []any{"SELECT"}},
			ok:       true,
			contains: []string{"has_table_privilege(o.oid, p)", "has_schema_privilege($1, 'USAGE')", "o.relname = ANY($4)"},
			nargs:    4,
			privs:    `{"SELECT WITH GRANT OPTION"}`,
		},
		{
			name:     "all tables, ALL expanded with MAINTAIN on 17",
			values:   map[string]any{"schema": "s1", "object_type": "table", "privileges": []any{"ALL"}},
			ok:       true,
			contains: []string{"has_table_privilege(o.oid, p)"},
			nargs:    3,
			privs:    `{"SELECT WITH GRANT OPTION","INSERT WITH GRANT OPTION","UPDATE WITH GRANT OPTION","DELETE WITH GRANT OPTION","TRUNCATE WITH GRANT OPTION","REFERENCES WITH GRANT OPTION","TRIGGER WITH GRANT OPTION","MAINTAIN WITH GRANT OPTION"}`,
		},
		{
			name:     "sequence",
			values:   map[string]any{"schema": "s1", "object_type": "sequence", "objects": []any{"q"}, "privileges": []any{"usage"}},
			ok:       true,
			contains: []string{"has_sequence_privilege(o.oid, p)"},
			nargs:    4,
			privs:    `{"USAGE WITH GRANT OPTION"}`,
		},
		{
			name:     "function with signature",
			values:   map[string]any{"schema": "s1", "object_type": "function", "objects": []any{"f(int)"}, "privileges": []any{"EXECUTE"}},
			ok:       true,
			contains: []string{"has_function_privilege(o.oid, p)", "FROM pg_proc o", "o.proname = ANY($4)"},
			nargs:    4,
		},
		{
			name:     "column",
			values:   map[string]any{"schema": "s1", "object_type": "column", "objects": []any{"t1"}, "columns": []any{"c1"}, "privileges": []any{"SELECT"}},
			ok:       true,
			contains: []string{"has_column_privilege(o.oid, col, p)", "unnest($5::text[]) col"},
			nargs:    5,
		},
		{
			name:     "schema",
			values:   map[string]any{"schema": "s1", "object_type": "schema", "privileges": []any{"USAGE"}},
			ok:       true,
			contains: []string{"has_schema_privilege($1, p)"},
			nargs:    2,
		},
		{
			name:     "database",
			values:   map[string]any{"database": "db1", "object_type": "database", "privileges": []any{"CONNECT"}},
			ok:       true,
			contains: []string{"has_database_privilege($1, p)"},
			nargs:    2,
		},
		{
			name:   "foreign server is not covered",
			values: map[string]any{"object_type": "foreign_server", "objects": []any{"srv"}, "privileges": []any{"USAGE"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, withGrantDefaults(tc.values))
			query, args, ok := grantableDirectlyQuery(db, d)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			for _, want := range tc.contains {
				if !strings.Contains(query, want) {
					t.Errorf("query does not contain %q:\n%s", want, query)
				}
			}
			if len(args) != tc.nargs {
				t.Errorf("got %d args, want %d: %v", len(args), tc.nargs, args)
			}
			if tc.privs != "" {
				got, err := args[1].(driver.Valuer).Value()
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.privs {
					t.Errorf("privileges = %v, want %v", got, tc.privs)
				}
			}
		})
	}
}

// withGrantDefaults fills the required attributes of postgresql_grant.
func withGrantDefaults(values map[string]any) map[string]any {
	out := map[string]any{"database": "db1", "role": "r1"}
	for k, v := range values {
		out[k] = v
	}
	return out
}

func runWithRolesGrantedOrDirect(t *testing.T, db *DBConnection, directGrant func() (bool, error)) (called bool, err error) {
	t.Helper()
	txn, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer deferredRollback(txn)
	err = withRolesGrantedOrDirect(db, txn, []string{"owner"}, directGrant, func() error { called = true; return nil })
	return called, err
}

// On PostgreSQL 16+ a role created by another role cannot be granted to the
// provider without ADMIN OPTION (#132): the failed GRANT is rolled back to a
// savepoint and the caller may grant directly.
func TestWithRolesGrantedFallsBackToDirectGrant(t *testing.T) {
	t.Run("direct grant allowed", func(t *testing.T) {
		db, drv := newSessionRoleConnection(t, "17.0.0", false)
		drv.failGrant = true

		asked := false
		called, err := runWithRolesGrantedOrDirect(t, db, func() (bool, error) { asked = true; return true, nil })
		if err != nil {
			t.Fatalf("withRolesGrantedOrDirect: %v", err)
		}
		if !asked || !called {
			t.Fatalf("directGrant asked = %v, fn called = %v; want both", asked, called)
		}
		if got := drv.count("ROLLBACK TO SAVEPOINT"); got != 1 {
			t.Errorf("rolled back to the savepoint %d times, want 1; statements: %q", got, drv.all())
		}
		if got := drv.count("REVOKE"); got != 0 {
			t.Errorf("revoked a membership that was never granted; statements: %q", drv.all())
		}
	})

	t.Run("no grant option", func(t *testing.T) {
		db, drv := newSessionRoleConnection(t, "17.0.0", false)
		drv.failGrant = true

		called, err := runWithRolesGrantedOrDirect(t, db, func() (bool, error) { return false, nil })
		if err == nil || called {
			t.Fatalf("err = %v, fn called = %v; want an error and fn not called", err, called)
		}
		for _, want := range []string{`cannot act as "owner"`, "permission denied to grant role", "WITH GRANT OPTION"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})

	t.Run("no direct grant for this caller", func(t *testing.T) {
		db, drv := newSessionRoleConnection(t, "17.0.0", false)
		drv.failGrant = true

		called, err := runWithRolesGrantedOrDirect(t, db, nil)
		if err == nil || called {
			t.Fatalf("err = %v, fn called = %v; want an error and fn not called", err, called)
		}
	})
}
