package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// Tests in this file run the provider with max_idle_connections > 0, where the
// provider keeps idle connections to the databases it touched. Each test sets
// a connect_timeout no other test uses: dbRegistry is process-wide and keyed by
// DSN only, so a pool opened earlier with other settings would be reused.

func idleProviderFactories() map[string]func() (*schema.Provider, error) {
	return map[string]func() (*schema.Provider, error){
		"postgresql": func() (*schema.Provider, error) { return testAccProvider, nil },
	}
}

// TestAccPostgresqlDatabase_RenameWithIdleConnections renames a database the
// provider holds idle pooled connections to. ALTER DATABASE ... RENAME fails
// with "database is being accessed by other users" unless the pool to the old
// name is evicted first.
func TestAccPostgresqlDatabase_RenameWithIdleConnections(t *testing.T) {
	var (
		oldName = testUniqueName("tf_tests_idle_rename_old")
		newName = testUniqueName("tf_tests_idle_rename_new")
	)

	providerBlock := `
provider "postgresql" {
  max_idle_connections = 2
  connect_timeout      = 170
}
`
	dbOnly := func(name string) string {
		return providerBlock + fmt.Sprintf(`
resource "postgresql_database" "db" {
  name = "%s"
}
`, name)
	}
	withSchema := dbOnly(oldName) + `
resource "postgresql_schema" "schema" {
  name     = "idle_rename"
  database = postgresql_database.db.name
}
`

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: idleProviderFactories(),
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				// The schema makes the provider open a pool to the database.
				Config: withSchema,
				Check:  testAccCheckDBConnectionRegistered(oldName, true),
			},
			{
				// Removing the schema still refreshes it first, so the pool to
				// the database holds idle connections after this step.
				Config: dbOnly(oldName),
				Check:  testAccCheckDBConnectionRegistered(oldName, true),
			},
			{
				Config: dbOnly(newName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.db"),
					resource.TestCheckResourceAttr("postgresql_database.db", "name", newName),
					testAccCheckDBConnectionRegistered(oldName, false),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabase_TemplateWithIdleConnections creates a database
// from a managed database the provider holds idle pooled connections to.
// CREATE DATABASE ... TEMPLATE fails with "source database is being accessed
// by other users" unless the template's pool is evicted first.
func TestAccPostgresqlDatabase_TemplateWithIdleConnections(t *testing.T) {
	var (
		srcName = testUniqueName("tf_tests_idle_tpl_src")
		dstName = testUniqueName("tf_tests_idle_tpl_dst")
	)

	src := fmt.Sprintf(`
provider "postgresql" {
  max_idle_connections = 2
  connect_timeout      = 171
}

resource "postgresql_database" "src" {
  name = "%s"
}

resource "postgresql_schema" "schema" {
  name     = "idle_template"
  database = postgresql_database.src.name
}
`, srcName)
	withCopy := src + fmt.Sprintf(`
resource "postgresql_database" "dst" {
  name     = "%s"
  template = postgresql_database.src.name

  depends_on = [postgresql_schema.schema]
}
`, dstName)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: idleProviderFactories(),
		CheckDestroy:      testAccCheckPostgresqlDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: src,
				Check:  testAccCheckDBConnectionRegistered(srcName, true),
			},
			{
				// The refresh of the schema leaves idle connections to src.
				Config: withCopy,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseExists("postgresql_database.dst"),
					resource.TestCheckResourceAttr("postgresql_database.dst", "template", srcName),
				),
			},
		},
	})
}

// registerFakePool puts a pool for database into dbRegistry, as Connect would,
// without dialing anything, and removes it again when the test ends.
func registerFakePool(t *testing.T, c *Config, database string) (*DBConnection, string) {
	t.Helper()
	sqlDB, err := sql.Open("postgres", c.connStr(database))
	if err != nil {
		t.Fatalf("sql.Open() returned unexpected error: %v", err)
	}
	dsn := c.connStr(database)
	conn := &DBConnection{DB: sqlDB, client: c.NewClient(database)}
	dbRegistryLock.Lock()
	dbRegistry[dsn] = conn
	dbRegistryLock.Unlock()
	t.Cleanup(func() {
		dbRegistryLock.Lock()
		delete(dbRegistry, dsn)
		dbRegistryLock.Unlock()
		_ = sqlDB.Close()
	})
	return conn, dsn
}

