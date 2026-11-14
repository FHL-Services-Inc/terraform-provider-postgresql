package postgresql

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

func TestAccPostgresqlSecurityLabel_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testSuperuserPreCheck(t)
			testSecurityLabelProviderPreCheck(t, "dummy")
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSecurityLabelDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSecurityLabelConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_type", "role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test_role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label_provider", "dummy"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "secret"),
				),
			},
		},
	})
}

func TestAccPostgresqlSecurityLabel_Update(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureSecurityLabel)
			testSuperuserPreCheck(t)
			testSecurityLabelProviderPreCheck(t, "dummy")
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlSecurityLabelDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlSecurityLabelConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_type", "role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test_role"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label_provider", "dummy"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "secret"),
				),
			},
			{
				Config: testAccPostgresqlSecurityLabelChanges2,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "label", "top secret"),
				),
			},
			{
				Config: testAccPostgresqlSecurityLabelChanges3,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlSecurityLabelExists("postgresql_security_label.test_label"),
					resource.TestCheckResourceAttr(
						"postgresql_security_label.test_label", "object_name", "security_label_test-role2"),
				),
			},
		},
	})
}

// testSecurityLabelProviderPreCheck skips the test unless the label provider
// is loaded on the server. The tests use the dummy_seclabel module, which the
// image built from tests/build preloads but a stock postgres image doesn't
// ship. The probe sets no label on the current role (IS NULL) in a transaction
// that is rolled back, and fails with invalid_parameter_value (22023) when the
// provider isn't loaded.
func testSecurityLabelProviderPreCheck(t *testing.T, provider string) {
	client := testAccProvider.Meta().(*Client)
	db, err := client.Connect()
	if err != nil {
		t.Fatalf("could not connect to database: %v", err)
	}

	txn, err := db.Begin()
	if err != nil {
		t.Fatalf("could not start transaction: %v", err)
	}
	defer deferredRollback(txn)

	var role string
	if err := txn.QueryRow("SELECT current_user").Scan(&role); err != nil {
		t.Fatalf("could not read current user: %v", err)
	}

	_, err = txn.Exec(fmt.Sprintf(
		"SECURITY LABEL FOR %s ON ROLE %s IS NULL", pq.QuoteIdentifier(provider), pq.QuoteIdentifier(role),
	))
	var pqErr *pq.Error
	switch {
	case err == nil:
	case errors.As(err, &pqErr) && pqErr.Code == "22023":
		t.Skipf("Skip test: %s; run against an image built from tests/build", pqErr.Message)
	default:
		t.Fatalf("could not check security label provider %q: %v", provider, err)
	}
}

func checkSecurityLabelExists(txn *sql.Tx, objectType string, objectName string, provider string) (bool, error) {
	var _rez bool
	err := txn.QueryRow("SELECT TRUE FROM pg_seclabels WHERE objtype = $1 AND objname = $2 AND provider = $3", objectType, quoteIdentifier(objectName), provider).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about security label: %s", err)
	}

	return true, nil
}

func testAccCheckPostgresqlSecurityLabelDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_security_label" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		splitted := strings.Split(rs.Primary.ID, ".")
		exists, err := checkSecurityLabelExists(txn, splitted[1], splitted[2], splitted[0])

		if err != nil {
			return fmt.Errorf("error checking security label%s", err)
		}

		if exists {
			return fmt.Errorf("Security label still exists after destroy")
		}
	}

	return nil
}

func testAccCheckPostgresqlSecurityLabelExists(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		objectType, ok := rs.Primary.Attributes[securityLabelObjectTypeAttr]
		if !ok {
			return fmt.Errorf("No Attribute for object type is set")
		}

		objectName, ok := rs.Primary.Attributes[securityLabelObjectNameAttr]
		if !ok {
			return fmt.Errorf("No Attribute for object name is set")
		}

		provider, ok := rs.Primary.Attributes[securityLabelProviderAttr]
		if !ok {
			return fmt.Errorf("No Attribute for security provider is set")
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkSecurityLabelExists(txn, objectType, objectName, provider)

		if err != nil {
			return fmt.Errorf("error checking security label%s", err)
		}

		if !exists {
			return fmt.Errorf("Security label not found")
		}

		return nil
	}
}

var testAccPostgresqlSecurityLabelConfig = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "secret"
}
`

var testAccPostgresqlSecurityLabelChanges2 = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test_role"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "top secret"
}
`

var testAccPostgresqlSecurityLabelChanges3 = `
resource "postgresql_role" "test_role" {
  name            = "security_label_test-role2"
  login           = true
  create_database = true
}
resource "postgresql_security_label" "test_label" {
  object_type    = "role"
  object_name    = postgresql_role.test_role.name
  label_provider = "dummy"
  label          = "top secret"
}
`
