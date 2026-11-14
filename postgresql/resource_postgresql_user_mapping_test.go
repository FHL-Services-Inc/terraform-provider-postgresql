package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

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
				Config: fmt.Sprintf(testAccPostgresqlUserMappingNonSuperuserConfig, "pass"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.masked"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.masked", "options.%", "2"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.masked", "options.password", "pass"),
				),
			},
			{
				Config: fmt.Sprintf(testAccPostgresqlUserMappingNonSuperuserConfig, "passUpdated"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlUserMappingExists("postgresql_user_mapping.masked"),
					resource.TestCheckResourceAttr("postgresql_user_mapping.masked", "options.password", "passUpdated"),
				),
			},
		},
	})
}

var testAccPostgresqlUserMappingNonSuperuserConfig = `
resource "postgresql_server" "masked" {
  server_name = "tf_tests_um_masked_srv"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
  }
}

resource "postgresql_role" "masked" {
  name = "tf_tests_um_masked_role"
}

resource "postgresql_user_mapping" "masked" {
  server_name = postgresql_server.masked.server_name
  user_name   = postgresql_role.masked.name
  options = {
    user     = "remote"
    password = "%s"
  }
}
`

func checkUserMappingExists(txn *sql.Tx, username string, serverName string) (bool, error) {
	var _rez bool
	err := txn.QueryRow("SELECT TRUE FROM pg_user_mappings WHERE usename = $1 AND srvname = $2", username, serverName).Scan(&_rez)
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

		splitted := strings.Split(rs.Primary.ID, ".")
		exists, err := checkUserMappingExists(txn, splitted[0], splitted[1])

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
