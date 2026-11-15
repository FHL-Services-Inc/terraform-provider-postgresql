package postgresql

import (
	"database/sql"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPostgresqlUserMapping_Basic(t *testing.T) {
	serverName := testUniqueName("myserver_postgres")
	remoteRole := testUniqueName("remote")
	specialRole := testUniqueName("special")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testSuperuserPreCheck(t)
			testDropExtensionsIfExist(t, "postgres_fdw")
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlUserMappingConfig(serverName, remoteRole, specialRole),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "server_name", serverName),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "user_name", remoteRole),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.user", "admin"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.password", "pass"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.special_chars", "options.password", "pass=$*'"),
				),
			},
			testAccUserMappingImportStep("postgresql_user_mapping.remote"),
			testAccUserMappingImportStep("postgresql_user_mapping.special_chars"),
		},
	})
}

func TestAccPostgresqlUserMapping_Update(t *testing.T) {
	serverName := testUniqueName("myserver_postgres")
	remoteRole := testUniqueName("remote")
	specialRole := testUniqueName("special")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testSuperuserPreCheck(t)
			testDropExtensionsIfExist(t, "postgres_fdw")
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlUserMappingConfig(serverName, remoteRole, specialRole),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlServerExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "server_name", serverName),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.password", "pass"),
				),
			},
			{
				Config: testAccPostgresqlUserMappingChanges2(serverName, remoteRole),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlServerExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.password", "passUpdated"),
				),
			},
			{
				Config: testAccPostgresqlUserMappingChanges3(serverName, remoteRole),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlServerExists("postgresql_user_mapping.remote"),
					resource.TestCheckResourceAttr(
						"postgresql_user_mapping.remote", "options.%", "0"),
				),
			},
		},
	})
}

// testAccUserMappingImportStep imports resourceName by its ID and compares
// the imported state with the current one, except for the ignored attributes.
func testAccUserMappingImportStep(resourceName string, ignore ...string) resource.TestStep {
	return resource.TestStep{
		ResourceName:            resourceName,
		ImportState:             true,
		ImportStateVerify:       true,
		ImportStateVerifyIgnore: ignore,
	}
}

// testUserMappingMaskedPreCheck skips unless the provider connects as a
// non-superuser that may use the postgres_fdw wrapper. tests/switch_rds.sh and
// tests/container_acc.sh (rds profile) set that up.
func testUserMappingMaskedPreCheck(t *testing.T) {
	client := testAccProvider.Meta().(*Client)
	if client.config.Superuser {
		t.Skip("Skip test: needs a non-superuser provider connection")
	}
	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect to database: %v", err)
	}
	var usable bool
	if err := db.QueryRow(
		"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_foreign_data_wrapper WHERE fdwname = 'postgres_fdw' AND has_foreign_data_wrapper_privilege(oid, 'USAGE'))",
	).Scan(&usable); err != nil {
		t.Fatalf("could not check postgres_fdw: %v", err)
	}
	if !usable {
		t.Skip("Skip test: postgres_fdw is not installed or not usable by the provider user")
	}
}

// TestAccPostgresqlUserMapping_NonSuperuser covers a non-superuser provider
// that owns the foreign server and manages the mapping of another role
// (issue #43). information_schema._pg_user_mappings is not readable there and
// pg_user_mappings hides the options, so Read must neither fail nor report a
// permanent diff on options (each step's follow-up plan must be empty).
func TestAccPostgresqlUserMapping_NonSuperuser(t *testing.T) {
	serverName := testUniqueName("tf_tests_um_masked_srv")
	roleName := testUniqueName("tf_tests_um_masked_role")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testUserMappingMaskedPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlUserMappingNonSuperuserConfig(serverName, roleName, "pass"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.masked"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.masked", "options.%", "2"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.masked", "options.password", "pass"),
				),
			},
			{
				Config: testAccPostgresqlUserMappingNonSuperuserConfig(serverName, roleName, "passUpdated"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.masked"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.masked", "options.password", "passUpdated"),
				),
			},
			// The options of another role's mapping are not visible here.
			testAccUserMappingImportStep("postgresql_user_mapping.masked", "options"),
		},
	})
}

func testAccPostgresqlUserMappingNonSuperuserConfig(serverName, roleName, password string) string {
	return fmt.Sprintf(`
resource "postgresql_server" "masked" {
  server_name = "%s"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_role" "masked" {
  name = "%s"
}

resource "postgresql_user_mapping" "masked" {
  server_name = postgresql_server.masked.server_name
  user_name   = postgresql_role.masked.name
  options = {
    user     = "remote"
    password = "%s"
  }
}
`, serverName, roleName, password)
}

