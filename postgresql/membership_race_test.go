package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// TestAccPostgresqlGrant_DatabaseMembershipRace runs database grants on
// databases owned by one role while postgresql_database resources with the
// same owner are created and destroyed in the same apply (#152).
//
// For a non-superuser provider, withRolesGranted checks whether the provider
// is already a member of the owner and only grants the membership itself when
// it is not. createDatabase and resourcePostgreSQLDatabaseDelete grant that
// same membership in autocommit and revoke it again afterwards. A grant that
// saw their membership and ran its GRANT ... ON DATABASE after their revoke
// was a no-op with only a WARNING ("no privileges were granted"): the apply
// succeeded, the ACL did not change and the next plan was not empty.
func TestAccPostgresqlGrant_DatabaseMembershipRace(t *testing.T) {
	skipIfNotAcc(t)
	testAccPreCheck(t)

	const (
		// Databases with grants (created outside of Terraform) and databases
		// churned by postgresql_database in each step.
		grantDatabases = 8
		churnDatabases = 8
		steps          = 6
	)

	owner := testUniqueName("tf_tests_race_owner")
	grantee := testUniqueName("tf_tests_race_grantee")
	prefix := testUniqueName("tf_tests_race")

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	adminUser := pq.QuoteIdentifier(config.getDatabaseUsername())

	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", owner))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", grantee))

	// The provider user must not be a member of the owner (that is the case
	// under test), but it needs the membership to create and drop the
	// databases out of band.
	grantDBs := make([]string, grantDatabases)
	dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s", owner, adminUser))
	for i := range grantDBs {
		grantDBs[i] = fmt.Sprintf("%s_g%d", prefix, i)
		dbExecute(t, dsn, fmt.Sprintf("CREATE DATABASE %s OWNER %s", grantDBs[i], owner))
	}
	dbExecute(t, dsn, fmt.Sprintf("REVOKE %s FROM %s", owner, adminUser))
	defer func() {
		dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s", owner, adminUser))
		for _, name := range grantDBs {
			dropTestDatabase(t, config, name)
		}
		dbExecute(t, dsn, fmt.Sprintf("REVOKE %s FROM %s", owner, adminUser))
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", grantee))
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", owner))
	}()

	privilegeSets := [][]string{{"CONNECT"}, {"TEMPORARY"}}

	// Every step replaces all churned databases (the resource address
	// alternates) and flips the privileges of every grant, so each step has
	// 2 * churnDatabases membership windows next to grantDatabases grants.
	tfConfig := func(step int) string {
		privileges := privilegeSets[step%2]
		set := []string{"a", "b"}[step%2]
		return fmt.Sprintf(`
locals {
  grant_databases = ["%[1]s"]
}

resource "postgresql_database" "churn_%[2]s" {
  count = %[3]d
  name  = "%[4]s_%[2]s${count.index}"
  owner = %[5]q
}

resource "postgresql_grant" "db" {
  count       = length(local.grant_databases)
  database    = local.grant_databases[count.index]
  role        = %[6]q
  object_type = "database"
  privileges  = ["%[7]s"]
}
`, strings.Join(grantDBs, `", "`), set, churnDatabases, prefix, owner, grantee, strings.Join(privileges, `", "`))
	}

	testSteps := make([]resource.TestStep, steps)
	for i := range testSteps {
		testSteps[i] = resource.TestStep{
			Config: tfConfig(i),
			Check: resource.ComposeTestCheckFunc(
				testCheckDatabasePrivileges(t, dsn, grantDBs, grantee, privilegeSets[i%2]),
				// The temporary memberships are gone.
				checkUserMembership(t, config.Username, owner, false),
			),
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers: testAccProviders,
		Steps:     testSteps,
	})
}

