package postgresql

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPostgresqlRole_Basic(t *testing.T) {
	myrole2 := testUniqueName("myrole2")
	roleDefault := testUniqueName("role_default")
	roleWithCreateDatabase := testUniqueName("role_with_create_database")
	roleSimple := testUniqueName("role_simple")
	subRole := testUniqueName("sub_role")
	roleWithSearchPath := testUniqueName("role_with_search_path")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlRoleConfig(myrole2, roleSimple, roleDefault, roleWithCreateDatabase, subRole, roleWithSearchPath),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(myrole2, nil, nil),
					resource.TestCheckResourceAttr("postgresql_role.myrole2", "name", myrole2),
					resource.TestCheckResourceAttr("postgresql_role.myrole2", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.myrole2", "roles.#", "0"),

					testAccCheckPostgresqlRoleExists(roleDefault, nil, nil),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "name", roleDefault),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "superuser", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "create_database", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "create_role", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "inherit", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "replication", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "bypass_row_level_security", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "connection_limit", "-1"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "encrypted_password", "true"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "password", ""),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "valid_until", "infinity"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "skip_drop_role", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "skip_reassign_owned", "false"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "statement_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "idle_in_transaction_session_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.role_with_defaults", "assume_role", ""),

					resource.TestCheckResourceAttr("postgresql_role.role_with_create_database", "name", roleWithCreateDatabase),
					resource.TestCheckResourceAttr("postgresql_role.role_with_create_database", "create_database", "true"),

					testAccCheckPostgresqlRoleExists(subRole, []string{myrole2, roleSimple}, nil),
					resource.TestCheckResourceAttr("postgresql_role.sub_role", "name", subRole),
					resource.TestCheckResourceAttr("postgresql_role.sub_role", "roles.#", "2"),
					resource.TestCheckTypeSetElemAttr("postgresql_role.sub_role", "roles.*", myrole2),
					resource.TestCheckTypeSetElemAttr("postgresql_role.sub_role", "roles.*", roleSimple),

					testAccCheckPostgresqlRoleExists(roleWithSearchPath, nil, []string{"bar", "foo-with-hyphen"}),
				),
			},
		},
	})
}

// Test creating a superuser role.
func TestAccPostgresqlRole_Superuser(t *testing.T) {
	roleName := testUniqueName("role_with_superuser")

	roleConfig := fmt.Sprintf(`
resource "postgresql_role" "role_with_superuser" {
  name = "%s"
  superuser = true
  login = true
  password = "mypass"
}`, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
			// Need to a be a superuser to create a superuser
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: roleConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.role_with_superuser", "name", roleName),
					resource.TestCheckResourceAttr("postgresql_role.role_with_superuser", "superuser", "true"),
				),
			},
		},
	})
}

func TestAccPostgresqlRole_Update(t *testing.T) {
	updateRole := testUniqueName("update_role")
	updateRole2 := testUniqueName("update_role2")
	groupRole := testUniqueName("group_role")

	var configCreate = fmt.Sprintf(`
resource "postgresql_role" "update_role" {
  name = "%s"
  login = true
  password = "toto"
  valid_until = "2099-05-04 12:00:00+00"
}
`, updateRole)

	var configUpdate = fmt.Sprintf(`
resource "postgresql_role" "group_role" {
  name = "%s"
}

resource "postgresql_role" "update_role" {
  name = "%s"
  login = true
  connection_limit = 5
  password = "titi"
  roles = ["${postgresql_role.group_role.name}"]
  search_path = ["mysearchpath"]
  statement_timeout = 30000
  idle_in_transaction_session_timeout = 60000
  assume_role = "${postgresql_role.group_role.name}"
}
`, groupRole, updateRole2)
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: configCreate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(updateRole, []string{}, nil),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "name", updateRole),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "connection_limit", "-1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "password", "toto"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "valid_until", "2099-05-04 12:00:00+00"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "statement_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "idle_in_transaction_session_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "assume_role", ""),
					testAccCheckRoleCanLogin(t, updateRole, "toto"),
				),
			},
			{
				Config: configUpdate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(updateRole2, []string{groupRole}, nil),
					resource.TestCheckResourceAttr(
						"postgresql_role.update_role", "name", updateRole2,
					),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "connection_limit", "5"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "password", "titi"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "valid_until", "infinity"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.#", "1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.0", groupRole),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.#", "1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.0", "mysearchpath"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "statement_timeout", "30000"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "idle_in_transaction_session_timeout", "60000"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "assume_role", groupRole),
					testAccCheckRoleCanLogin(t, updateRole2, "titi"),
				),
			},
			// apply the first one again to test that the granted role is correctly
			// revoked and the search path has been reset to default.
			{
				Config: configCreate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(updateRole, []string{}, nil),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "name", updateRole),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "login", "true"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "connection_limit", "-1"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "password", "toto"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "roles.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "search_path.#", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "statement_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "idle_in_transaction_session_timeout", "0"),
					resource.TestCheckResourceAttr("postgresql_role.update_role", "assume_role", ""),
					testAccCheckRoleCanLogin(t, updateRole, "toto"),
				),
			},
		},
	})
}