func checkUserMappingExists(txn *sql.Tx, username string, serverName string) (bool, error) {
	var _rez bool
	// pg_user_mappings reports a PUBLIC mapping as umuser = 0 (usename 'public').
	query := "SELECT TRUE FROM pg_user_mappings WHERE srvname = $1 AND umuser <> 0 AND usename = $2"
	args := []any{serverName, username}
	switch {
	case isPublicUserMapping(username):
		query = "SELECT TRUE FROM pg_user_mappings WHERE srvname = $1 AND umuser = 0"
		args = args[:1]
	case isCurrentUserMapping(username):
		query = "SELECT TRUE FROM pg_user_mappings WHERE srvname = $1 AND usename = current_user"
		args = args[:1]
	}
	err := txn.QueryRow(query, args...).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about user mapping: %s", err)
	}

	return true, nil
}

func testAccCheckPostgresqlUserMappingDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_user_mapping" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkUserMappingExists(txn,
			rs.Primary.Attributes[userMappingUserNameAttr],
			rs.Primary.Attributes[userMappingServerNameAttr])

		if err != nil {
			return fmt.Errorf("error checking user mapping %s", err)
		}

		if exists {
			return fmt.Errorf("User mapping still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlUserMappingExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		username, ok := rs.Primary.Attributes[userMappingUserNameAttr]
		if !ok {
			return fmt.Errorf("No Attribute for username is set")
		}

		serverName, ok := rs.Primary.Attributes[userMappingServerNameAttr]
		if !ok {
			return fmt.Errorf("No Attribute for server name is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkUserMappingExists(txn, username, serverName)

		if err != nil {
			return fmt.Errorf("error checking user mapping %s", err)
		}

		if !exists {
			return fmt.Errorf("User mapping not found")
		}

		return nil
	}
}

func testAccPostgresqlUserMappingConfig(serverName, remoteRole, specialRole string) string {
	return fmt.Sprintf(`
resource "postgresql_extension" "ext_postgres_fdw" {
  name = "postgres_fdw"
}

resource "postgresql_server" "myserver_postgres" {
  server_name = "%[1]s"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
    port   = "5432"
  }

  depends_on = [postgresql_extension.ext_postgres_fdw]
}

resource "postgresql_role" "remote" {
  name = "%[2]s"
}

resource "postgresql_user_mapping" "remote" {
  server_name = postgresql_server.myserver_postgres.server_name
  user_name   = postgresql_role.remote.name
  options = {
    user = "admin"
    password = "pass"
  }
}

resource "postgresql_role" "special" {
  name = "%[3]s"
}

resource "postgresql_user_mapping" "special_chars" {
  server_name = postgresql_server.myserver_postgres.server_name
  user_name   = postgresql_role.special.name
  options = {
	user = "admin"
	password = "pass=$*'"
  }
}
`, serverName, remoteRole, specialRole)
}

func testAccPostgresqlUserMappingChanges2(serverName, remoteRole string) string {
	return fmt.Sprintf(`
resource "postgresql_extension" "ext_postgres_fdw" {
	name = "postgres_fdw"
  }
  
  resource "postgresql_server" "myserver_postgres" {
	server_name = "%[1]s"
	fdw_name    = "postgres_fdw"
	options = {
	  host   = "foo"
	  dbname = "foodb"
	  port   = "5432"
	}
  
	depends_on = [postgresql_extension.ext_postgres_fdw]
  }
  
  resource "postgresql_role" "remote" {
	name = "%[2]s"
  }
  
  resource "postgresql_user_mapping" "remote" {
	server_name = postgresql_server.myserver_postgres.server_name
	user_name   = postgresql_role.remote.name
	options = {
	  user = "admin"
	  password = "passUpdated"
	}
  }
`, serverName, remoteRole)
}

func testAccPostgresqlUserMappingChanges3(serverName, remoteRole string) string {
	return fmt.Sprintf(`
resource "postgresql_extension" "ext_postgres_fdw" {
	name = "postgres_fdw"
  }
  
  resource "postgresql_server" "myserver_postgres" {
	server_name = "%[1]s"
	fdw_name    = "postgres_fdw"
	options = {
	  host   = "foo"
	  dbname = "foodb"
	  port   = "5432"
	}
  
	depends_on = [postgresql_extension.ext_postgres_fdw]
  }
  
  resource "postgresql_role" "remote" {
	name = "%[2]s"
  }
  
  resource "postgresql_user_mapping" "remote" {
	server_name = postgresql_server.myserver_postgres.server_name
	user_name   = postgresql_role.remote.name
  }
`, serverName, remoteRole)
}

// testUserMappingPublicPreCheck makes postgres_fdw available to the provider
// user: a superuser installs it, a non-superuser (rds profile) needs it to be
// installed and usable already.
func testUserMappingPublicPreCheck(t *testing.T) {
	if testAccProvider.Meta().(*Client).config.Superuser {
		config := getTestConfig(t)
		dbExecute(t, config.connStr("postgres"), "CREATE EXTENSION IF NOT EXISTS postgres_fdw")
		return
	}
	testUserMappingMaskedPreCheck(t)
}

// TestAccPostgresqlUserMapping_Public covers user mappings for PUBLIC
// (issues #121 and #150). The catalog reports them as 'PUBLIC' in
// information_schema._pg_user_mappings and as 'public' in pg_user_mappings,
// so Read must find them on both paths: superuser (primary query) and
// non-superuser (fallback). Each step's follow-up plan must be empty.
func TestAccPostgresqlUserMapping_Public(t *testing.T) {
	lowerServer := testUniqueName("tf_tests_um_public_lower")
	upperServer := testUniqueName("tf_tests_um_public_upper")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testUserMappingPublicPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlUserMappingPublicConfig(lowerServer, upperServer, "pass"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.lower"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.lower", "user_name", "public"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.lower", "options.password", "pass"),
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.upper"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.upper", "user_name", "PUBLIC"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.upper", "options.password", "pass"),
				),
			},
			{
				Config: testAccPostgresqlUserMappingPublicConfig(lowerServer, upperServer, "passUpdated"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_user_mapping.lower", "options.password", "passUpdated"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.upper", "options.password", "passUpdated"),
				),
			},
			// The ID keeps the spelling of user_name ("public.<server>",
			// "PUBLIC.<server>"), and so does the imported state.
			testAccUserMappingImportStep("postgresql_user_mapping.lower"),
			testAccUserMappingImportStep("postgresql_user_mapping.upper"),
		},
	})
}

