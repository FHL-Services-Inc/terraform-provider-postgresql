package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// catalogFakeDriver is a fake database/sql driver serving canned rows per
// query, so the snapshot build, the pg_proc load and the direct Reads can be
// told apart by the statements they issue.
type catalogFakeDriver struct {
	mu         sync.Mutex
	statements []string
	rows       func(query string) ([][]driver.Value, error)
	openErr    error
}

func (d *catalogFakeDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.openErr != nil {
		return nil, d.openErr
	}
	return &catalogFakeConn{driver: d}, nil
}

func (d *catalogFakeDriver) count(substr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.statements {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func (d *catalogFakeDriver) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.statements...)
}

type catalogFakeConn struct{ driver *catalogFakeDriver }

func (c *catalogFakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *catalogFakeConn) Close() error                        { return nil }
func (c *catalogFakeConn) Begin() (driver.Tx, error)           { return grantReadTx{}, nil }

func (c *catalogFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.driver.mu.Lock()
	c.driver.statements = append(c.driver.statements, query)
	c.driver.mu.Unlock()
	return driver.RowsAffected(0), nil
}

func (c *catalogFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.mu.Lock()
	c.driver.statements = append(c.driver.statements, query)
	rowsFn := c.driver.rows
	c.driver.mu.Unlock()
	rows, err := rowsFn(query)
	if err != nil {
		return nil, err
	}
	return &catalogFakeRows{rows: rows}, nil
}

type catalogFakeRows struct {
	rows [][]driver.Value
	next int
}

func (r *catalogFakeRows) Columns() []string {
	n := 6
	if len(r.rows) > 0 {
		n = len(r.rows[0])
	}
	cols := make([]string, n)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	return cols
}

func (r *catalogFakeRows) Close() error { return nil }

func (r *catalogFakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

const catalogFakeDriverName = "pg-catalog-snapshot-fake"

var (
	catalogFake         = &catalogFakeDriver{}
	registerCatalogFake sync.Once
)

// catalogFixture is the canned catalog of database "testdb": roles r (grantee
// of every resource) and o (owner), schema s with table t, sequence q and the
// routines f (two overloads, EXECUTE on one) and p, default ACLs by o, and
// the database ACL. Privileges match the resources in grantReadCases.
func catalogFixture() [][]driver.Value {
	acl := func(s string) driver.Value { return s }
	return [][]driver.Value{
		{"r", int64(10), int64(0), "r", nil, nil},
		{"r", int64(11), int64(0), "o", nil, nil},
		{"n", int64(20), int64(0), "s", nil, acl("{o=UC/o,r=U/o,=U/o}")},
		{"n", int64(21), int64(0), "empty", nil, nil},
		{"c", int64(0), int64(20), "t", "r", acl("{o=arwdDxt/o,r=r/o}")},
		{"c", int64(0), int64(20), "q", "S", acl("{o=rwU/o,r=U/o}")},
		{"c", int64(0), int64(21), "noacl", "r", nil},
		{"d", int64(11), int64(20), nil, "r", acl("{r=r/o}")},
		{"d", int64(11), int64(0), nil, "S", acl("{r=r/o}")},
		{"D", int64(0), int64(0), "testdb", nil, acl("{=Tc/o,r=c/o}")},
	}
}

func catalogProcFixture() [][]driver.Value {
	return [][]driver.Value{
		{int64(20), "f", "{o=X/o,r=X*/o}"},
		{int64(20), "f", "{o=X/o}"},
		{int64(20), "p", "{o=X/o,r=X/o}"},
	}
}

// catalogFixtureRows serves the snapshot queries from the fixtures and every
// other (direct Read) query with one row of the shape that Read scans.
func catalogFixtureRows(catalog, procs [][]driver.Value) func(string) ([][]driver.Value, error) {
	return func(query string) ([][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, catalogSnapshotQuery):
			return catalog, nil
		case strings.HasPrefix(query, catalogSnapshotProcQuery):
			return procs, nil
		case strings.Contains(query, "o.column_name"):
			return [][]driver.Value{{true, true, "t", "c", []byte("{UPDATE}")}}, nil
		case strings.Contains(query, "LEFT JOIN LATERAL"):
			return [][]driver.Value{{true, true, "t", []byte("{SELECT}")}}, nil
		default:
			return [][]driver.Value{{true, true, []byte("{SELECT}")}}, nil
		}
	}
}

func clearCatalogSnapshots() {
	catalogSnapshots.mu.Lock()
	defer catalogSnapshots.mu.Unlock()
	catalogSnapshots.entries = map[string]*catalogSnapshotEntry{}
}

func catalogSnapshotBuilds() int {
	catalogSnapshots.mu.Lock()
	defer catalogSnapshots.mu.Unlock()
	return catalogSnapshots.builds
}

// newCatalogSnapshotConnection registers a DBConnection backed by catalogFake
// as the pool of database "testdb" (and "otherdb") for a provider configured
// with catalog_snapshot = true.
func newCatalogSnapshotConnection(t *testing.T, rows func(string) ([][]driver.Value, error), openErr error) *Client {
	return newCatalogConnection(t, true, rows, openErr)
}

