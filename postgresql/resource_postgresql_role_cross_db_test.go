package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// A role that owns objects, or holds privileges, in databases other than the
// provider's one can still be deleted: REASSIGN OWNED / DROP OWNED have to run
// in each of these databases, not only in the provider's database (#137).
func TestAccPostgresqlRole_DeleteOwnsObjectsInOtherDatabases(t *testing.T) {
	roleName := testUniqueName("tf_xdb_owner")
	const rolePassword = "xdb-owner-pwd"
	dbNames := []string{
		testUniqueName("tf_xdb_one"),
		testUniqueName("tf_xdb_two"),
		testUniqueName("tf_xdb_three"),
	}

	databasesConfig := fmt.Sprintf(`
resource "postgresql_database" "one" {
  name = "%s"
}

resource "postgresql_database" "two" {
  name = "%s"
}

resource "postgresql_database" "three" {
  name = "%s"
}
`, dbNames[0], dbNames[1], dbNames[2])

	// The role gets CREATE on the first two databases, where it creates its
	// own schema and table (the usual application owner), and only a
	// privilege in the third one.
	withRoleConfig := databasesConfig + fmt.Sprintf(`
resource "postgresql_role" "owner" {
  name     = "%s"
  login    = true
  password = "%s"
}

resource "postgresql_grant" "create_one" {
  database    = postgresql_database.one.name
  role        = postgresql_role.owner.name
  object_type = "database"
  privileges  = ["CREATE"]
}

resource "postgresql_grant" "create_two" {
  database    = postgresql_database.two.name
  role        = postgresql_role.owner.name
  object_type = "database"
  privileges  = ["CREATE"]
}
`, roleName, rolePassword)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: withRoleConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(roleName, nil, nil),
					testAccCreateObjectsAsRole(t, roleName, rolePassword, dbNames[:2]),
					testAccGrantSchemaUsage(t, roleName, dbNames[2]),
				),
			},
			{
				// The role (and its database grants) go away, the objects
				// it owns in the other databases stay.
				Config: databasesConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleDestroy,
					testAccCheckSchemaOwnedByProviderUser(t, dbNames[:2], "xdb_owned"),
				),
			},
		},
	})
}

// testAccCreateObjectsAsRole connects as role to each database and creates a
// schema with a table in it, owned by role.
func testAccCreateObjectsAsRole(t *testing.T, role, password string, dbNames []string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		config := getTestConfig(t)
		config.Username = role
		config.Password = password
		for _, dbName := range dbNames {
			if err := execOnDatabase(config, dbName,
				"CREATE SCHEMA xdb_owned",
				"CREATE TABLE xdb_owned.t (id int)",
			); err != nil {
				return err
			}
		}
		return nil
	}
}

// testAccGrantSchemaUsage creates, as the provider user, a schema in dbName
// and grants USAGE on it to role: the role holds a privilege there but owns
// nothing.
func testAccGrantSchemaUsage(t *testing.T, role, dbName string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		return execOnDatabase(getTestConfig(t), dbName,
			"CREATE SCHEMA xdb_priv",
			fmt.Sprintf("GRANT USAGE ON SCHEMA xdb_priv TO %s", pq.QuoteIdentifier(role)),
		)
	}
}

// testAccCheckSchemaOwnedByProviderUser checks that schema is owned by the
// provider user in each database (REASSIGN OWNED gives the objects to it).
func testAccCheckSchemaOwnedByProviderUser(t *testing.T, dbNames []string, schema string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		config := getTestConfig(t)
		for _, dbName := range dbNames {
			db, err := sql.Open("postgres", config.connStr(dbName))
			if err != nil {
				return fmt.Errorf("could not open connection to %s: %w", dbName, err)
			}
			var owner string
			err = db.QueryRow(
				"SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = $1", schema,
			).Scan(&owner)
			_ = db.Close()
			if err != nil {
				return fmt.Errorf("could not read the owner of schema %s in %s: %w", schema, dbName, err)
			}
			if owner != config.Username {
				return fmt.Errorf("schema %s in %s is owned by %s, expected %s", schema, dbName, owner, config.Username)
			}
		}
		return nil
	}
}

func execOnDatabase(config Config, dbName string, queries ...string) error {
	db, err := sql.Open("postgres", config.connStr(dbName))
	if err != nil {
		return fmt.Errorf("could not open connection to %s: %w", dbName, err)
	}
	defer func() { _ = db.Close() }()
	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			return fmt.Errorf("could not execute %q in %s as %s: %w", query, dbName, config.Username, err)
		}
	}
	return nil
}