func testAccPostgresqlUserMappingPublicConfig(lowerServer, upperServer, password string) string {
	return fmt.Sprintf(`
resource "postgresql_server" "lower" {
  server_name = "%[1]s"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_server" "upper" {
  server_name = "%[2]s"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_user_mapping" "lower" {
  server_name = postgresql_server.lower.server_name
  user_name   = "public"
  options = {
    user     = "remote"
    password = "%[3]s"
  }
}

resource "postgresql_user_mapping" "upper" {
  server_name = postgresql_server.upper.server_name
  user_name   = "PUBLIC"
  options = {
    user     = "remote"
    password = "%[3]s"
  }
}
`, lowerServer, upperServer, password)
}

// TestAccPostgresqlUserMapping_ImportDots imports mappings whose role or
// server name contains dots, which the "<user_name>.<server_name>" ID can't
// separate syntactically (issue #166). The importer picks the split that
// names an existing mapping.
func TestAccPostgresqlUserMapping_ImportDots(t *testing.T) {
	suffix := acctest.RandString(6)
	dottedRole := "tf_tests.um.role_" + suffix
	dottedServer := "tf_tests.um.srv_" + suffix
	plainRole := "tf_tests_um_role_" + suffix
	plainServer := "tf_tests_um_srv_" + suffix

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testUserMappingPublicPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlUserMappingDotsConfig(dottedRole, dottedServer, plainRole, plainServer),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_user_mapping.dotted_role", "id", dottedRole+"."+plainServer),
					resource.TestCheckResourceAttr("postgresql_user_mapping.dotted_server", "id", plainRole+"."+dottedServer),
				),
			},
			testAccUserMappingImportStep("postgresql_user_mapping.dotted_role"),
			testAccUserMappingImportStep("postgresql_user_mapping.dotted_server"),
			testAccUserMappingImportStep("postgresql_user_mapping.dotted_both"),
			testAccUserMappingImportStep("postgresql_user_mapping.public_dotted_server"),
			{
				ResourceName:  "postgresql_user_mapping.dotted_role",
				ImportState:   true,
				ImportStateId: plainRole + ".no_such_server_" + suffix,
				ExpectError:   regexp.MustCompile("no user mapping matches the ID"),
			},
		},
	})
}

