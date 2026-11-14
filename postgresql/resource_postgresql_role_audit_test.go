package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func TestReadRoleAuditLogClasses(t *testing.T) {
	tests := []struct {
		name   string
		config pq.ByteaArray
		want   string
	}{
		{"empty", nil, ""},
		{"other settings only", pq.ByteaArray{[]byte("statement_timeout=1000"), []byte("search_path=app")}, ""},
		{"single class", pq.ByteaArray{[]byte("pgaudit.log=all")}, "all"},
		{"list among other settings", pq.ByteaArray{[]byte("statement_timeout=1000"), []byte("pgaudit.log=read, write"), []byte("search_path=app")}, "read, write"},
		{"similar prefix is ignored", pq.ByteaArray{[]byte("pgaudit.log_level=notice")}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readRoleAuditLogClasses(tt.config); got != tt.want {
				t.Errorf("readRoleAuditLogClasses() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccPostgresqlRoleAudit_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleAuditDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlRoleAuditConfig("all"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleAuditValue("role_audit_test-role", "all"),
					resource.TestCheckResourceAttr("postgresql_role_audit.test", "id", "role_audit_test-role"),
					resource.TestCheckResourceAttr("postgresql_role_audit.test", "role_name", "role_audit_test-role"),
					resource.TestCheckResourceAttr("postgresql_role_audit.test", "log_classes", "all"),
					testAccCheckPostgresqlRoleSetting("role_audit_test-role", "statement_timeout", "1000"),
				),
			},
			{
				Config: testAccPostgresqlRoleAuditConfig("read, write"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleAuditValue("role_audit_test-role", "read, write"),
					resource.TestCheckResourceAttr("postgresql_role_audit.test", "log_classes", "read, write"),
					testAccCheckPostgresqlRoleSetting("role_audit_test-role", "statement_timeout", "1000"),
				),
			},
			{
				ResourceName:      "postgresql_role_audit.test",
				ImportState:       true,
				ImportStateId:     "role_audit_test-role",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccPostgresqlRoleAudit_DriftIsRestored(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlRoleAuditDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlRoleAuditConfig("ddl"),
				Check:  testAccCheckPostgresqlRoleAuditValue("role_audit_test-role", "ddl"),
			},
			{
				PreConfig: func() {
					testAccExecSQL(t, `ALTER ROLE "role_audit_test-role" RESET pgaudit.log`)
				},
				Config: testAccPostgresqlRoleAuditConfig("ddl"),
				Check:  testAccCheckPostgresqlRoleAuditValue("role_audit_test-role", "ddl"),
			},
		},
	})
}

func TestAccPostgresqlRoleAudit_RoleDroppedOutsideTerraform(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					testAccCreateExternalRole(t, "role_audit_external")
				},
				Config: testAccPostgresqlRoleAuditExternalConfig,
				Check:  testAccCheckPostgresqlRoleAuditValue("role_audit_external", "all"),
			},
			{
				PreConfig: func() {
					testAccExecSQL(t, `DROP ROLE "role_audit_external"`)
				},
				Config:             testAccPostgresqlRoleAuditExternalConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccPostgresqlRoleAudit_DestroyResetsSetting(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers: testAccProviders,
		CheckDestroy: resource.ComposeTestCheckFunc(
			testAccCheckPostgresqlRoleExists("role_audit_kept", nil, nil),
			testAccCheckPostgresqlRoleAuditValue("role_audit_kept", ""),
			testAccCheckPostgresqlRoleSetting("role_audit_kept", "statement_timeout", "1000"),
		),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					testAccCreateExternalRole(t, "role_audit_kept")
					testAccExecSQL(t, `ALTER ROLE "role_audit_kept" SET statement_timeout TO 1000`)
				},
				Config: testAccPostgresqlRoleAuditKeptConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlRoleAuditValue("role_audit_kept", "ddl"),
					testAccCheckPostgresqlRoleSetting("role_audit_kept", "statement_timeout", "1000"),
				),
			},
		},
	})
}

func testAccCreateExternalRole(t *testing.T, role string) {
	t.Helper()
	testAccExecSQL(t, fmt.Sprintf("CREATE ROLE %s", pq.QuoteIdentifier(role)))
	t.Cleanup(func() {
		testAccExecSQL(t, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(role)))
	})
}

func testAccExecSQL(t *testing.T, query string) {
	t.Helper()
	config := getTestConfig(t)
	db, err := sql.Open("postgres", config.connStr("postgres"))
	if err != nil {
		t.Fatalf("could not open connection: %v", err)
	}
	defer closeDB(t, db)
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("could not execute %q: %v", query, err)
	}
}

func testAccCheckPostgresqlRoleAuditValue(role, want string) resource.TestCheckFunc {
	return testAccCheckPostgresqlRoleSetting(role, pgauditLogParameter, want)
}

func testAccCheckPostgresqlRoleSetting(role, parameter, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		got, err := readTestRoleSetting(role, parameter)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s for role %s = %q, want %q", parameter, role, got, want)
		}
		return nil
	}
}

func readTestRoleSetting(role, parameter string) (string, error) {
	client := testAccProvider.Meta().(*Client)
	db, err := client.Connect()
	if err != nil {
		return "", err
	}

	var value sql.NullString
	err = db.QueryRow(
		"SELECT split_part(s, '=', 2) FROM pg_catalog.pg_roles, unnest(rolconfig) s WHERE rolname = $1 AND split_part(s, '=', 1) = $2",
		role, parameter,
	).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("could not read %s for role %s: %w", parameter, role, err)
	}
	return value.String, nil
}

func testAccCheckPostgresqlRoleAuditDestroy(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_role_audit" {
			continue
		}

		got, err := readTestRoleSetting(rs.Primary.ID, pgauditLogParameter)
		if err != nil {
			return err
		}
		if got != "" {
			return fmt.Errorf("pgaudit.log for role %s still set to %q after destroy", rs.Primary.ID, got)
		}
	}
	return nil
}

func testAccPostgresqlRoleAuditConfig(value string) string {
	return fmt.Sprintf(`
resource "postgresql_role" "test" {
  name              = "role_audit_test-role"
  statement_timeout = 1000
}

resource "postgresql_role_audit" "test" {
  role_name   = postgresql_role.test.name
  log_classes = %q
}
`, value)
}

const testAccPostgresqlRoleAuditExternalConfig = `
resource "postgresql_role_audit" "external" {
  role_name   = "role_audit_external"
  log_classes = "all"
}
`

const testAccPostgresqlRoleAuditKeptConfig = `
resource "postgresql_role_audit" "kept" {
  role_name   = "role_audit_kept"
  log_classes = "ddl"
}
`
