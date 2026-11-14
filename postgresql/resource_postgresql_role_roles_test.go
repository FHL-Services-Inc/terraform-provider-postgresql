package postgresql

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestRoleRolesStatements(t *testing.T) {
	set := func(names ...string) *schema.Set {
		s := schema.NewSet(schema.HashString, nil)
		for _, n := range names {
			s.Add(n)
		}
		return s
	}

	cases := []struct {
		name     string
		old, new *schema.Set
		current  []string
		want     []string
	}{
		{
			name:    "add one, remove one, keep one",
			old:     set("keep", "drop"),
			new:     set("keep", "add"),
			current: []string{"keep", "drop"},
			want:    []string{`REVOKE "drop" FROM "member"`, `GRANT "add" TO "member"`},
		},
		{
			name: "unchanged",
			old:  set("a", "b"),
			new:  set("b", "a"),
			want: nil,
		},
		{
			name:    "remove all",
			old:     set("b", "a"),
			new:     set(),
			current: []string{"a", "b"},
			want:    []string{`REVOKE "a" FROM "member"`, `REVOKE "b" FROM "member"`},
		},
		{
			// "gone" was dropped (e.g. its postgresql_role destroyed in the
			// same apply), which removed the membership with it
			name:    "removed role no longer a membership",
			old:     set("gone", "kept"),
			new:     set("kept"),
			current: []string{"kept"},
			want:    nil,
		},
		{
			name: "from empty, sorted",
			old:  set(),
			new:  set("z", "a"),
			want: []string{`GRANT "a" TO "member"`, `GRANT "z" TO "member"`},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			current := map[string]bool{}
			for _, r := range c.current {
				current[r] = true
			}
			got := roleRolesStatements("member", c.old, c.new, current)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func checkMembership(t *testing.T, dsn, member, group string, wantMember, wantAdmin bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("could not create connection pool: %v", err)
		}
		defer closeDB(t, db)

		var admin bool
		err = db.QueryRow(`
SELECT m.admin_option FROM pg_auth_members m
JOIN pg_roles mr ON mr.oid = m.member
JOIN pg_roles gr ON gr.oid = m.roleid
WHERE mr.rolname = $1 AND gr.rolname = $2`, member, group).Scan(&admin)
		switch {
		case err == sql.ErrNoRows:
			if wantMember {
				return fmt.Errorf("%s is not a member of %s", member, group)
			}
			return nil
		case err != nil:
			t.Fatalf("could not check membership: %v", err)
		}
		if !wantMember {
			return fmt.Errorf("%s is still a member of %s", member, group)
		}
		if admin != wantAdmin {
			return fmt.Errorf("%s in %s: admin_option = %t, want %t", member, group, admin, wantAdmin)
		}
		return nil
	}
}

// TestAccPostgresqlRole_RolesIncremental changes postgresql_role.roles and
// checks that only the added/removed memberships change: a kept membership
// keeps the ADMIN OPTION it was given outside of postgresql_role.
func TestAccPostgresqlRole_RolesIncremental(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, false)
	defer teardown()
	_, prefix := getTestDBNames(dbSuffix)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	member := prefix + "_member"

	tfConfig := func(roles string) string {
		return fmt.Sprintf(`
resource "postgresql_role" "a" {
  name = "%[1]s_a"
}
resource "postgresql_role" "b" {
  name = "%[1]s_b"
}
resource "postgresql_role" "c" {
  name = "%[1]s_c"
}
resource "postgresql_role" "member" {
  name  = "%[1]s_member"
  roles = [%[2]s]
}
`, prefix, roles)
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: tfConfig("postgresql_role.a.name, postgresql_role.b.name"),
				Check: resource.ComposeTestCheckFunc(
					checkMembership(t, dsn, member, prefix+"_a", true, false),
					checkMembership(t, dsn, member, prefix+"_b", true, false),
				),
			},
			{
				// ADMIN OPTION on the kept membership must survive the update.
				PreConfig: func() {
					dbExecute(t, dsn, fmt.Sprintf("GRANT %s_a TO %s WITH ADMIN OPTION", prefix, member))
				},
				Config: tfConfig("postgresql_role.a.name, postgresql_role.c.name"),
				Check: resource.ComposeTestCheckFunc(
					checkMembership(t, dsn, member, prefix+"_a", true, true),
					checkMembership(t, dsn, member, prefix+"_b", false, false),
					checkMembership(t, dsn, member, prefix+"_c", true, false),
				),
			},
			{
				Config: tfConfig(""),
				Check: resource.ComposeTestCheckFunc(
					checkMembership(t, dsn, member, prefix+"_a", false, false),
					checkMembership(t, dsn, member, prefix+"_c", false, false),
				),
			},
		},
	})
}

