package postgresql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// stmtRecorder is a fake database/sql driver that records every statement
// with its bind arguments. Queries return no rows.
type stmtRecorder struct {
	mu    sync.Mutex
	stmts []string
}

func (r *stmtRecorder) record(query string, args []driver.NamedValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vals := make([]string, 0, len(args))
	for _, a := range args {
		vals = append(vals, fmt.Sprint(a.Value))
	}
	if len(vals) > 0 {
		query += " [" + strings.Join(vals, ",") + "]"
	}
	r.stmts = append(r.stmts, strings.Join(strings.Fields(query), " "))
}

func (r *stmtRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.stmts
	r.stmts = nil
	return out
}

func (r *stmtRecorder) Open(string) (driver.Conn, error) { return &stmtRecorderConn{r: r}, nil }

type stmtRecorderConn struct{ r *stmtRecorder }

func (c *stmtRecorderConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not supported")
}
func (c *stmtRecorderConn) Close() error { return nil }
func (c *stmtRecorderConn) Begin() (driver.Tx, error) {
	c.r.record("BEGIN", nil)
	return &stmtRecorderTx{r: c.r}, nil
}
func (c *stmtRecorderConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.r.record(query, args)
	return driver.RowsAffected(0), nil
}
func (c *stmtRecorderConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.r.record(query, args)
	return emptyRows{}, nil
}

type stmtRecorderTx struct{ r *stmtRecorder }

func (t *stmtRecorderTx) Commit() error   { t.r.record("COMMIT", nil); return nil }
func (t *stmtRecorderTx) Rollback() error { t.r.record("ROLLBACK", nil); return nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"x"} }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

var registerStmtRecorder sync.Once

// newRecordingDBConnection returns a DBConnection backed by the recorder and
// registers it so that startTransaction(db.client, "") finds it.
func newRecordingDBConnection(t *testing.T, rec *stmtRecorder) *DBConnection {
	t.Helper()
	registerStmtRecorder.Do(func() { sql.Register("grantRoleStmtRecorder", rec) })

	// A non-"postgres" scheme keeps the DSN free of the sslmode and
	// connect_timeout parameters, whose order is not stable, so the registry
	// key below matches the one startTransaction computes. Nothing is dialed.
	config := &Config{Scheme: "awspostgres", Host: "grant-role-lock.invalid", Port: 5432, Username: "u"}
	client := config.NewClient("postgres")
	db, err := sql.Open("grantRoleStmtRecorder", "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn := &DBConnection{db, client, semver.MustParse("16.0.0")}

	dsn := config.connStr("postgres")
	dbRegistryLock.Lock()
	dbRegistry[dsn] = conn
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, dsn)
		dbRegistryLock.Unlock()
		_ = db.Close()
	})
	return conn
}

// TestGrantRoleLocksMember checks that postgresql_grant_role takes the
// advisory lock of the member role (the key pgLockRole uses, i.e. what
// postgresql_role takes on update) inside its write transaction, before
// touching the membership, so the two resources serialize on the same member.
func TestGrantRoleLocksMember(t *testing.T) {
	rec := &stmtRecorder{}
	db := newRecordingDBConnection(t, rec)

	d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrantRole().Schema, map[string]any{
		"role":       "member_role",
		"grant_role": "group_role",
	})

	lockPrefix := []string{
		"BEGIN",
		"SET statement_timeout = 0",
		"SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = $1 [member_role]",
		"SELECT pg_advisory_xact_lock(member::bigint) FROM pg_auth_members JOIN pg_roles ON roleid = pg_roles.oid WHERE rolname = $1 [member_role]",
	}

	cases := []struct {
		name string
		fn   func(*DBConnection, *schema.ResourceData) error
		want []string
	}{
		{
			name: "create",
			fn:   resourcePostgreSQLGrantRoleCreate,
			want: append(append([]string{}, lockPrefix...),
				`REVOKE "group_role" FROM "member_role"`,
				`GRANT "group_role" TO "member_role"`,
				"COMMIT",
			),
		},
		{
			name: "delete",
			fn:   resourcePostgreSQLGrantRoleDelete,
			want: append(append([]string{}, lockPrefix...),
				`REVOKE "group_role" FROM "member_role"`,
				"COMMIT",
			),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn(db, d); err != nil {
				t.Fatal(err)
			}
			got := rec.take()
			// Create reads the membership back after the commit.
			if len(got) > len(c.want) {
				got = got[:len(c.want)]
			}
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Fatalf("unexpected statements:\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(c.want, "\n  "))
			}
		})
	}
}

// TestAccPostgresqlGrantRole_SameMemberAsRole manages one member with a
// postgresql_role (roles ignored, as documented for this combination) and
// postgresql_grant_role resources, and changes both in the same applies so
// they run concurrently. Every step is followed by a plan that must be empty.
func TestAccPostgresqlGrantRole_SameMemberAsRole(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, false)
	defer teardown()
	_, prefix := getTestDBNames(dbSuffix)

	const groups = 4
	config := func(iteration int, withGrants bool) string {
		var b strings.Builder
		fmt.Fprintf(&b, `
resource "postgresql_role" "member" {
  name             = "%[1]s_member"
  connection_limit = %[2]d
  roles            = [postgresql_role.base.name]

  lifecycle {
    ignore_changes = [roles]
  }
}

resource "postgresql_role" "base" {
  name = "%[1]s_base"
}
`, prefix, iteration)
		for i := range groups {
			fmt.Fprintf(&b, `
resource "postgresql_role" "group_%[2]d" {
  name = "%[1]s_group_%[2]d"
}
`, prefix, i)
			if withGrants {
				// The member is referenced by name, not by attribute, so that
				// Terraform does not order the grant after the role update.
				fmt.Fprintf(&b, `
resource "postgresql_grant_role" "g%[2]d" {
  role              = "%[1]s_member"
  grant_role        = postgresql_role.group_%[2]d.name
  with_admin_option = %[3]t
}
`, prefix, i, i%2 == 0)
			}
		}
		return b.String()
	}

	steps := []resource.TestStep{{Config: config(0, false)}}
	for i := 1; i <= 5; i++ {
		// Add the grants while the member is updated, then update the member
		// again with the grants in place.
		steps = append(steps,
			resource.TestStep{Config: config(2*i-1, true)},
			resource.TestStep{Config: config(2*i, true)},
			resource.TestStep{Config: config(2*i, false)},
		)
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps:     steps,
	})
}