// Test to create a role with admin user (usually postgres) granted to it
// There were a bug on RDS like setup (with a non-superuser postgres role)
// where it couldn't delete the role in this case.
func TestAccPostgresqlRole_AdminGranted(t *testing.T) {
	admin := os.Getenv("PGUSER")
	if admin == "" {
		admin = "postgres"
	}

	roleName := testUniqueName("test_role")

	roleConfig := fmt.Sprintf(`
resource "postgresql_role" "test_role" {
  name  = "%s"
  roles = [
	  "%s"
  ]
}`, roleName, admin)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			client := testAccProvider.Meta().(*Client)
			db, err := client.Connect()
			if err != nil {
				t.Fatalf("could connect to database: %v", err)
			}
			// Requires >= 9 and <16
			// We disable this test for >= pg16 as it makes no sense with the new createRoleSelfGrant feature
			if !db.featureSupported(featurePrivileges) || db.featureSupported(featureCreateRoleSelfGrant) {
				t.Skipf("Skip extension tests for Postgres %s", db.version)
			}
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: roleConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(roleName, []string{admin}, nil),
					resource.TestCheckResourceAttr("postgresql_role.test_role", "name", roleName),
				),
			},
		},
	})
}

func testAccCheckPostgresqlRoleDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_role" {
			continue
		}

		exists, err := checkRoleExists(client, rs.Primary.ID)

		if err != nil {
			return fmt.Errorf("error checking role %s", err)
		}

		if exists {
			return fmt.Errorf("Role still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlRoleExists(roleName string, grantedRoles []string, searchPath []string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Client)

		exists, err := checkRoleExists(client, roleName)
		if err != nil {
			return fmt.Errorf("error checking role %s", err)
		}

		if !exists {
			return fmt.Errorf("Role not found")
		}

		if grantedRoles != nil {
			if err := checkGrantedRoles(client, roleName, grantedRoles); err != nil {
				return err
			}
		}

		if searchPath != nil {
			if err := checkSearchPath(client, roleName, searchPath); err != nil {
				return err
			}
		}
		return nil
	}
}

func checkRoleExists(client *Client, roleName string) (bool, error) {
	db, err := client.Connect()
	if err != nil {
		return false, err
	}
	var _rez int
	err = db.QueryRow("SELECT 1 from pg_roles d WHERE rolname=$1", roleName).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about role: %s", err)
	}

	return true, nil
}

func testAccCheckRoleCanLogin(t *testing.T, role, password string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		config := getTestConfig(t)
		config.Username = role
		config.Password = password
		db, err := sql.Open("postgres", config.connStr("postgres"))
		if err != nil {
			return fmt.Errorf("could not open SQL connection: %v", err)
		}
		if err := db.Ping(); err != nil {
			return fmt.Errorf("could not connect as role %s: %v", role, err)
		}
		return nil
	}
}

func checkGrantedRoles(client *Client, roleName string, expectedRoles []string) error {
	db, err := client.Connect()
	if err != nil {
		return err
	}

	rows, err := db.Query(
		"SELECT pg_get_userbyid(roleid) as rolname from pg_auth_members WHERE pg_get_userbyid(member) = $1 ORDER BY rolname",
		roleName,
	)
	if err != nil {
		return fmt.Errorf("error reading granted roles: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v", err)
		}
	}()

	grantedRoles := []string{}
	for rows.Next() {
		var grantedRole string
		if err := rows.Scan(&grantedRole); err != nil {
			return fmt.Errorf("error scanning granted role: %v", err)
		}
		grantedRoles = append(grantedRoles, grantedRole)
	}

	sort.Strings(expectedRoles)
	if !reflect.DeepEqual(grantedRoles, expectedRoles) {
		return fmt.Errorf(
			"Role %s is not a member of the expected list of roles. expected %v - got %v",
			roleName, expectedRoles, grantedRoles,
		)
	}
	return nil
}

func checkSearchPath(client *Client, roleName string, expectedSearchPath []string) error {
	db, err := client.Connect()
	if err != nil {
		return err
	}

	var searchPathStr string
	err = db.QueryRow(
		"SELECT (pg_options_to_table(rolconfig)).option_value FROM pg_roles WHERE rolname=$1;",
		roleName,
	).Scan(&searchPathStr)

	// The query returns ErrNoRows if the search path hasn't been altered.
	if err != nil && err == sql.ErrNoRows {
		searchPathStr = "\"$user\", public"
	} else if err != nil {
		return fmt.Errorf("error reading search_path: %v", err)
	}

	searchPath := strings.Split(searchPathStr, ", ")
	for i := range searchPath {
		searchPath[i] = strings.Trim(searchPath[i], `"`)
	}
	sort.Strings(expectedSearchPath)
	if !reflect.DeepEqual(searchPath, expectedSearchPath) {
		return fmt.Errorf(
			"search_path is not equal to expected value. expected %v - got %v",
			expectedSearchPath, searchPath,
		)
	}
	return nil
}