func newCatalogConnection(t *testing.T, snapshot bool, rows func(string) ([][]driver.Value, error), openErr error) *Client {
	t.Helper()
	registerCatalogFake.Do(func() { sql.Register(catalogFakeDriverName, catalogFake) })
	catalogFake.mu.Lock()
	catalogFake.statements = nil
	catalogFake.rows = rows
	catalogFake.openErr = openErr
	catalogFake.mu.Unlock()
	clearCatalogSnapshots()
	t.Cleanup(clearCatalogSnapshots)

	config := Config{Scheme: "awspostgres", CatalogSnapshot: snapshot, CatalogSnapshotTTLSeconds: defaultCatalogSnapshotTTLSeconds}
	client := config.NewClient("testdb")
	for _, database := range []string{"testdb", "otherdb"} {
		sqlDB, err := sql.Open(catalogFakeDriverName, "")
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		sqlDB.SetMaxIdleConns(0)
		dbClient := config.NewClient(database)
		conn := &DBConnection{DB: sqlDB, client: dbClient, version: semver.MustParse("16.0.0")}
		dsn := dbClient.config.connStr(database)
		dbRegistryLock.Lock()
		dbRegistry[dsn] = conn
		dbRegistryLock.Unlock()
		t.Cleanup(func() {
			dbRegistryLock.Lock()
			delete(dbRegistry, dsn)
			dbRegistryLock.Unlock()
			_ = sqlDB.Close()
		})
	}
	return client
}

// snapshotCovered reports whether the snapshot serves this Read case.
func snapshotCovered(tc grantReadCase) bool {
	switch tc.attrs["object_type"] {
	case "column", "foreign_data_wrapper", "foreign_server":
		return false
	}
	return true
}

func readCase(t *testing.T, client *Client, tc grantReadCase, override map[string]any) *schema.ResourceData {
	t.Helper()
	attrs := map[string]any{}
	for k, v := range tc.attrs {
		attrs[k] = v
	}
	for k, v := range override {
		attrs[k] = v
	}
	d := schema.TestResourceDataRaw(t, tc.res.Schema, attrs)
	d.SetId("some-id")
	if err := PGResourceFunc(tc.read)(d, client); err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	return d
}

func privilegesOfData(d *schema.ResourceData) []string {
	var out []string
	for _, p := range d.Get("privileges").(*schema.Set).List() {
		out = append(out, p.(string))
	}
	return out
}

// TestCatalogSnapshotServesReadsFromOneBuild pins the point of the snapshot:
// every covered Read of one database costs no statement of its own; the
// database is read once (plus pg_proc once, for the routine grants), while
// the uncovered object types keep their direct statement.
func TestCatalogSnapshotServesReadsFromOneBuild(t *testing.T) {
	client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)

	direct := 0
	for _, tc := range grantReadCases() {
		d := readCase(t, client, tc, nil)
		if d.Id() == "" {
			t.Errorf("%s: Read of an existing grant cleared its ID", tc.name)
		}
		if !snapshotCovered(tc) {
			direct++
			continue
		}
		if got, want := privilegesOfData(d), tc.attrs["privileges"]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: privileges after Read = %v, want %v", tc.name, got, want)
		}
	}

	if got := catalogFake.count(catalogSnapshotQuery); got != 1 {
		t.Errorf("catalog snapshot built %d times, want 1", got)
	}
	if got := catalogFake.count(catalogSnapshotProcQuery); got != 1 {
		t.Errorf("pg_proc loaded %d times, want 1", got)
	}
	if got := len(catalogFake.all()); got != 2+direct {
		t.Errorf("%d statements for %d Reads, want %d (snapshot + pg_proc + %d direct): %q", got, len(grantReadCases()), 2+direct, direct, catalogFake.all())
	}
}

// TestCatalogSnapshotDetectsDrift: the first object (by name) whose privileges
// differ is written to state, as the direct Read does; a table without any
// ACL reads as no privileges.
func TestCatalogSnapshotDetectsDrift(t *testing.T) {
	client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
	cases := grantReadCases()

	d := readCase(t, client, cases[0], map[string]any{"schema": "empty"}) // table noacl: NULL relacl
	if got := privilegesOfData(d); fmt.Sprint(got) != "[]" {
		t.Errorf("table grant on a table without ACL: privileges = %v, want []", got)
	}

	d = readCase(t, client, cases[0], map[string]any{"privileges": []any{"INSERT"}}) // table, all objects
	if got := privilegesOfData(d); fmt.Sprint(got) != "[SELECT]" {
		t.Errorf("table grant with drift: privileges = %v, want [SELECT]", got)
	}

	d = readCase(t, client, cases[1], map[string]any{"objects": []any{"t"}, "privileges": []any{"INSERT"}})
	if got := privilegesOfData(d); fmt.Sprint(got) != "[SELECT]" {
		t.Errorf("table grant on t with drift: privileges = %v, want [SELECT]", got)
	}

	// Overloads merge: f has EXECUTE on one overload only and reads as granted.
	d = readCase(t, client, cases[3], nil)
	if got := privilegesOfData(d); fmt.Sprint(got) != "[EXECUTE]" {
		t.Errorf("function grant on f: privileges = %v, want [EXECUTE]", got)
	}

	// Database grant: PUBLIC has TEMPORARY and CONNECT, the role only CONNECT.
	d = readCase(t, client, cases[9], map[string]any{"role": "public", "privileges": []any{"CONNECT"}})
	if got := privilegesOfData(d); fmt.Sprint(got) != "[CONNECT TEMPORARY]" {
		t.Errorf("database grant to public: privileges = %v, want [CONNECT TEMPORARY]", got)
	}

	if got := catalogFake.count(catalogSnapshotQuery); got != 1 {
		t.Errorf("catalog snapshot built %d times, want 1", got)
	}
}