// TestAccPostgresqlDatabase_AlterOwnershipWithGrantsMaxConnectionsOne changes
// the owner of a database with alter_object_ownership while grants on a
// schema of that database, owned by the new owner, run in the same apply, with
// max_connections = 1 (#152).
//
// setAlterOwnership and withRolesGranted take the membership lock of the new
// owner. setAlterOwnership also needs a connection from the pool of that
// database, whose only connection a grant may hold while it waits for the
// lock: it has to take the connection before the lock, otherwise the apply
// hangs.
func TestAccPostgresqlDatabase_AlterOwnershipWithGrantsMaxConnectionsOne(t *testing.T) {
	skipIfNotAcc(t)
	testAccPreCheck(t)

	const grantees = 4

	owner1 := testUniqueName("tf_tests_reassign_grants_owner1")
	owner2 := testUniqueName("tf_tests_reassign_grants_owner2")
	dbName := testUniqueName("tf_tests_reassign_grants_db")
	roles := make([]string, grantees)
	for i := range roles {
		roles[i] = testUniqueName(fmt.Sprintf("tf_tests_reassign_grants_r%d", i))
	}

	config := getTestConfig(t)

	// A connect_timeout unique to this test gives the provider its own pools
	// (see TestAccPostgresqlDatabase_OwnerMaxConnectionsOne).
	base := fmt.Sprintf(`
provider "postgresql" {
  max_connections = 1
  connect_timeout = 172
}

locals {
  roles = ["%s"]
}

resource "postgresql_role" "owner1" {
  name = %q
}

resource "postgresql_role" "owner2" {
  name = %q
}

resource "postgresql_role" "grantee" {
  count = length(local.roles)
  name  = local.roles[count.index]
}

resource "postgresql_schema" "s" {
  database = postgresql_database.db.name
  name     = "s"
  owner    = postgresql_role.owner2.name
}
`, strings.Join(roles, `", "`), owner1, owner2)

	dbConfig := func(owner string) string {
		return base + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name                   = %q
  owner                  = postgresql_role.%s.name
  alter_object_ownership = true
}
`, dbName, owner)
	}
	// The grants name the database and the schema literally, so Terraform
	// runs them next to the owner change instead of after it.
	// Only one owner change: changing it back would also reassign the schema.
	grantsConfig := func(owner string, privilege string) string {
		return dbConfig(owner) + fmt.Sprintf(`
resource "postgresql_grant" "usage" {
  count       = length(local.roles)
  database    = %q
  schema      = "s"
  role        = local.roles[count.index]
  object_type = "schema"
  privileges  = [%q]

  depends_on = [postgresql_role.grantee, postgresql_schema.s]
}
`, dbName, privilege)
	}

	checkGrants := func(want []string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			db, err := sql.Open("postgres", config.connStr(dbName))
			if err != nil {
				return err
			}
			defer closeDB(t, db)
			for _, role := range roles {
				var got pq.StringArray
				if err := db.QueryRow(`
SELECT coalesce(array_agg(privilege_type::text ORDER BY privilege_type), '{}')
  FROM (SELECT (aclexplode(nspacl)).* FROM pg_namespace WHERE nspname = 's') acl
 WHERE grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)`, role).Scan(&got); err != nil {
					return fmt.Errorf("could not read ACL of schema s: %w", err)
				}
				if !slices.Equal([]string(got), want) {
					return fmt.Errorf("privileges of %s on schema s: got %v, want %v", role, []string(got), want)
				}
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		ProviderFactories: singleConnProviderFactories,
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: dbConfig("owner1"),
				Check:  testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
			},
			{
				Config: grantsConfig("owner2", "USAGE"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_database.db", "owner", owner2),
					checkGrants([]string{"USAGE"}),
					checkUserMembership(t, config.Username, owner1, false),
					checkUserMembership(t, config.Username, owner2, false),
				),
			},
		},
	})
}

// testCheckDatabasePrivileges checks, in the ACL of each database, that role
// holds exactly the privileges want.
func testCheckDatabasePrivileges(t *testing.T, dsn string, databases []string, role string, want []string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer closeDB(t, db)

		var failed []string
		for _, name := range databases {
			var got pq.StringArray
			if err := db.QueryRow(`