// TestAccPostgresqlRole_RolesChangeKeepsGrantRole changes postgresql_role.roles
// in the same apply that a postgresql_grant_role (WITH ADMIN OPTION) adds
// another membership to the same member. The grant_role membership must
// survive whichever runs first. postgresql_role.roles stays authoritative, so
// the next plan wants to remove it from the role: that plan is expected.
func TestAccPostgresqlRole_RolesChangeKeepsGrantRole(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, false)
	defer teardown()
	_, prefix := getTestDBNames(dbSuffix)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	member := prefix + "_member"

	tfConfig := func(roles string, withGrant bool) string {
		cfg := fmt.Sprintf(`
resource "postgresql_role" "a" {
  name = "%[1]s_a"
}
resource "postgresql_role" "b" {
  name = "%[1]s_b"
}
resource "postgresql_role" "d" {
  name = "%[1]s_d"
}
resource "postgresql_role" "member" {
  name  = "%[1]s_member"
  roles = [%[2]s]
}
`, prefix, roles)
		if withGrant {
			// by name, so that it runs concurrently with the role update
			cfg += fmt.Sprintf(`
resource "postgresql_grant_role" "d" {
  role              = "%[1]s_member"
  grant_role        = postgresql_role.d.name
  with_admin_option = true
}
`, prefix)
		}
		return cfg
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: tfConfig("postgresql_role.a.name", false),
			},
			{
				Config:             tfConfig("postgresql_role.b.name", true),
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeTestCheckFunc(
					checkMembership(t, dsn, member, prefix+"_a", false, false),
					checkMembership(t, dsn, member, prefix+"_b", true, false),
					checkMembership(t, dsn, member, prefix+"_d", true, true),
				),
			},
			{
				// remove the grant_role before the roles are destroyed
				Config:             tfConfig("postgresql_role.b.name", false),
				ExpectNonEmptyPlan: false,
			},
		},
	})
}

// TestAccPostgresqlRole_RolesRemoveDroppedRole removes a role from roles and
// destroys that role in the same apply. DROP ROLE takes the membership with
// it, so the update must not try to REVOKE a role that no longer exists.
func TestAccPostgresqlRole_RolesRemoveDroppedRole(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, false, false)
	defer teardown()
	_, prefix := getTestDBNames(dbSuffix)

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	member := prefix + "_member"

	withGone := fmt.Sprintf(`
resource "postgresql_role" "kept" {
  name = "%[1]s_kept"
}
resource "postgresql_role" "gone" {
  name = "%[1]s_gone"
}
resource "postgresql_role" "member" {
  name  = "%[1]s_member"
  roles = [postgresql_role.kept.name, postgresql_role.gone.name]
}
`, prefix)
	withoutGone := fmt.Sprintf(`
resource "postgresql_role" "kept" {
  name = "%[1]s_kept"
}
resource "postgresql_role" "member" {
  name  = "%[1]s_member"
  roles = [postgresql_role.kept.name]
}
`, prefix)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: withGone,
				Check: resource.ComposeTestCheckFunc(
					checkMembership(t, dsn, member, prefix+"_kept", true, false),
					checkMembership(t, dsn, member, prefix+"_gone", true, false),
				),
			},
			{
				Config: withoutGone,
				Check:  checkMembership(t, dsn, member, prefix+"_kept", true, false),
			},
		},
	})
}
