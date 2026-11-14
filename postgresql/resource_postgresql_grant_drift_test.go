package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func privSet(privs ...string) *schema.Set {
	s := schema.NewSet(schema.HashString, nil)
	for _, p := range privs {
		s.Add(p)
	}
	return s
}

// TestGrantReadDriftPrivileges checks the value the Read stores for a grant
// covering several objects. It must differ from the wanted privileges as soon
// as one object differs (missing or extra privilege), and a second refresh,
// which compares against that stored value, must keep it: otherwise the drift
// flips back and disappears from the plan.
func TestGrantReadDriftPrivileges(t *testing.T) {
	equal := func(granted, wanted *schema.Set) bool { return granted.Equal(wanted) }

	cases := []struct {
		name    string
		wanted  *schema.Set
		objects []*schema.Set
		drift   bool
		stored  *schema.Set
	}{
		{
			name:    "all objects match",
			wanted:  privSet("SELECT"),
			objects: []*schema.Set{privSet("SELECT"), privSet("SELECT")},
		},
		{
			name:    "no object",
			wanted:  privSet("SELECT"),
			objects: nil,
		},
		{
			name:    "uniform change",
			wanted:  privSet("SELECT"),
			objects: []*schema.Set{privSet("SELECT", "INSERT"), privSet("SELECT", "INSERT")},
			drift:   true, stored: privSet("SELECT", "INSERT"),
		},
		{
			name:    "missing on one object, first",
			wanted:  privSet("SELECT"),
			objects: []*schema.Set{privSet(), privSet("SELECT")},
			drift:   true, stored: privSet("SELECT (on some objects only)"),
		},
		{
			name:    "missing on one object, last",
			wanted:  privSet("SELECT"),
			objects: []*schema.Set{privSet("SELECT"), privSet()},
			drift:   true, stored: privSet("SELECT (on some objects only)"),
		},
		{
			name:    "extra on one object",
			wanted:  privSet("SELECT"),
			objects: []*schema.Set{privSet("SELECT", "INSERT"), privSet("SELECT")},
			drift:   true, stored: privSet("SELECT", "INSERT (on some objects only)"),
		},
		{
			name:    "missing and extra on different objects",
			wanted:  privSet("SELECT", "UPDATE"),
			objects: []*schema.Set{privSet("SELECT"), privSet("SELECT", "UPDATE", "INSERT"), privSet("SELECT", "UPDATE")},
			drift:   true, stored: privSet("SELECT", "UPDATE (on some objects only)", "INSERT (on some objects only)"),
		},
		{
			name:    "revoke-only config, granted on one object",
			wanted:  privSet(),
			objects: []*schema.Set{privSet(), privSet("SELECT")},
			drift:   true, stored: privSet("SELECT (on some objects only)"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, drift := grantReadDriftPrivileges(tc.objects, tc.wanted, equal)
			if drift != tc.drift {
				t.Fatalf("drift = %t, want %t", drift, tc.drift)
			}
			if !drift {
				return
			}
			if !stored.Equal(tc.stored) {
				t.Fatalf("stored %v, want %v", stored.List(), tc.stored.List())
			}
			if stored.Equal(tc.wanted) {
				t.Fatalf("stored value equals the wanted privileges: drift hidden")
			}
			// Next refresh, against the stored value: stable.
			again, drift := grantReadDriftPrivileges(tc.objects, stored, equal)
			if drift && !again.Equal(stored) {
				t.Fatalf("second refresh changed the stored value: %v -> %v", stored.List(), again.List())
			}
		})
	}
}

// TestCreateRevokeQueryPartialPrivileges checks that the REVOKE built from a
// state holding drift markers revokes the underlying privileges.
func TestCreateRevokeQueryPartialPrivileges(t *testing.T) {
	d := schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{
		"role": "r", "database": "db", "schema": "s", "object_type": "table",
		"objects":    []any{"t"},
		"privileges": []any{"SELECT (on some objects only)"},
	})
	if got, want := createRevokeQuery(d.Get), `REVOKE SELECT ON TABLE "s"."t" FROM "r"`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	d = schema.TestResourceDataRaw(t, resourcePostgreSQLGrant().Schema, map[string]any{
		"role": "r", "database": "db", "schema": "s", "object_type": "table",
		"objects":    []any{"t"},
		"privileges": []any{"SELECT", "INSERT (on some objects only)"},
	})
	got := createRevokeQuery(d.Get)
	if got != `REVOKE INSERT,SELECT ON TABLE "s"."t" FROM "r"` && got != `REVOKE SELECT,INSERT ON TABLE "s"."t" FROM "r"` {
		t.Fatalf("unexpected revoke query %q", got)
	}
}

