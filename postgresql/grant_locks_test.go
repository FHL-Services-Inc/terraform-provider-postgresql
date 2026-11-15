package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
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

func defaultPrivilegesResourceData(t *testing.T, values map[string]any) *schema.ResourceData {
	t.Helper()
	d := resourcePostgreSQLDefaultPrivileges().TestResourceData()
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
			name:   "database: locked by OID with the grantee, no object key",
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

func TestAdvisoryLockStatementOrder(t *testing.T) {
	keys := []advisoryLockKey{
		{namespace: advisoryLockNamespaceObject, key: 5},
		{namespace: advisoryLockNamespaceSchemaObjects, key: 9, shared: true},
		{namespace: advisoryLockNamespaceObject, key: -3},
		{namespace: advisoryLockNamespaceObject, key: 5},
	}
	got := advisoryLockStatement(keys)
	want := "SELECT pg_advisory_xact_lock_shared(1952870515, 9);\n" +
		"SELECT pg_advisory_xact_lock(1952870511, k) FROM unnest('{-3,5}'::int4[]) WITH ORDINALITY AS l(k, n) ORDER BY n"
	if got != want {
		t.Fatalf("advisoryLockStatement =\n%s\nwant\n%s", got, want)
	}
	if got := advisoryLockStatement(nil); got != "" {
		t.Fatalf("advisoryLockStatement(nil) = %q, want empty", got)
	}
}

func TestDefaultPrivilegesLockKey(t *testing.T) {
	d := defaultPrivilegesResourceData(t, map[string]any{
		"database": "db1", "owner": "o1", "role": "r1", "schema": "s1", "object_type": "routine", "privileges": []any{"EXECUTE"},
	})
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
		{fmt.Errorf("could not execute revoke query: %w", &pq.Error{Code: "XX000", Message: "tuple concurrently deleted"}), true},
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

// lockRecordingDriver is a fake driver recording the statements of a write,
// with their arguments. Every query returns no rows.
type lockRecordingDriver struct {
	mu    sync.Mutex
	stmts []recordedStmt
}

type recordedStmt struct {
	query string
	args  []any
}

func (d *lockRecordingDriver) record(query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := recordedStmt{query: query}
	for _, a := range args {
		s.args = append(s.args, a.Value)
	}
	d.stmts = append(d.stmts, s)
}

func (d *lockRecordingDriver) Open(string) (driver.Conn, error) { return lockRecordingConn{d}, nil }

type lockRecordingConn struct{ d *lockRecordingDriver }

func (lockRecordingConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (lockRecordingConn) Close() error                        { return nil }
func (lockRecordingConn) Begin() (driver.Tx, error)           { return stmtCountingTx{}, nil }

func (c lockRecordingConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.d.record(query, args)
	return driver.RowsAffected(0), nil
}

func (c lockRecordingConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.record(query, args)
	return lockRecordingRows{}, nil
}

type lockRecordingRows struct{}

func (lockRecordingRows) Columns() []string         { return []string{"c"} }
func (lockRecordingRows) Close() error              { return nil }
func (lockRecordingRows) Next([]driver.Value) error { return io.EOF }

const lockRecordingDriverName = "pg-lock-recorder"

var (
	lockRecorder         = &lockRecordingDriver{}
	registerLockRecorder sync.Once
)

// recordWriteStatements runs write on a fake connection registered for
// database "testdb" and returns the statements it sent.
func recordWriteStatements(t *testing.T, write func(*DBConnection, *schema.ResourceData) error, d *schema.ResourceData) []recordedStmt {
	t.Helper()
	rec := lockRecorder
	registerLockRecorder.Do(func() { sql.Register(lockRecordingDriverName, rec) })
	rec.mu.Lock()
	rec.stmts = nil
	rec.mu.Unlock()

	sqlDB, err := sql.Open(lockRecordingDriverName, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	sqlDB.SetMaxIdleConns(0)

	client := &Client{config: Config{Scheme: "awspostgres"}, databaseName: "testdb"}
	conn := &DBConnection{DB: sqlDB, client: client, version: semver.MustParse("16.0.0")}
	dsn := client.config.connStr(client.databaseName)
	dbRegistryLock.Lock()
	dbRegistry[dsn] = conn
	dbRegistryLock.Unlock()
	defer func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, dsn)
		dbRegistryLock.Unlock()
		_ = sqlDB.Close()
	}()

	_ = write(conn, d)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recordedStmt(nil), rec.stmts...)
}

// assertLockPreamble checks that the first statements of a write are, in this
// order: disabling statement_timeout, the grantee lock (role only, no member,
// with the database when given), and the object locks.
func assertLockPreamble(t *testing.T, stmts []recordedStmt, role, database string, objectLocks string) {
	t.Helper()
	want := 3
	if objectLocks == "" {
		want = 2
	}
	if len(stmts) < want {
		t.Fatalf("write sent %d statements, want at least %d: %+v", len(stmts), want, stmts)
	}
	if stmts[0].query != "SET statement_timeout = 0" {
		t.Errorf("statement 1 = %q, want SET statement_timeout = 0", stmts[0].query)
	}
	if stmts[1].query != granteeLockQuery {
		t.Errorf("statement 2 = %q, want the grantee lock", stmts[1].query)
	}
	if fmt.Sprint(stmts[1].args) != fmt.Sprint([]any{role, database}) {
		t.Errorf("grantee lock args = %v, want [%s %s]", stmts[1].args, role, database)
	}
	if objectLocks != "" && stmts[2].query != objectLocks {
		t.Errorf("statement 3 = %q, want the object locks %q", stmts[2].query, objectLocks)
	}
	for _, s := range stmts[:want] {
		if strings.Contains(s.query, "pg_auth_members") {
			t.Errorf("write locks the members of the role: %q", s.query)
		}
	}
}

func TestGrantWritesLockObjectsNotMembers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		values   map[string]any
		database string
		objects  string
	}{
		{
			name:    "listed tables",
			values:  map[string]any{"schema": "s1", "object_type": "table", "objects": []any{"t1"}},
			objects: advisoryLockStatement(grantLockKeys(grantResourceData(t, map[string]any{"schema": "s1", "object_type": "table", "objects": []any{"t1"}}).Get)),
		},
		{
			name:    "all tables in schema",
			values:  map[string]any{"schema": "s1", "object_type": "table"},
			objects: "SELECT " + newAdvisoryLockKey(advisoryLockNamespaceSchemaObjects, false, "table", "s1").call(),
		},
		{
			name:     "database",
			values:   map[string]any{"object_type": "database"},
			database: "testdb",
		},
	} {
		values := map[string]any{"database": "testdb", "role": "r1", "privileges": []any{"CONNECT"}}
		if tc.values["object_type"] != "database" {
			values["privileges"] = []any{"SELECT"}
		}
		for k, v := range tc.values {
			values[k] = v
		}
		for name, write := range map[string]func(*DBConnection, *schema.ResourceData) error{
			"create": resourcePostgreSQLGrantCreate,
			"delete": resourcePostgreSQLGrantDelete,
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				stmts := recordWriteStatements(t, write, grantResourceData(t, values))
				assertLockPreamble(t, stmts, "r1", tc.database, tc.objects)
			})
		}
	}
}