// TestCatalogSnapshotPrunes pins the "gone" rules of the direct Read on the
// snapshot path: missing role, missing schema, dropped database, and default
// privileges without an entry.
func TestCatalogSnapshotPrunes(t *testing.T) {
	for _, tc := range grantReadCases() {
		if !snapshotCovered(tc) {
			continue
		}
		t.Run(tc.name+"/missing_role", func(t *testing.T) {
			client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
			if d := readCase(t, client, tc, map[string]any{"role": "gone"}); d.Id() != "" {
				t.Errorf("kept ID %q, want it cleared", d.Id())
			}
		})
		if tc.hasSchema {
			t.Run(tc.name+"/missing_schema", func(t *testing.T) {
				client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
				if d := readCase(t, client, tc, map[string]any{"schema": "gone"}); d.Id() != "" {
					t.Errorf("kept ID %q, want it cleared", d.Id())
				}
			})
		}
		t.Run(tc.name+"/dropped_database", func(t *testing.T) {
			dropped := &pq.Error{Code: "3D000", Message: `database "testdb" does not exist`}
			client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), dropped)
			if d := readCase(t, client, tc, nil); d.Id() != "" {
				t.Errorf("kept ID %q, want it cleared", d.Id())
			}
		})
	}

	t.Run("default_privileges_without_entry", func(t *testing.T) {
		client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
		tc := grantReadCases()[12]
		if d := readCase(t, client, tc, map[string]any{"owner": "r"}); d.Id() != "" {
			t.Errorf("default privileges with another owner kept ID %q, want it cleared", d.Id())
		}
		if d := readCase(t, client, tc, map[string]any{"object_type": "function"}); d.Id() != "" {
			t.Errorf("default privileges of another object type kept ID %q, want it cleared", d.Id())
		}
	})
}

// TestCatalogSnapshotInvalidatedByWrites: a write through PGResourceWriteFunc
// drops the snapshot of its database only; PGResourceClusterWriteFunc drops
// every snapshot.
func TestCatalogSnapshotInvalidatedByWrites(t *testing.T) {
	client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
	tc := grantReadCases()[0]
	noop := func(*DBConnection, *schema.ResourceData) error { return nil }
	builds := func() int { return catalogFake.count(catalogSnapshotQuery) }

	readCase(t, client, tc, nil)
	readCase(t, client, tc, nil)
	if got := builds(); got != 1 {
		t.Fatalf("two Reads built the snapshot %d times, want 1", got)
	}

	// A write to another database leaves the snapshot of testdb alone.
	other := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{"database": "otherdb", "role": "r", "object_type": "database", "privileges": []any{"CONNECT"}})
	if err := PGResourceWriteFunc(noop)(other, client); err != nil {
		t.Fatal(err)
	}
	readCase(t, client, tc, nil)
	if got := builds(); got != 1 {
		t.Fatalf("a write to another database rebuilt the snapshot (%d builds), want 1", got)
	}

	d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, tc.attrs)
	if err := PGResourceWriteFunc(noop)(d, client); err != nil {
		t.Fatal(err)
	}
	readCase(t, client, tc, nil)
	if got := builds(); got != 2 {
		t.Fatalf("Read after a write to testdb: %d builds, want 2", got)
	}

	// A failing write invalidates too: its transaction may have committed.
	failing := func(*DBConnection, *schema.ResourceData) error { return errors.New("boom") }
	if err := PGResourceWriteFunc(failing)(d, client); err == nil {
		t.Fatal("failing write returned no error")
	}
	readCase(t, client, tc, nil)
	if got := builds(); got != 3 {
		t.Fatalf("Read after a failed write: %d builds, want 3", got)
	}

	role := schema.TestResourceDataRaw(t, resourcePostgreSQLRole().Schema, map[string]any{"name": "r"})
	if err := PGResourceClusterWriteFunc(noop)(role, client); err != nil {
		t.Fatal(err)
	}
	readCase(t, client, tc, nil)
	if got := builds(); got != 4 {
		t.Fatalf("Read after a role write: %d builds, want 4", got)
	}
}

// TestCatalogSnapshotExpires: a snapshot older than the TTL is rebuilt.
func TestCatalogSnapshotExpires(t *testing.T) {
	client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
	tc := grantReadCases()[0]

	readCase(t, client, tc, nil)
	catalogSnapshots.mu.Lock()
	for _, e := range catalogSnapshots.entries {
		e.snap.builtAt = time.Now().Add(-time.Duration(client.config.CatalogSnapshotTTLSeconds+1) * time.Second)
	}
	catalogSnapshots.mu.Unlock()
	readCase(t, client, tc, nil)
	if got := catalogFake.count(catalogSnapshotQuery); got != 2 {
		t.Errorf("Read after the TTL: %d builds, want 2", got)
	}
}