func testAccPostgresqlRoleConfig(myrole2, roleSimple, roleDefault, roleWithCreateDatabase, subRole, roleWithSearchPath string) string {
	return fmt.Sprintf(`
resource "postgresql_role" "myrole2" {
  name = "%[1]s"
  login = true
}

resource "postgresql_role" "role_with_pwd" {
  name = "%[1]s_pwd"
  login = true
  password = "mypass"
}

resource "postgresql_role" "role_with_pwd_encr" {
  name = "%[1]s_pwd_encr"
  login = true
  password = "mypass"
  encrypted_password = true
}

resource "postgresql_role" "role_simple" {
  name = "%[2]s"
}

resource "postgresql_role" "role_with_defaults" {
  name = "%[3]s"
  superuser = false
  create_database = false
  create_role = false
  inherit = false
  login = false
  replication = false
  bypass_row_level_security = false
  connection_limit = -1
  encrypted_password = true
  password = ""
  skip_drop_role = false
  valid_until = "infinity"
  statement_timeout = 0
  idle_in_transaction_session_timeout = 0
  assume_role = ""
}

resource "postgresql_role" "role_with_create_database" {
  name = "%[4]s"
  create_database = true
}

resource "postgresql_role" "sub_role" {
	name = "%[5]s"
	roles = [
		"${postgresql_role.myrole2.id}",
		"${postgresql_role.role_simple.id}",
	]
}

resource "postgresql_role" "role_with_search_path" {
  name = "%[6]s"
  search_path = ["bar", "foo-with-hyphen"]
}
`, myrole2, roleSimple, roleDefault, roleWithCreateDatabase, subRole, roleWithSearchPath)
}

func TestAccPostgresqlRole_WriteOnlyPassword_Basic(t *testing.T) {
	roleName := testUniqueName("wo_pwd_role")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featurePrivileges)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "postgresql_role" "wo_pwd_role" {
  name        = "%s"
  login       = true
  password_wo = "secretpass"
  password_wo_version = "1"
}
`, roleName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleExists(roleName, nil, nil),
					resource.TestCheckNoResourceAttr("postgresql_role.wo_pwd_role", "password_wo"), // must be blank
					resource.TestCheckNoResourceAttr("postgresql_role.wo_pwd_role", "password"),    // must be blank
					testAccCheckRoleCanLogin(t, roleName, "secretpass"),                            // actually usable
				),
			},
		},
	})
}

func TestAccPostgresqlRole_WriteOnlyPassword_Switch(t *testing.T) {
	roleName := testUniqueName("switch_role")

	configPassword := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name     = "%s"
  login    = true
  password = "initialpass"
}`, roleName)

	configPasswordWO := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name        = "%s"
  login       = true
  password_wo = "wopass"
  password_wo_version = "1"
}`, roleName)

	configPasswordWOChangedWithoutVersionChange := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name        = "%s"
  login       = true
  password_wo = "wopass123"
  password_wo_version = "1"
}`, roleName)

	configPasswordReturn := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name     = "%s"
  login    = true
  password = "revertpass"
}`, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: configPassword,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password", "initialpass"),
					testAccCheckRoleCanLogin(t, roleName, "initialpass"),
				),
			},
			{
				Config: configPasswordWO,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleCanLogin(t, roleName, "wopass"),
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password", ""), // value cleared from state
				),
			},
			{
				Config: configPasswordWOChangedWithoutVersionChange,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleCanLogin(t, roleName, "wopass"), //because the version didn't change, the old password still works
				),
			},
			{
				Config: configPasswordReturn,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password", "revertpass"),
					testAccCheckRoleCanLogin(t, roleName, "revertpass"),
				),
			},
		},
	})
}

func TestAccPostgresqlRole_WriteOnlyPassword_WithVersioning(t *testing.T) {
	roleName := testUniqueName("switch_role")

	configWOv1 := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name                 = "%s"
  login                = true
  password_wo          = "wopass1"
  password_wo_version  = 1
}`, roleName)

	configWOv1NewPass := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name                 = "%s"
  login                = true
  password_wo          = "wopass2"
  password_wo_version  = "1"
}`, roleName)

	configWOv2 := fmt.Sprintf(`
resource "postgresql_role" "switch_role" {
  name                 = "%s"
  login                = true
  password_wo          = "wopass2"
  password_wo_version  = "2"
}`, roleName)

	resource.Test(t, resource.TestCase{
		PreCheck:     func() { testAccPreCheck(t) },
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleDestroy,
		Steps: []resource.TestStep{
			{
				Config: configWOv1,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password_wo_version", "1"),
					testAccCheckRoleCanLogin(t, roleName, "wopass1"),
				),
			},
			{
				// Update the password but keep the version the same → should still login with old password
				Config: configWOv1NewPass,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleCanLogin(t, roleName, "wopass1"),
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password_wo_version", "1"),
				),
			},
			{
				// Now bump the version → new password should apply
				Config: configWOv2,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckRoleCanLogin(t, roleName, "wopass2"),
					resource.TestCheckResourceAttr("postgresql_role.switch_role", "password_wo_version", "2"),
				),
			},
		},
	})
}
