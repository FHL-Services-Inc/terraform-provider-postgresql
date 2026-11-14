package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func grantResourceData(t *testing.T, values map[string]any) *schema.ResourceData {
	t.Helper()
	d := resourcePostgreSQLGrant().TestResourceData()
	for k, v := range values {
		if err := d.Set(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	return d
}

func TestGrantLockKeys(t *testing.T) {
	schemaWide := func(class, schemaName string) advisoryLockKey {
		return newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, false, class, schemaName)
	}
	schemaShared := func(class, schemaName string) advisoryLockKey {
		return newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, true, class, schemaName)
	}
	object := func(class, schemaName, name string) advisoryLockKey {
		return newAdvisoryLockKey(advisoryLockNamespaceObject, false, class, schemaName, name)
	}

	for _, tc := range []struct {
		name   string
		values map[string]any
		want   []advisoryLockKey
	}{
		{
			name:   "database: locked by pgLockDatabase, no object key",
			values: map[string]any{"object_type": "database"},
			want:   nil,
		},
		{
			name:   "schema",
			values: map[string]any{"object_type": "schema", "schema": "s1"},
			want:   []advisoryLockKey{object("schema", "", "s1")},
		},
		{
			name:   "all tables in schema",
			values: map[string]any{"object_type": "table", "schema": "s1"},
			want:   []advisoryLockKey{schemaWide("table", "s1")},
		},
		{
			name:   "all sequences in schema",
			values: map[string]any{"object_type": "sequence", "schema": "s1"},
			want:   []advisoryLockKey{schemaWide("sequence", "s1")},
		},
		{
			name:   "listed tables: shared schema key, then one key per table",
			values: map[string]any{"object_type": "table", "schema": "s1", "objects": []any{"t2", "t1"}},
			want: append(
				[]advisoryLockKey{schemaShared("table", "s1")},
				sortedKeys(object("table", "s1", "t1"), object("table", "s1", "t2"))...,
			),
		},
		{
			name: "column grant locks its table like a table grant",
			values: map[string]any{
				"object_type": "column", "schema": "s1", "objects": []any{"t1"}, "columns": []any{"c1"},
			},
			want: []advisoryLockKey{schemaShared("table", "s1"), object("table", "s1", "t1")},
		},
		{
			name:   "functions, procedures and routines share one class",
			values: map[string]any{"object_type": "procedure", "schema": "s1"},
			want:   []advisoryLockKey{schemaWide("routine", "s1")},
		},
		{
			name: "overloads of a function share a key",
			values: map[string]any{
				"object_type": "function", "schema": "s1", "objects": []any{"f(integer)", "f(text)", "g"},
			},
			want: append(
				[]advisoryLockKey{schemaShared("routine", "s1")},
				sortedKeys(object("routine", "s1", "f"), object("routine", "s1", "g"))...,
			),
		},
		{
			name:   "foreign data wrapper",
			values: map[string]any{"object_type": "foreign_data_wrapper", "objects": []any{"w1"}},
			want:   []advisoryLockKey{object("foreign_data_wrapper", "", "w1")},
		},
		{
			name:   "foreign server",
			values: map[string]any{"object_type": "foreign_server", "objects": []any{"srv1"}},
			want:   []advisoryLockKey{object("foreign_server", "", "srv1")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]any{"database": "db1", "role": "r1", "privileges": []any{"SELECT"}}
			for k, v := range tc.values {
				values[k] = v
			}
			got := grantLockKeys(grantResourceData(t, values).Get)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("grantLockKeys =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

func sortedKeys(keys ...advisoryLockKey) []advisoryLockKey {
	sortAdvisoryLockKeys(keys)
	return keys
}

// The keys are shared by every provider process that may run against the same
// server at once (two applies, two provider versions), so the derivation must
// not change silently.
func TestAdvisoryLockKeyIsStable(t *testing.T) {
	for _, tc := range []struct {
		key  advisoryLockKey
		want string
	}{
		{newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, false, "table", "public"), "pg_advisory_xact_lock(1952870515, 1474111144)"},
		{newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, true, "table", "public"), "pg_advisory_xact_lock_shared(1952870515, 1474111144)"},
		{newAdvisoryLockKey(advisoryLockNamespaceObject, false, "table", "public", "t1"), "pg_advisory_xact_lock(1952870511, -1296691377)"},
		{newAdvisoryLockKey(advisoryLockNamespaceDefaultACL, false, "owner", "public", "r"), "pg_advisory_xact_lock(1952870500, -1024446641)"},
		{newAdvisoryLockKey(advisoryLockNamespaceMembership, false, "g", "m"), "pg_advisory_xact_lock(1952870509, 1579545037)"},
	} {
		if got := tc.key.call(); got != tc.want {
			t.Errorf("%v.call() = %s, want %s", tc.key, got, tc.want)
		}
	}
}

func TestAdvisoryLockKeyParts(t *testing.T) {
	// Parts are separated, so shifting characters between them changes the key.
	a := newAdvisoryLockKey(advisoryLockNamespaceObject, false, "table", "ab", "c")
	b := newAdvisoryLockKey(advisoryLockNamespaceObject, false, "table", "a", "bc")
	if a.key == b.key {
		t.Fatalf("keys for (ab, c) and (a, bc) collide: %v", a)
	}
	// Different object classes in the same schema don't share a key.
	tbl := newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, false, "table", "s")
	seq := newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, false, "sequence", "s")
	if tbl.key == seq.key {
		t.Fatalf("table and sequence schema keys collide: %v", tbl)
	}
}