// TestCatalogSnapshotFallsBackToDirectRead: when the snapshot cannot be built
// (query error, memory cap) the Read runs its own statement as before, and
// the build is not retried before the TTL.
func TestCatalogSnapshotFallsBackToDirectRead(t *testing.T) {
	fixtureRows := catalogFixtureRows(catalogFixture(), catalogProcFixture())
	failing := func(query string) ([][]driver.Value, error) {
		if strings.HasPrefix(query, catalogSnapshotQuery) {
			return nil, errors.New("permission denied for table pg_default_acl")
		}
		return fixtureRows(query)
	}
	tc := grantReadCases()[0]

	t.Run("build_error", func(t *testing.T) {
		client := newCatalogSnapshotConnection(t, failing, nil)
		for i := 0; i < 3; i++ {
			if d := readCase(t, client, tc, nil); d.Id() == "" {
				t.Errorf("Read %d cleared the ID", i)
			}
		}
		if got := catalogFake.count(catalogSnapshotQuery); got != 1 {
			t.Errorf("build attempted %d times, want 1 (backed off until the TTL)", got)
		}
		if got := catalogFake.count("aclexplode(c.relacl)"); got != 3 {
			t.Errorf("%d direct Reads, want 3: %q", got, catalogFake.all())
		}
	})

	t.Run("memory_cap", func(t *testing.T) {
		defer func(max int) { catalogSnapshotMaxBytes = max }(catalogSnapshotMaxBytes)
		catalogSnapshotMaxBytes = 16
		client := newCatalogSnapshotConnection(t, fixtureRows, nil)
		for i := 0; i < 2; i++ {
			if d := readCase(t, client, tc, nil); d.Id() == "" {
				t.Errorf("Read %d cleared the ID", i)
			}
		}
		if got := catalogFake.count(catalogSnapshotQuery); got != 1 {
			t.Errorf("build attempted %d times, want 1", got)
		}
		if got := catalogFake.count("aclexplode(c.relacl)"); got != 2 {
			t.Errorf("%d direct Reads, want 2", got)
		}
	})

	t.Run("unparseable_acl", func(t *testing.T) {
		catalog := catalogFixture()
		catalog[4][5] = "{unknown (OID=16388)=r/o}" // table t
		client := newCatalogSnapshotConnection(t, catalogFixtureRows(catalog, catalogProcFixture()), nil)
		if d := readCase(t, client, grantReadCases()[1], nil); d.Id() == "" { // objects = [t]
			t.Error("Read cleared the ID")
		}
		if got := catalogFake.count("aclexplode(c.relacl)"); got != 1 {
			t.Errorf("%d direct Reads, want 1", got)
		}
		// The snapshot stays for the Reads it can serve.
		readCase(t, client, grantReadCases()[2], nil)
		if got := catalogFake.count(catalogSnapshotQuery); got != 1 {
			t.Errorf("build attempted %d times, want 1", got)
		}
	})
}

// TestCatalogSnapshotBuiltAcrossWriteIsRebuilt: a snapshot whose build was in
// flight when a write landed is not published; the Read rebuilds it.
func TestCatalogSnapshotBuiltAcrossWriteIsRebuilt(t *testing.T) {
	fixtureRows := catalogFixtureRows(catalogFixture(), catalogProcFixture())
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var builds int
	var mu sync.Mutex
	blocking := func(query string) ([][]driver.Value, error) {
		if strings.HasPrefix(query, catalogSnapshotQuery) {
			mu.Lock()
			builds++
			first := builds == 1
			mu.Unlock()
			if first {
				started <- struct{}{}
				<-release
			}
		}
		return fixtureRows(query)
	}
	client := newCatalogSnapshotConnection(t, blocking, nil)
	tc := grantReadCases()[0]

	done := make(chan *schema.ResourceData)
	go func() { done <- readCase(t, client, tc, nil) }()
	<-started
	invalidateCatalogSnapshot(client, "testdb")
	close(release)
	if d := <-done; d.Id() == "" {
		t.Error("Read cleared the ID")
	}

	if got := catalogFake.count(catalogSnapshotQuery); got != 2 {
		t.Errorf("%d builds, want 2 (the first straddled the write)", got)
	}
	catalogSnapshots.mu.Lock()
	defer catalogSnapshots.mu.Unlock()
	for dsn, e := range catalogSnapshots.entries {
		if e.snap == nil || e.snap.gen != e.gen {
			t.Errorf("%s: cached snapshot is nil or stale (snap=%v)", dsn, e.snap)
		}
	}
}

// TestCatalogSnapshotOffKeepsDirectReads pins the opt-in: without
// catalog_snapshot no snapshot query is issued at all.
func TestCatalogSnapshotOffKeepsDirectReads(t *testing.T) {
	client := newCatalogConnection(t, false, catalogFixtureRows(catalogFixture(), catalogProcFixture()), nil)
	for _, tc := range grantReadCases() {
		readCase(t, client, tc, nil)
	}
	if got := catalogFake.count(catalogSnapshotQuery); got != 0 {
		t.Errorf("snapshot built %d times with catalog_snapshot = false, want 0", got)
	}
	if got, want := len(catalogFake.all()), len(grantReadCases()); got != want {
		t.Errorf("%d statements, want %d (one per Read)", got, want)
	}
}

// --- acceptance ---------------------------------------------------------

// aclFixtureRoles are roles whose names exercise aclitem quoting: a space, a
// double quote, a backslash and a non-ASCII character. They are created on
// the cluster and dropped with the test database.
var aclFixtureRoles = []string{`tf acl space`, `tf_acl"q"`, `tf_acl\b`, `tf_acl_ü`}