func testAccPostgresqlUserMappingDotsConfig(dottedRole, dottedServer, plainRole, plainServer string) string {
	return fmt.Sprintf(`
resource "postgresql_role" "dotted" {
  name = "%[1]s"
}

resource "postgresql_role" "plain" {
  name = "%[3]s"
}

resource "postgresql_server" "dotted" {
  server_name = "%[2]s"
  fdw_name    = "postgres_fdw"
}

resource "postgresql_server" "plain" {
  server_name = "%[4]s"
  fdw_name    = "postgres_fdw"
}

resource "postgresql_user_mapping" "dotted_role" {
  server_name = postgresql_server.plain.server_name
  user_name   = postgresql_role.dotted.name
}

resource "postgresql_user_mapping" "dotted_server" {
  server_name = postgresql_server.dotted.server_name
  user_name   = postgresql_role.plain.name
}

resource "postgresql_user_mapping" "dotted_both" {
  server_name = postgresql_server.dotted.server_name
  user_name   = postgresql_role.dotted.name
}

resource "postgresql_user_mapping" "public_dotted_server" {
  server_name = postgresql_server.dotted.server_name
  user_name   = "PUBLIC"
}
`, dottedRole, dottedServer, plainRole, plainServer)
}

// TestAccPostgresqlUserMapping_CurrentUser covers the CURRENT_USER,
// CURRENT_ROLE and USER keywords in user_name (issue #167). They must create
// a mapping for the provider role, not for a role of that name, stay in state
// as written (each step's follow-up plan must be empty), and import.
func TestAccPostgresqlUserMapping_CurrentUser(t *testing.T) {
	prefix := testUniqueName("tf_tests_um_cur")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureServer)
			testUserMappingPublicPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlUserMappingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlUserMappingCurrentUserConfig(prefix, "pass"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckUserMappingOfCurrentUser(prefix+"_current_user"),
					testAccCheckUserMappingOfCurrentUser(prefix+"_current_role"),
					testAccCheckUserMappingOfCurrentUser(prefix+"_user"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.current_user", "user_name", "CURRENT_USER"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.current_role", "user_name", "CURRENT_ROLE"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.user", "user_name", "USER"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.current_user", "options.password", "pass"),
				),
			},
			{
				Config: testAccPostgresqlUserMappingCurrentUserConfig(prefix, "passUpdated"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_user_mapping.current_user", "options.password", "passUpdated"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.current_role", "options.password", "passUpdated"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.user", "options.password", "passUpdated"),
				),
			},
			testAccUserMappingImportStep("postgresql_user_mapping.current_user"),
			testAccUserMappingImportStep("postgresql_user_mapping.current_role"),
			testAccUserMappingImportStep("postgresql_user_mapping.user"),
		},
	})
}

// testAccCheckUserMappingOfCurrentUser checks that serverName has a mapping
// for the provider role.
func testAccCheckUserMappingOfCurrentUser(serverName string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		db, err := testAccProvider.Meta().(*Client).Connect()
		if err != nil {
			return err
		}
		var exists bool
		if err := db.QueryRow(
			"SELECT EXISTS (SELECT 1 FROM pg_user_mappings WHERE srvname = $1 AND usename = current_user)", serverName,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("server %s has no user mapping for the current user", serverName)
		}
		return nil
	}
}

func testAccPostgresqlUserMappingCurrentUserConfig(prefix, password string) string {
	return fmt.Sprintf(`
resource "postgresql_server" "current_user" {
  server_name = "%[1]s_current_user"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_server" "current_role" {
  server_name = "%[1]s_current_role"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_server" "user" {
  server_name = "%[1]s_user"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_user_mapping" "current_user" {
  server_name = postgresql_server.current_user.server_name
  user_name   = "CURRENT_USER"
  options = {
    user     = "remote"
    password = "%[2]s"
  }
}

resource "postgresql_user_mapping" "current_role" {
  server_name = postgresql_server.current_role.server_name
  user_name   = "CURRENT_ROLE"
  options = {
    user     = "remote"
    password = "%[2]s"
  }
}

resource "postgresql_user_mapping" "user" {
  server_name = postgresql_server.user.server_name
  user_name   = "USER"
  options = {
    user     = "remote"
    password = "%[2]s"
  }
}
`, prefix, password)
}