// TestAccPostgresqlGrantTableDriftIsStable checks end to end that drift on a
// grant covering several tables shows on consecutive refreshes (each PlanOnly
// step refreshes twice), whether one table lost a privilege or gained one,
// and that applying fixes both.
func TestAccPostgresqlGrantTableDriftIsStable(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)
	dsn := config.connStr(dbName)

	for _, stmt := range []string{
		"CREATE TABLE test_schema.t1 (id int)",
		"CREATE TABLE test_schema.t2 (id int)",
		"CREATE TABLE dev_schema.d1 (id int)",
	} {
		dbExecute(t, dsn, stmt)
	}

	role := pq.QuoteIdentifier(roleName)
	tfConfig := fmt.Sprintf(`
resource "postgresql_grant" "listed" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "test_schema"
	object_type = "table"
	objects     = ["t1", "t2"]
	privileges  = ["SELECT"]
}

resource "postgresql_grant" "all_tables" {
	database    = "%[1]s"
	role        = "%[2]s"
	schema      = "dev_schema"
	object_type = "table"
	objects     = []
	privileges  = ["SELECT"]
}
`, dbName, roleName)

	exec := func(stmts ...string) func() {
		return func() {
			for _, stmt := range stmts {
				dbExecute(t, dsn, stmt)
			}
		}
	}
	driftStep := func(preConfig func()) resource.TestStep {
		return resource.TestStep{PreConfig: preConfig, Config: tfConfig, PlanOnly: true, ExpectNonEmptyPlan: true}
	}
	hasPrivilege := func(table, privilege string, want bool) resource.TestCheckFunc {
		return func(*terraform.State) error {
			db, err := sql.Open("postgres", dsn)
			if err != nil {
				return err
			}
			defer closeDB(t, db)
			var got bool
			if err := db.QueryRow("SELECT has_table_privilege($1, $2, $3)", roleName, table, privilege).Scan(&got); err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("%s on %s for %s = %t, want %t", privilege, table, roleName, got, want)
			}
			return nil
		}
	}
	noop := func() {}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{Config: tfConfig},
			// One table lost the privilege: drift on every refresh.
			driftStep(exec(fmt.Sprintf("REVOKE SELECT ON test_schema.t1 FROM %s", role))),
			driftStep(noop),
			// One table gained a privilege.
			driftStep(exec(
				fmt.Sprintf("GRANT SELECT ON test_schema.t1 TO %s", role),
				fmt.Sprintf("GRANT INSERT ON test_schema.t2 TO %s", role),
			)),
			driftStep(noop),
			// Both at once, on different tables.
			driftStep(exec(fmt.Sprintf("REVOKE SELECT ON test_schema.t1 FROM %s", role))),
			driftStep(noop),
			{
				// Apply grants the missing privilege and revokes the extra one.
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_grant.listed", "privileges.#", "1"),
					resource.TestCheckTypeSetElemAttr("postgresql_grant.listed", "privileges.*", "SELECT"),
					hasPrivilege("test_schema.t1", "SELECT", true),
					hasPrivilege("test_schema.t2", "SELECT", true),
					hasPrivilege("test_schema.t2", "INSERT", false),
				),
			},
			// A whole schema grant and a new table without the privilege.
			driftStep(exec("CREATE TABLE dev_schema.d2 (id int)")),
			driftStep(noop),
			{
				Config: tfConfig,
				Check: resource.ComposeTestCheckFunc(
					hasPrivilege("dev_schema.d1", "SELECT", true),
					hasPrivilege("dev_schema.d2", "SELECT", true),
				),
			},
		},
	})
}