// setupACLFixture fills the test database with ACLs of every shape the
// snapshot reads: grants to the role, to PUBLIC, with grant option, to roles
// with quoted names, a table without ACL, overloaded functions, default ACLs
// with and without schema, schema and database ACLs. It returns the test
// database and role names and the owner of the fixture objects.
func setupACLFixture(t *testing.T) (dbName, roleName, owner string, teardown func()) {
	t.Helper()
	config := getTestConfig(t)
	owner = config.getDatabaseUsername()

	dbSuffix, teardownDB := setupTestDatabase(t, true, true)
	dbName, roleName = getTestDBNames(dbSuffix)
	for _, r := range aclFixtureRoles {
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("CREATE ROLE %s", pq.QuoteIdentifier(r)))
	}
	teardown = func() {
		for _, r := range aclFixtureRoles {
			dbExecute(t, config.connStr(dbName), fmt.Sprintf("DROP OWNED BY %s", pq.QuoteIdentifier(r)))
		}
		teardownDB()
		for _, r := range aclFixtureRoles {
			dbExecute(t, config.connStr("postgres"), fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(r)))
		}
	}

	role := pq.QuoteIdentifier(roleName)
	for _, stmt := range []string{
		"CREATE TABLE test_schema.t1 (id int)",
		"CREATE TABLE test_schema.t2 (id int)",
		"CREATE TABLE test_schema.t3 (id int)",
		"CREATE SEQUENCE test_schema.s1",
		"CREATE FUNCTION test_schema.f(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1'",
		"CREATE FUNCTION test_schema.f(text) RETURNS text LANGUAGE sql AS 'SELECT $1'",
		"CREATE PROCEDURE test_schema.p() LANGUAGE sql AS 'SELECT 1'",
		"REVOKE ALL ON FUNCTION test_schema.f(integer), test_schema.f(text) FROM PUBLIC",
		"REVOKE ALL ON PROCEDURE test_schema.p() FROM PUBLIC",
		"GRANT SELECT, INSERT ON test_schema.t1 TO " + role,
		"GRANT SELECT ON test_schema.t1 TO PUBLIC",
		"GRANT SELECT ON test_schema.t2 TO " + role + " WITH GRANT OPTION",
		"GRANT USAGE ON test_schema.s1 TO " + role,
		"GRANT EXECUTE ON FUNCTION test_schema.f(integer) TO " + role,
		"GRANT EXECUTE ON PROCEDURE test_schema.p() TO " + role,
		"GRANT CREATE ON SCHEMA test_schema TO " + role,
		"GRANT USAGE ON SCHEMA test_schema TO PUBLIC",
		fmt.Sprintf("GRANT CONNECT, TEMPORARY ON DATABASE %s TO %s", pq.QuoteIdentifier(dbName), role),
		"ALTER DEFAULT PRIVILEGES IN SCHEMA test_schema GRANT SELECT ON TABLES TO " + role,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA test_schema GRANT SELECT ON TABLES TO PUBLIC",
		"ALTER DEFAULT PRIVILEGES GRANT USAGE ON SEQUENCES TO " + role,
		"ALTER DEFAULT PRIVILEGES GRANT EXECUTE ON FUNCTIONS TO " + role + " WITH GRANT OPTION",
	} {
		dbExecute(t, config.connStr(dbName), stmt)
	}
	for _, r := range aclFixtureRoles {
		dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT SELECT ON test_schema.t1 TO %s WITH GRANT OPTION", pq.QuoteIdentifier(r)))
		dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT UPDATE ON test_schema.t2 TO %s", pq.QuoteIdentifier(r)))
		dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT USAGE ON SCHEMA test_schema TO %s", pq.QuoteIdentifier(r)))
	}
	return dbName, roleName, owner, teardown
}