func TestAdvisoryLockStatementsOrder(t *testing.T) {
	keys := []advisoryLockKey{
		{namespace: advisoryLockNamespaceObject, key: 5},
		{namespace: advisoryLockNamespaceSchemaObjects, key: 9, shared: true},
		{namespace: advisoryLockNamespaceObject, key: -3},
		{namespace: advisoryLockNamespaceObject, key: 5},
	}
	got := advisoryLockStatements(keys)
	want := []string{
		"SELECT pg_advisory_xact_lock_shared(1952870515, 9)",
		"SELECT pg_advisory_xact_lock(1952870511, k) FROM unnest('{-3,5}'::int4[]) WITH ORDINALITY AS l(k, n) ORDER BY n",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("advisoryLockStatements =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDefaultPrivilegesLockKey(t *testing.T) {
	d := resourcePostgreSQLDefaultPrivileges().TestResourceData()
	for k, v := range map[string]any{
		"database": "db1", "owner": "o1", "role": "r1", "schema": "s1", "object_type": "routine", "privileges": []any{"EXECUTE"},
	} {
		if err := d.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	got := defaultPrivilegesLockKey(d)
	want := newAdvisoryLockKey(advisoryLockNamespaceDefaultACL, false, "o1", "s1", "f")
	if got != want {
		t.Fatalf("defaultPrivilegesLockKey = %v, want %v (function and routine share pg_default_acl rows)", got, want)
	}
}

func TestIsConcurrentCatalogUpdateError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&pq.Error{Code: "XX000", Message: "tuple concurrently updated"}, true},
		{fmt.Errorf("could not execute revoke/grant query: %w", &pq.Error{Code: "XX000", Message: "tuple concurrently deleted"}), true},
		{&pq.Error{Code: "XX000", Message: "cache lookup failed for relation 1"}, false},
		{&pq.Error{Code: "40P01", Message: "deadlock detected"}, false},
		{errors.New("tuple concurrently updated"), false},
	} {
		if got := isConcurrentCatalogUpdateError(tc.err); got != tc.want {
			t.Errorf("isConcurrentCatalogUpdateError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRetryOnConcurrentCatalogUpdate(t *testing.T) {
	tupleErr := &pq.Error{Code: "XX000", Message: "tuple concurrently updated"}
	for _, tc := range []struct {
		name      string
		errs      []error
		wantCalls int
		wantErr   bool
	}{
		{"success", []error{nil}, 1, false},
		{"retried once", []error{tupleErr, nil}, 2, false},
		{"only one retry", []error{tupleErr, tupleErr, nil}, 2, true},
		{"other errors are not retried", []error{errors.New("boom"), nil}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryOnConcurrentCatalogUpdate(func(*DBConnection, *schema.ResourceData) error {
				err := tc.errs[calls]
				calls++
				return err
			})(nil, nil)
			if calls != tc.wantCalls || (err != nil) != tc.wantErr {
				t.Fatalf("calls = %d, err = %v; want %d calls, error %v", calls, err, tc.wantCalls, tc.wantErr)
			}
		})
	}
}

// grantLockPreamble runs a grant write on the statement-recording driver and
// returns its first statement, the lock preamble. The fake driver returns no
// rows, so writes that look up owners stop right after the preamble.
func grantLockPreamble(t *testing.T, write func(*DBConnection, *schema.ResourceData) error, d *schema.ResourceData) string {
	t.Helper()
	db := newWriteStmtConnection(t, 0, nil)
	_ = write(db, d)
	stmts := writeStmtCounter.inTxn()
	if len(stmts) == 0 {
		t.Fatal("no statement sent")
	}
	assertNoBindParameters(t, stmts[:1])
	return stmts[0].query
}

func TestGrantWritesLockObjectsNotMembers(t *testing.T) {
	values := map[string]any{
		"database": writeStmtDatabase, "role": "r1", "schema": "s1", "object_type": "table",
		"objects": []any{"t1"}, "privileges": []any{"SELECT"},
	}
	for name, write := range map[string]func(*DBConnection, *schema.ResourceData) error{
		"create": resourcePostgreSQLGrantCreate,
		"delete": resourcePostgreSQLGrantDelete,
	} {
		t.Run(name, func(t *testing.T) {
			preamble := grantLockPreamble(t, write, grantResourceData(t, values))
			for _, part := range []string{
				"SET LOCAL statement_timeout = 0",
				"SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = 'r1'",
				newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, true, "table", "s1").call(),
				newAdvisoryLockKey(advisoryLockNamespaceObject, false, "table", "s1", "t1").call(),
			} {
				if !strings.Contains(preamble, part) {
					t.Errorf("lock preamble is missing %q:\n%s", part, preamble)
				}
			}
			if strings.Contains(preamble, "pg_auth_members") {
				t.Errorf("grant write locks the members of the grantee:\n%s", preamble)
			}
		})
	}
}

func TestDefaultPrivilegesWritesLockRowNotMembers(t *testing.T) {
	d := resourcePostgreSQLDefaultPrivileges().TestResourceData()
	for k, v := range map[string]any{
		"database": writeStmtDatabase, "owner": "o1", "role": "r1", "schema": "s1",
		"object_type": "table", "privileges": []any{"SELECT"},
	} {
		if err := d.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, write := range map[string]func(*DBConnection, *schema.ResourceData) error{
		"create": resourcePostgreSQLDefaultPrivilegesCreate,
		"delete": resourcePostgreSQLDefaultPrivilegesDelete,
	} {
		t.Run(name, func(t *testing.T) {
			preamble := grantLockPreamble(t, write, d)
			for _, part := range []string{
				"SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = 'o1'",
				newAdvisoryLockKey(advisoryLockNamespaceDefaultACL, false, "o1", "s1", "r").call(),
			} {
				if !strings.Contains(preamble, part) {
					t.Errorf("lock preamble is missing %q:\n%s", part, preamble)
				}
			}
			if strings.Contains(preamble, "pg_auth_members") {
				t.Errorf("default privileges write locks the members of the owner:\n%s", preamble)
			}
		})
	}
}

// Grants that change the ACL of the same tables for different grantees
// (several roles and PUBLIC, schema-wide and listed objects) applied in
// parallel, without depends_on. Every step updates all of them at once and the
// last step destroys them, so each step is a burst of concurrent writes on the
// same pg_class rows. Without object locks this fails with "tuple concurrently
// updated" (#53).
func TestAccPostgresqlGrant_parallelSameObjects(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)

	var tables []string
	for i := range 6 {
		tables = append(tables, fmt.Sprintf("test_schema.parallel_%d", i))
	}
	createTestTables(t, dbSuffix, tables, "")

	dbName, roleName := getTestDBNames(dbSuffix)
	roles := []string{roleName}
	testConfig := getTestConfig(t)
	for i := range 4 {
		role := fmt.Sprintf("tf_tests_par_%d_%s", i, dbSuffix)
		defer createTestRole(t, role)()
		dbExecute(t, testConfig.connStr(dbName), fmt.Sprintf("GRANT USAGE ON SCHEMA test_schema TO %s", role))
		roles = append(roles, role)
	}
	roles = append(roles, "public")
	// Deferred last so it runs first: the database goes before the roles.
	defer teardown()

	config := func(privileges string) string {
		var b strings.Builder
		for i, role := range roles {
			fmt.Fprintf(&b, `
resource "postgresql_grant" "all_%[1]d" {
  database    = %[2]q
  role        = %[3]q
  schema      = "test_schema"
  object_type = "table"
  privileges  = %[4]s
}

resource "postgresql_grant" "listed_%[1]d" {
  database    = %[2]q
  role        = %[3]q
  schema      = "test_schema"
  object_type = "table"
  objects     = ["parallel_0", "parallel_1"]
  privileges  = %[4]s
}
`, i, dbName, role, privileges)
		}
		return b.String()
	}

	checkAll := func(want []string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			for _, role := range roles[:len(roles)-1] {
				if err := testCheckTablesPrivileges(t, dbName, role, tables[2:], want); err != nil {
					return err
				}
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{Config: config(`["SELECT"]`), Check: checkAll([]string{"SELECT"})},
			{Config: config(`["SELECT", "INSERT"]`), Check: checkAll([]string{"SELECT", "INSERT"})},
			{Config: config(`["SELECT"]`), Check: checkAll([]string{"SELECT"})},
		},
	})
}