func TestDefaultPrivilegesWritesLockRowNotMembers(t *testing.T) {
	values := map[string]any{
		"database": "testdb", "owner": "o1", "role": "r1", "schema": "s1",
		"object_type": "table", "privileges": []any{"SELECT"},
	}
	for name, write := range map[string]func(*DBConnection, *schema.ResourceData) error{
		"create": resourcePostgreSQLDefaultPrivilegesCreate,
		"delete": resourcePostgreSQLDefaultPrivilegesDelete,
	} {
		t.Run(name, func(t *testing.T) {
			stmts := recordWriteStatements(t, write, defaultPrivilegesResourceData(t, values))
			assertLockPreamble(t, stmts, "o1", "",
				"SELECT "+newAdvisoryLockKey(advisoryLockNamespaceDefaultACL, false, "o1", "s1", "r").call())
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
		role := testUniqueName(fmt.Sprintf("tf_tests_par_%d", i))
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

// TestAccAdvisoryLockGrantObjects checks which grant writes wait for which:
// another session holds the locks of one grant (or the lock of a member of the
// grantee), and the locks of a second grant are taken with lock_timeout set.
func TestAccAdvisoryLockGrantObjects(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, true)
	_, roleName := getTestDBNames(dbSuffix)
	config := getTestConfig(t)
	member := testUniqueName("tf_tests_lockmember")
	defer createTestRole(t, member)()
	defer teardown()
	dbExecute(t, config.connStr("postgres"), fmt.Sprintf("GRANT %s TO %s", roleName, member))

	db, err := sql.Open("postgres", config.connStr("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB(t, db)

	keysFor := func(objects ...any) []advisoryLockKey {
		values := map[string]any{
			"database": "postgres", "role": roleName, "schema": "s1", "object_type": "table", "privileges": []any{"SELECT"},
		}
		if len(objects) > 0 {
			values["objects"] = objects
		}
		return grantLockKeys(grantResourceData(t, values).Get)
	}

	// hold takes keys (and the OID lock of role, if set) as session-level
	// locks on a dedicated connection.
	hold := func(t *testing.T, role string, keys []advisoryLockKey) func() {
		t.Helper()
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if _, err := conn.ExecContext(context.Background(),
				"SELECT pg_advisory_lock(oid::bigint) FROM pg_roles WHERE rolname = $1", role); err != nil {
				t.Fatal(err)
			}
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
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock_all()")
			_ = conn.Close()
		}
	}

	tryLock := func(t *testing.T, keys []advisoryLockKey) error {
		t.Helper()
		txn, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer deferredRollback(txn)
		if _, err := txn.Exec("SET LOCAL lock_timeout = 300"); err != nil {
			t.Fatal(err)
		}
		return pgLockGranteeAndObjects(txn, roleName, "", keys)
	}

	for _, tc := range []struct {
		name     string
		heldRole string
		held     []advisoryLockKey
		try      []advisoryLockKey
		blocked  bool
	}{
		{"listed t1 held, listed t2 runs", "", keysFor("t1"), keysFor("t2"), false},
		{"listed t1 held, listed t1 waits", "", keysFor("t1"), keysFor("t1", "t2"), true},
		{"listed t1 held, all tables waits", "", keysFor("t1"), keysFor(), true},
		{"all tables held, listed t2 waits", "", keysFor(), keysFor("t2"), true},
		{"all tables held, all tables waits", "", keysFor(), keysFor(), true},
		{"grantee held, grant waits", roleName, nil, keysFor("t1"), true},
		{"member of the grantee held, grant runs", member, nil, keysFor("t1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := hold(t, tc.heldRole, tc.held)
			defer release()

			err := tryLock(t, tc.try)
			var pqErr *pq.Error
			lockTimeout := errors.As(err, &pqErr) && pqErr.Code == "55P03"
			switch {
			case tc.blocked && err == nil:
				t.Fatal("locks acquired while a conflicting session holds them")
			case tc.blocked && !lockTimeout:
				t.Fatalf("want lock_not_available (55P03), got: %v", err)
			case !tc.blocked && err != nil:
				t.Fatalf("locks not acquired: %v", err)
			}
		})
	}
}