// TestAccCatalogSnapshotACLParserMatchesAclexplode cross-checks parseACL
// against aclexplode on every ACL of the fixture database: same grantee
// (PUBLIC as ""), grantor, privilege type and grant option for every item.
func TestAccCatalogSnapshotACLParserMatchesAclexplode(t *testing.T) {
	skipIfNotAcc(t)
	dbName, _, _, teardown := setupACLFixture(t)
	defer teardown()

	config := getTestConfig(t)
	db, err := sql.Open("postgres", config.connStr(dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB(t, db)

	// One row per ACL: its text form and the aclexplode rows as
	// "grantee|grantor|privilege|grantable", both sides sorted in Go.
	const exploded = `(SELECT array_agg(format('%s|%s|%s|%s',
	    COALESCE((SELECT rolname FROM pg_roles WHERE oid = a.grantee), ''),
	    (SELECT rolname FROM pg_roles WHERE oid = a.grantor), a.privilege_type, a.is_grantable::text))
	  FROM aclexplode(__ACL__) a)`
	query := ""
	for _, src := range []struct{ name, catalog, acl string }{
		{"relation ", "pg_class", "relacl"},
		{"schema ", "pg_namespace", "nspacl"},
		{"routine ", "pg_proc", "proacl"},
		{"default acl ", "pg_default_acl", "defaclacl"},
		{"database ", "pg_database", "datacl"},
	} {
		if query != "" {
			query += " UNION ALL "
		}
		query += fmt.Sprintf("SELECT %s || oid::text, %s::text, %s FROM %s WHERE %s IS NOT NULL",
			pq.QuoteLiteral(src.name), src.acl, strings.ReplaceAll(exploded, "__ACL__", src.acl), src.catalog, src.acl)
	}
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGrantReadRows(rows)

	checked, items := 0, 0
	for rows.Next() {
		var object, acl string
		var want pq.StringArray
		if err := rows.Scan(&object, &acl, &want); err != nil {
			t.Fatal(err)
		}
		entries, err := parseACL(acl)
		if err != nil {
			t.Errorf("%s: parseACL(%q): %v", object, acl, err)
			continue
		}
		var got []string
		for _, e := range entries {
			for _, p := range e.Privileges {
				got = append(got, fmt.Sprintf("%s|%s|%s|%t", e.Grantee, e.Grantor, p.Type, p.Grantable))
			}
		}
		sort.Strings(got)
		sort.Strings(want)
		if fmt.Sprint(got) != fmt.Sprint([]string(want)) {
			t.Errorf("%s: parseACL(%q)\n got %v\nwant %v", object, acl, got, []string(want))
		}
		checked++
		items += len(entries)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked < 20 || items < 40 {
		t.Fatalf("only %d ACLs / %d items compared, the fixture is not what this test expects", checked, items)
	}
	t.Logf("%d ACLs, %d aclitems match aclexplode", checked, items)
}

// TestAccCatalogSnapshotEquivalence reads every kind of grant and default
// privileges resource of the fixture twice, directly and from the snapshot,
// and requires the same resulting state: same ID (or both pruned) and the
// same attributes, including the privileges written back on drift.
func TestAccCatalogSnapshotEquivalence(t *testing.T) {
	skipIfNotAcc(t)
	dbName, roleName, owner, teardown := setupACLFixture(t)
	defer teardown()

	// The provider's client, as the Reads use it; the snapshot is taken through
	// the cache, as grantReadSnapshot does, whatever the flag says.
	testAccPreCheck(t)
	db, err := initConnection(testAccProvider.Meta().(*Client), dbName)
	if err != nil {
		t.Fatal(err)
	}
	clearCatalogSnapshots()
	snap, err := catalogSnapshots.get(db)
	if err != nil || snap == nil {
		t.Fatalf("catalog snapshot: snap=%v err=%v", snap, err)
	}

	grant := func(name string, attrs map[string]any) grantReadCase {
		attrs["database"] = dbName
		if _, ok := attrs["role"]; !ok {
			attrs["role"] = roleName
		}
		return grantReadCase{name: "grant/" + name, res: resourcePostgreSQLGrant(), attrs: attrs}
	}
	defaultPrivileges := func(name string, attrs map[string]any) grantReadCase {
		attrs["database"] = dbName
		if _, ok := attrs["role"]; !ok {
			attrs["role"] = roleName
		}
		if _, ok := attrs["owner"]; !ok {
			attrs["owner"] = owner
		}
		return grantReadCase{name: "default_privileges/" + name, res: resourcePostgreSQLDefaultPrivileges(), attrs: attrs}
	}
	cases := []grantReadCase{
		grant("table_all_select", map[string]any{"schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		grant("table_all_insert", map[string]any{"schema": "test_schema", "object_type": "table", "privileges": []any{"INSERT"}}),
		grant("table_t1", map[string]any{"schema": "test_schema", "object_type": "table", "objects": []any{"t1"}, "privileges": []any{"SELECT", "INSERT"}}),
		grant("table_t1_t2_drift", map[string]any{"schema": "test_schema", "object_type": "table", "objects": []any{"t1", "t2"}, "privileges": []any{"SELECT", "INSERT"}}),
		grant("table_t2_grant_option", map[string]any{"schema": "test_schema", "object_type": "table", "objects": []any{"t2"}, "privileges": []any{"SELECT"}, "with_grant_option": true}),
		grant("table_t3_no_acl", map[string]any{"schema": "test_schema", "object_type": "table", "objects": []any{"t3"}, "privileges": []any{"SELECT"}}),
		grant("table_missing_object", map[string]any{"schema": "test_schema", "object_type": "table", "objects": []any{"nope"}, "privileges": []any{"SELECT"}}),
		grant("table_public", map[string]any{"role": "public", "schema": "test_schema", "object_type": "table", "objects": []any{"t1"}, "privileges": []any{"SELECT"}}),
		grant("table_all_public_drift", map[string]any{"role": "public", "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		grant("table_quoted_role", map[string]any{"role": aclFixtureRoles[1], "schema": "test_schema", "object_type": "table", "objects": []any{"t1"}, "privileges": []any{"SELECT"}}),
		grant("table_space_role_all", map[string]any{"role": aclFixtureRoles[0], "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		grant("table_backslash_role", map[string]any{"role": aclFixtureRoles[2], "schema": "test_schema", "object_type": "table", "objects": []any{"t2"}, "privileges": []any{"UPDATE"}}),
		grant("table_unicode_role", map[string]any{"role": aclFixtureRoles[3], "schema": "test_schema", "object_type": "table", "objects": []any{"t2"}, "privileges": []any{"UPDATE"}}),
		grant("table_all_privileges", map[string]any{"schema": "test_schema", "object_type": "table", "objects": []any{"t1"}, "privileges": []any{"ALL"}}),
		grant("sequence", map[string]any{"schema": "test_schema", "object_type": "sequence", "privileges": []any{"USAGE"}}),
		grant("sequence_s1", map[string]any{"schema": "test_schema", "object_type": "sequence", "objects": []any{"s1"}, "privileges": []any{"USAGE"}}),
		grant("function_f", map[string]any{"schema": "test_schema", "object_type": "function", "objects": []any{"f"}, "privileges": []any{"EXECUTE"}}),
		grant("function_f_signature", map[string]any{"schema": "test_schema", "object_type": "function", "objects": []any{"f(text)"}, "privileges": []any{"EXECUTE"}}),
		grant("function_all", map[string]any{"schema": "test_schema", "object_type": "function", "privileges": []any{"EXECUTE"}}),
		grant("procedure_all", map[string]any{"schema": "test_schema", "object_type": "procedure", "privileges": []any{"EXECUTE"}}),
		grant("routine_p", map[string]any{"schema": "test_schema", "object_type": "routine", "objects": []any{"p"}, "privileges": []any{"EXECUTE"}}),
		grant("schema", map[string]any{"schema": "test_schema", "object_type": "schema", "privileges": []any{"USAGE", "CREATE"}}),
		grant("schema_drift", map[string]any{"schema": "test_schema", "object_type": "schema", "privileges": []any{"USAGE"}}),
		grant("schema_public", map[string]any{"role": "public", "schema": "test_schema", "object_type": "schema", "privileges": []any{"USAGE"}}),
		grant("schema_quoted_role", map[string]any{"role": aclFixtureRoles[1], "schema": "test_schema", "object_type": "schema", "privileges": []any{"USAGE"}}),
		grant("database", map[string]any{"object_type": "database", "privileges": []any{"CONNECT", "TEMPORARY"}}),
		grant("database_drift", map[string]any{"object_type": "database", "privileges": []any{"CONNECT"}}),
		grant("database_public", map[string]any{"role": "public", "object_type": "database", "privileges": []any{"CONNECT"}}),
		grant("missing_role", map[string]any{"role": "tf_acl_missing", "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		grant("missing_schema", map[string]any{"schema": "nope", "object_type": "table", "privileges": []any{"SELECT"}}),
		grant("missing_schema_function", map[string]any{"schema": "nope", "object_type": "function", "privileges": []any{"EXECUTE"}}),
		grant("missing_schema_schema", map[string]any{"schema": "nope", "object_type": "schema", "privileges": []any{"USAGE"}}),
		grant("empty_schema", map[string]any{"schema": "dev_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("tables_schema", map[string]any{"schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("tables_schema_drift", map[string]any{"schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT", "INSERT"}}),
		defaultPrivileges("tables_schema_public", map[string]any{"role": "public", "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("sequences_noschema", map[string]any{"object_type": "sequence", "privileges": []any{"USAGE"}}),
		defaultPrivileges("functions_noschema_grant_option", map[string]any{"object_type": "function", "privileges": []any{"EXECUTE"}, "with_grant_option": true}),
		defaultPrivileges("tables_noschema_none", map[string]any{"object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("tables_noschema_none_empty_input", map[string]any{"object_type": "table", "privileges": []any{}}),
		defaultPrivileges("other_owner", map[string]any{"owner": roleName, "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("missing_owner", map[string]any{"owner": "tf_acl_missing", "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("missing_role", map[string]any{"role": "tf_acl_missing", "schema": "test_schema", "object_type": "table", "privileges": []any{"SELECT"}}),
		defaultPrivileges("missing_schema", map[string]any{"schema": "nope", "object_type": "table", "privileges": []any{"SELECT"}}),
	}

	// When several relations of a schema-wide grant differ from the
	// configuration, the direct Read writes back the privileges of whichever
	// relation the server returned first (an unordered GROUP BY), the snapshot
	// those of the first relation by name. Both produce a non-empty plan and
	// the same REVOKE/GRANT on apply; for these cases the test only requires
	// that both detect the drift.
	severalDrift := map[string]bool{
		"grant/table_all_select":     true, // t1 has INSERT too, t3 has nothing
		"grant/table_all_insert":     true,
		"grant/table_space_role_all": true, // SELECT on t1, UPDATE on t2, nothing on t3
	}

	state := func(tc grantReadCase, d *schema.ResourceData) string {
		name := tc.name
		if d.Id() == "" {
			return "<pruned>"
		}
		s := d.State()
		if s == nil {
			return "<nil state>"
		}
		keys := make([]string, 0, len(s.Attributes))
		for k := range s.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		fmt.Fprintf(&b, "id=%s", s.ID)
		for _, k := range keys {
			if severalDrift[name] && strings.HasPrefix(k, "privileges.") {
				continue
			}
			fmt.Fprintf(&b, " %s=%s", k, s.Attributes[k])
		}
		if severalDrift[name] {
			configured := schema.NewSet(schema.HashString, tc.attrs["privileges"].([]any))
			fmt.Fprintf(&b, " drift=%t", !d.Get("privileges").(*schema.Set).Equal(configured))
		}
		return b.String()
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			direct := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			direct.SetId("some-id")
			fromSnapshot := schema.TestResourceDataRaw(t, tc.res.Schema, tc.attrs)
			fromSnapshot.SetId("some-id")

			var errDirect, errSnapshot error
			if strings.HasPrefix(tc.name, "grant/") {
				errDirect = readRolePrivileges(db, direct)
				errSnapshot = readRolePrivilegesFromSnapshot(snap, db, fromSnapshot)
			} else {
				errDirect = readRoleDefaultPrivileges(db, direct)
				errSnapshot = readRoleDefaultPrivilegesFromSnapshot(snap, db, fromSnapshot)
			}
			if errDirect != nil || errSnapshot != nil {
				t.Fatalf("direct: %v, snapshot: %v", errDirect, errSnapshot)
			}
			if got, want := state(tc, fromSnapshot), state(tc, direct); got != want {
				t.Errorf("snapshot Read differs from the direct Read\n  snapshot: %s\n  direct:   %s", got, want)
			} else {
				t.Logf("%s", got)
			}
		})
	}
}

// catalogSnapshotLifecycleConfig manages a table grant, a schema grant, a
// function grant and default privileges on the test database.
func catalogSnapshotLifecycleConfig(dbName, roleName, owner, tablePrivileges string) string {
	return fmt.Sprintf(`
resource "postgresql_grant" "table" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "table"
	privileges  = [%[4]s]
}

resource "postgresql_grant" "schema" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "schema"
	privileges  = ["USAGE"]
}

resource "postgresql_grant" "function" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "function"
	objects     = ["test_snapshot_fn"]
	privileges  = ["EXECUTE"]
}

resource "postgresql_default_privileges" "tables" {
	database    = "%[1]s"
	role        = "%[2]s"
	owner       = "%[3]s"
	schema      = "test_schema"
	object_type = "table"
	privileges  = ["SELECT"]
}
`, dbName, roleName, owner, tablePrivileges)
}

// TestAccPostgresqlGrantSnapshot_Lifecycle runs the grant resources through
// apply, a privilege change and an out-of-band revoke. With
// PGCATALOGSNAPSHOT=true it pins that Reads are served from the snapshot
// (the plan after apply is empty because the write dropped the snapshot) and
// that the next run sees the out-of-band change; without it, the same steps
// run through the direct Reads.
func TestAccPostgresqlGrantSnapshot_Lifecycle(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)
	createTestTables(t, dbSuffix, []string{"test_schema.test_table"}, "")
	dbExecute(t, config.connStr(dbName), "CREATE FUNCTION test_schema.test_snapshot_fn() RETURNS int LANGUAGE sql AS 'SELECT 1'")

	// The Check of a step runs right after its apply, when the write has
	// just dropped the snapshot, so the Reads of the plans before it are
	// visible only as builds: more than at the previous Check with the flag
	// on, none since the start of this test without it (other tests of the
	// package, such as the equivalence test, build snapshots directly).
	snapshotOn := os.Getenv("PGCATALOGSNAPSHOT") == "true"
	clearCatalogSnapshots()
	builds := catalogSnapshotBuilds()
	checkSnapshotUsed := func(*terraform.State) error {
		now := catalogSnapshotBuilds()
		defer func() { builds = now }()
		if snapshotOn && now == builds {
			return fmt.Errorf("no catalog snapshot built since the previous step, the Reads did not use it")
		}
		if !snapshotOn && now != builds {
			return fmt.Errorf("%d catalog snapshot(s) built with catalog_snapshot off", now-builds)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				// The plans the framework runs after this apply are the
				// first Reads after a write.
				Config: catalogSnapshotLifecycleConfig(dbName, roleName, config.Username, `"SELECT"`),
				Check:  resource.TestCheckResourceAttr("postgresql_grant.table", "privileges.#", "1"),
			},
			{
				// The Update drops the snapshot built by the previous plan;
				// otherwise the plan after this apply would still see SELECT.
				Config: catalogSnapshotLifecycleConfig(dbName, roleName, config.Username, `"SELECT", "INSERT"`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.table", "privileges.#", "2"),
					checkSnapshotUsed,
					func(*terraform.State) error {
						return testCheckTablesPrivileges(t, dbName, roleName, []string{"test_schema.test_table"}, []string{"SELECT", "INSERT"})
					},
				),
			},
			{
				// An out-of-band revoke is drift for the next run (a new
				// process starts without snapshots).
				PreConfig: func() {
					dbExecute(t, config.connStr(dbName), fmt.Sprintf("REVOKE INSERT ON ALL TABLES IN SCHEMA test_schema FROM %s", pq.QuoteIdentifier(roleName)))
					dbExecute(t, config.connStr(dbName), fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA test_schema REVOKE SELECT ON TABLES FROM %s", pq.QuoteIdentifier(config.Username), pq.QuoteIdentifier(roleName)))
					clearCatalogSnapshots()
				},
				Config:             catalogSnapshotLifecycleConfig(dbName, roleName, config.Username, `"SELECT", "INSERT"`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Re-applying repairs both, and the plan after it is empty.
				Config: catalogSnapshotLifecycleConfig(dbName, roleName, config.Username, `"SELECT", "INSERT"`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.table", "privileges.#", "2"),
					resource.TestCheckResourceAttr("postgresql_default_privileges.tables", "privileges.#", "1"),
					checkSnapshotUsed,
				),
			},
		},
	})
}