func dbRegistered(dsn string) bool {
	dbRegistryLock.Lock()
	defer dbRegistryLock.Unlock()
	_, found := dbRegistry[dsn]
	return found
}

// evictOtherDBPool must never close the pool of the database the connection
// itself runs on, but still evict the pools of other databases.
func TestEvictOtherDBPoolSkipsOwnDatabase(t *testing.T) {
	c := &Config{Scheme: "postgres", Host: "evict-other-test", Port: 5432, SSLMode: "disable", MaxIdleConns: 2}
	maint, maintDSN := registerFakePool(t, c, "maint_db")
	_, otherDSN := registerFakePool(t, c, "other_db")

	evictOtherDBPool(maint, "maint_db")
	if !dbRegistered(maintDSN) {
		t.Error("evictOtherDBPool() evicted the pool of the connection's own database")
	}
	if err := maint.Ping(); err != nil && strings.Contains(err.Error(), "database is closed") {
		t.Error("evictOtherDBPool() closed the pool of the connection's own database")
	}

	evictOtherDBPool(maint, "other_db")
	if dbRegistered(otherDSN) {
		t.Error("evictOtherDBPool() left the pool of another database in dbRegistry")
	}
}

// Deleting the database the provider connects to must fail up front with a
// clear error, without evicting the pool every other resource runs on (and
// without blocking connections to it).
func TestDatabaseDeleteRefusesProviderDatabase(t *testing.T) {
	c := &Config{Scheme: "postgres", Host: "delete-maint-test", Port: 5432, SSLMode: "disable", MaxIdleConns: 2}
	maint, maintDSN := registerFakePool(t, c, "maint_db")

	d := schema.TestResourceDataRaw(t, resourcePostgreSQLDatabase().Schema, map[string]any{dbNameAttr: "maint_db"})
	d.SetId("maint_db")

	err := resourcePostgreSQLDatabaseDelete(maint, d)
	if err == nil || !strings.Contains(err.Error(), "provider connects to") {
		t.Errorf("resourcePostgreSQLDatabaseDelete() = %v, want an error about the provider's own database", err)
	}
	if !dbRegistered(maintDSN) {
		t.Error("resourcePostgreSQLDatabaseDelete() evicted the provider's own pool")
	}
}

// TestAccPostgresqlDatabase_DeleteProviderDatabaseWithIdleConnections deletes,
// with max_idle_connections > 0, the very database the provider connects to.
// PostgreSQL never allows that, but the provider used to evict (close) its own
// pool first, failing with "sql: database is closed" and breaking every other
// resource running on that pool.
func TestAccPostgresqlDatabase_DeleteProviderDatabaseWithIdleConnections(t *testing.T) {
	skipIfNotAcc(t)
	testAccPreCheck(t)

	maintName := testUniqueName("tf_tests_idle_maint")
	admin := getTestConfig(t)
	dbExecute(t, admin.connStr("postgres"), fmt.Sprintf("CREATE DATABASE %s", pq.QuoteIdentifier(maintName)))

	config := testAccProvider.Meta().(*Client).config
	config.MaxIdleConns = 2
	client := config.NewClient(maintName)
	t.Cleanup(func() {
		config.evictDBConnection(maintName)
		dropTestDatabase(t, admin, maintName)
	})

	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect to %s: %v", maintName, err)
	}

	d := schema.TestResourceDataRaw(t, resourcePostgreSQLDatabase().Schema, map[string]any{dbNameAttr: maintName})
	d.SetId(maintName)

	err = resourcePostgreSQLDatabaseDelete(db, d)
	if err == nil || !strings.Contains(err.Error(), "provider connects to") {
		t.Fatalf("resourcePostgreSQLDatabaseDelete() = %v, want an error about the provider's own database", err)
	}

	if !dbRegistered(config.connStr(maintName)) {
		t.Error("the provider's own pool was evicted")
	}
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Errorf("the provider's own pool is no longer usable: %v", err)
	}

	var allowConn bool
	adminDB, err := sql.Open("postgres", admin.connStr("postgres"))
	if err != nil {
		t.Fatalf("could not create connection pool: %v", err)
	}
	defer closeDB(t, adminDB)
	if err := adminDB.QueryRow("SELECT datallowconn FROM pg_database WHERE datname = $1", maintName).Scan(&allowConn); err != nil {
		t.Fatalf("could not read datallowconn of %s: %v", maintName, err)
	}
	if !allowConn {
		t.Error("connections to the provider's own database were blocked")
	}
}