// holdAdvisoryLocks takes the given keys as session-level locks from a
// dedicated connection to the provider's database and returns a release func.
func holdAdvisoryLocks(t *testing.T, keys ...advisoryLockKey) func() {
	t.Helper()
	config := getTestConfig(t)
	holder, err := sql.Open("postgres", config.connStr("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		fn := "pg_advisory_lock"
		if k.shared {
			fn = "pg_advisory_lock_shared"
		}
		if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("SELECT %s(%d, %d)", fn, k.namespace, k.key)); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		_ = conn.Close()
		_ = holder.Close()
	}
}

func TestAccAdvisoryLockGrantObjects(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	defer teardown()
	_, roleName := getTestDBNames(dbSuffix)

	keysFor := func(objects ...any) []advisoryLockKey {
		values := map[string]any{
			"database": "postgres", "role": roleName, "schema": "s1", "object_type": "table", "privileges": []any{"SELECT"},
		}
		if len(objects) > 0 {
			values["objects"] = objects
		}
		return grantLockKeys(grantResourceData(t, values).Get)
	}

	tryLock := func(keys []advisoryLockKey) error {
		db := openSingleConnLockDB(t, 300)
		txn, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer deferredRollback(txn)
		return pgLockRoleAndObjects(db, txn, roleName, nil, keys)
	}

	for _, tc := range []struct {
		name    string
		held    []advisoryLockKey
		try     []advisoryLockKey
		blocked bool
	}{
		{"listed t1 held, listed t2 runs", keysFor("t1"), keysFor("t2"), false},
		{"listed t1 held, listed t1 waits", keysFor("t1"), keysFor("t1", "t2"), true},
		{"listed t1 held, all tables waits", keysFor("t1"), keysFor(), true},
		{"all tables held, listed t2 waits", keysFor(), keysFor("t2"), true},
		{"all tables held, all tables waits", keysFor(), keysFor(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := holdAdvisoryLocks(t, tc.held...)
			defer release()

			err := tryLock(tc.try)
			switch {
			case tc.blocked && err == nil:
				t.Fatal("lock acquired while a conflicting grant holds its locks")
			case tc.blocked && !strings.Contains(err.Error(), fmt.Sprintf("timed out after 300ms waiting for advisory lock on role %q or the objects it changes", roleName)):
				t.Fatalf("unexpected error: %v", err)
			case !tc.blocked && err != nil:
				t.Fatalf("lock on disjoint objects failed: %v", err)
			}
		})
	}
}