SELECT coalesce(array_agg(privilege_type::text ORDER BY privilege_type), '{}')
  FROM (SELECT (aclexplode(datacl)).* FROM pg_database WHERE datname = $1) acl
 WHERE grantee = (SELECT oid FROM pg_roles WHERE rolname = $2)`, name, role).Scan(&got); err != nil {
				return fmt.Errorf("could not read ACL of database %s: %w", name, err)
			}
			if !slices.Equal([]string(got), want) {
				failed = append(failed, fmt.Sprintf("%s: got %v", name, []string(got)))
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("privileges of %s, want %v: %s", role, want, strings.Join(failed, "; "))
		}
		return nil
	}
}

// TestAccPostgresqlDatabase_AlterOwnershipConnectionBeforeLock checks that
// setAlterOwnership does not hold the membership locks while it waits for a
// connection to the target database (#152). A withRolesGranted transaction in
// that database holds the only connection of its pool (max_connections = 1)
// and then asks for the membership lock of the new owner: it must get it,
// while setAlterOwnership waits for the connection.
func TestAccPostgresqlDatabase_AlterOwnershipConnectionBeforeLock(t *testing.T) {
	skipIfNotAcc(t)

	owner1 := testUniqueName("tf_tests_reassign_wait_owner1")
	owner2 := testUniqueName("tf_tests_reassign_wait_owner2")
	dbName := testUniqueName("tf_tests_reassign_wait_db")

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	adminUser := pq.QuoteIdentifier(config.getDatabaseUsername())

	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", owner1))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", owner2))
	dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s", owner1, adminUser))
	dbExecute(t, dsn, fmt.Sprintf("CREATE DATABASE %s OWNER %s", dbName, owner1))
	dbExecute(t, dsn, fmt.Sprintf("REVOKE %s FROM %s", owner1, adminUser))
	defer func() {
		dbExecute(t, dsn, fmt.Sprintf("GRANT %s, %s TO %s", owner1, owner2, adminUser))
		dropTestDatabase(t, config, dbName)
		dbExecute(t, dsn, fmt.Sprintf("REVOKE %s, %s FROM %s", owner1, owner2, adminUser))
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", owner1))
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", owner2))
	}()

	// A connect_timeout unique to this test gives it its own pools in
	// dbRegistry, all limited to one connection.
	config.ConnectTimeoutSec = 173
	config.MaxConns = 1
	config.ConnectionRetryTimeoutSeconds = 30
	// Fingerprint the server, as the provider does by default.
	config.ExpectedVersion = semver.MustParse(defaultExpectedPostgreSQLVersion)
	client := config.NewClient("postgres")
	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect: %v", err)
	}
	target, err := initConnection(client, dbName)
	if err != nil {
		t.Fatalf("could not connect to %s: %v", dbName, err)
	}

	// The grant transaction: holds the only connection to dbName.
	txn, err := target.Begin()
	if err != nil {
		t.Fatalf("could not start transaction: %v", err)
	}

	d := schema.TestResourceDataRaw(t, resourcePostgreSQLDatabase().Schema, map[string]any{
		dbNameAttr:             dbName,
		dbOwnerAttr:            owner2,
		dbAlterObjectOwnership: true,
	})
	alterDone := make(chan error, 1)
	go func() { alterDone <- setAlterOwnership(db, d) }()

	// Let setAlterOwnership reach the wait for the connection.
	time.Sleep(500 * time.Millisecond)

	locked := make(chan struct{})
	go func() {
		defer close(locked)
		lockMembershipsForTxn(txn, []string{owner2})
	}()
	select {
	case <-locked:
	case <-time.After(3 * time.Second):
		t.Error("setAlterOwnership holds the membership lock while it waits for a connection to the target database")
		// Break the cycle: give the connection back, setAlterOwnership
		// finishes and releases the lock.
		if err := txn.Rollback(); err != nil {
			t.Errorf("could not roll back: %v", err)
		}
		<-locked
	}
	deferredRollback(txn)

	select {
	case err := <-alterDone:
		if err != nil {
			t.Fatalf("setAlterOwnership: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("setAlterOwnership did not finish after the connection was released")
	}

	var gotOwner string
	if err := db.QueryRow("SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1", dbName).Scan(&gotOwner); err != nil {
		t.Fatalf("could not read database owner: %v", err)
	}
	if gotOwner != owner2 {
		t.Errorf("owner of %s: got %s, want %s", dbName, gotOwner, owner2)
	}
}

// TestAccPostgresqlSchema_UpdateLocksMembershipsBeforeDDL checks that a schema
// update that renames the schema and changes its policy takes the membership
// lock of the owner before it changes anything (#152). Otherwise it would wait
// for that lock with an uncommitted change to the schema, while the holder (an
// owner change of the database, setAlterOwnership) may wait for that change in
// REASSIGN OWNED: a hang PostgreSQL cannot detect.
func TestAccPostgresqlSchema_UpdateLocksMembershipsBeforeDDL(t *testing.T) {
	skipIfNotAcc(t)
	testAccPreCheck(t)

	owner := testUniqueName("tf_tests_schema_prelock_owner")
	grantee := testUniqueName("tf_tests_schema_prelock_grantee")
	dbName := testUniqueName("tf_tests_schema_prelock_db")

	config := getTestConfig(t)
	dsn := config.connStr("postgres")
	adminUser := pq.QuoteIdentifier(config.getDatabaseUsername())

	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", owner))
	dbExecute(t, dsn, fmt.Sprintf("CREATE ROLE %s", grantee))
	dbExecute(t, dsn, fmt.Sprintf("CREATE DATABASE %s", dbName))
	defer func() {
		dropTestDatabase(t, config, dbName)
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", grantee))
		dbExecute(t, dsn, fmt.Sprintf("DROP ROLE IF EXISTS %s", owner))
	}()
	// Renaming the schema needs ownership, which setSchemaName does not get
	// through withRolesGranted: the provider user stays a member of the owner.
	dbExecute(t, dsn, fmt.Sprintf("GRANT %s TO %s", owner, adminUser))
	defer dbExecute(t, dsn, fmt.Sprintf("REVOKE %s FROM %s", owner, adminUser))
	dbExecute(t, config.connStr(dbName), fmt.Sprintf("CREATE SCHEMA s_old AUTHORIZATION %s", owner))

	r := resourcePostgreSQLSchema()
	state := &terraform.InstanceState{
		ID: dbName + ".s_old",
		Attributes: map[string]string{
			"id":                    dbName + ".s_old",
			schemaNameAttr:          "s_old",
			schemaOwnerAttr:         owner,
			schemaDatabaseAttr:      dbName,
			schemaIfNotExists:       "true",
			schemaDropCascade:       "false",
			schemaPolicyAttr + ".#": "0",
		},
	}
	cfg := terraform.NewResourceConfigRaw(map[string]any{
		schemaNameAttr:     "s_new",
		schemaOwnerAttr:    owner,
		schemaDatabaseAttr: dbName,
		schemaPolicyAttr: []any{map[string]any{
			schemaPolicyRoleAttr:  grantee,
			schemaPolicyUsageAttr: true,
		}},
	})
	diff, err := r.Diff(context.Background(), state, cfg, nil)
	if err != nil {
		t.Fatalf("could not diff: %v", err)
	}
	d, err := schema.InternalMap(r.Schema).Data(state, diff)
	if err != nil {
		t.Fatalf("could not build resource data: %v", err)
	}
	if !d.HasChange(schemaNameAttr) || !d.HasChange(schemaPolicyAttr) {
		t.Fatal("expected a change of the name and the policy")
	}

	db, err := testAccProvider.Meta().(*Client).Connect()
	if err != nil {
		t.Fatalf("could not connect: %v", err)
	}

	// Stands in for an owner change of the database holding the lock.
	unlock := lockDatabaseMemberships(owner)
	updateDone := make(chan error, 1)
	go func() { updateDone <- resourcePostgreSQLSchemaUpdate(db, d) }()

	// Let the update reach the lock.
	time.Sleep(500 * time.Millisecond)

	// A transaction that has changed anything has a transaction ID.
	var writers int
	if err := db.QueryRow(
		"SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_xid IS NOT NULL", dbName,
	).Scan(&writers); err != nil {
		unlock()
		t.Fatalf("could not read pg_stat_activity: %v", err)
	}
	unlock()
	if writers != 0 {
		t.Error("the schema update changed the schema before it took the membership lock of the owner")
	}

	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("schema update: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("schema update did not finish after the lock was released")
	}

	target, err := sql.Open("postgres", config.connStr(dbName))
	if err != nil {
		t.Fatalf("could not connect to %s: %v", dbName, err)
	}
	defer closeDB(t, target)
	var usage bool
	if err := target.QueryRow("SELECT has_schema_privilege($1, 's_new', 'USAGE')", grantee).Scan(&usage); err != nil {
		t.Fatalf("could not read privileges on s_new: %v", err)
	}
	if !usage {
		t.Errorf("%s has no USAGE on the renamed schema", grantee)
	}
}
