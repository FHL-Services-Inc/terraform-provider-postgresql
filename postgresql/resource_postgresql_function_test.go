package postgresql

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccPostgresqlFunction_Basic(t *testing.T) {
	funcName := testUniqueName("basic_function")
	config := fmt.Sprintf(`
resource "postgresql_function" "basic_function" {
    name = "%s"
    returns = "integer"
    language = "plpgsql"
    body = <<-EOF
        BEGIN
            RETURN 1;
        END;
    EOF
}
`, funcName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.basic_function", ""),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "name", funcName),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "schema", "public"),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "language", "plpgsql"),
				),
			},
		},
	})
}

func TestAccPostgresqlFunction_SpecificDatabase(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()

	dbName, _ := getTestDBNames(dbSuffix)

	config := `
resource "postgresql_function" "basic_function" {
    name = "basic_function"
    database = "%s"
    returns = "integer"
    language = "plpgsql"
    body = <<-EOF
        BEGIN
            RETURN 1;
        END;
    EOF
}
`

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(config, dbName),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.basic_function", dbName),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "name", "basic_function"),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "database", dbName),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "schema", "public"),
					resource.TestCheckResourceAttr(
						"postgresql_function.basic_function", "language", "plpgsql"),
				),
			},
		},
	})
}

func TestAccPostgresqlFunction_MultipleArgs(t *testing.T) {
	schemaName := testUniqueName("test")
	funcName := testUniqueName("increment")
	config := fmt.Sprintf(`
resource "postgresql_schema" "test" {
    name = "%[1]s"
}

resource "postgresql_function" "increment" {
    schema = postgresql_schema.test.name
    name = "%[2]s"
    arg {
        name = "i"
        type = "integer"
        default = "7"
    }
    arg {
        name = "result"
        type = "integer"
        mode = "OUT"
    }
    language = "plpgsql"
    parallel = "RESTRICTED"
    strict = true
    security_definer = true
    volatility = "STABLE"
    body = <<-EOF
        BEGIN
            result = i + 1;
        END;
    EOF
}
`, schemaName, funcName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.increment", ""),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "name", funcName),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "schema", schemaName),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "language", "plpgsql"),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "strict", "true"),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "parallel", "RESTRICTED"),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "security_definer", "true"),
					resource.TestCheckResourceAttr(
						"postgresql_function.increment", "volatility", "STABLE"),
				),
			},
		},
	})
}

func TestAccPostgresqlFunction_Update(t *testing.T) {
	funcName := testUniqueName("func")
	configCreate := fmt.Sprintf(`
resource "postgresql_function" "func" {
    name = "%s"
    returns = "integer"
    language = "plpgsql"
    body = <<-EOF
        BEGIN
            RETURN 1;
        END;
    EOF
}
`, funcName)

	configUpdate := fmt.Sprintf(`
resource "postgresql_function" "func" {
    name = "%s"
    returns = "integer"
    language = "plpgsql"
    volatility = "IMMUTABLE"
    body = <<-EOF
        BEGIN
            RETURN 2;
        END;
    EOF
}
`, funcName)
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: configCreate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.func", ""),
					resource.TestCheckResourceAttr(
						"postgresql_function.func", "name", funcName),
					resource.TestCheckResourceAttr(
						"postgresql_function.func", "schema", "public"),
					resource.TestCheckResourceAttr(
						"postgresql_function.func", "volatility", "VOLATILE"),
				),
			},
			{
				Config: configUpdate,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.func", ""),
					resource.TestCheckResourceAttr(
						"postgresql_function.func", "name", funcName),
					resource.TestCheckResourceAttr(
						"postgresql_function.func", "schema", "public"),
					resource.TestCheckResourceAttr(
						"postgresql_function.func", "volatility", "IMMUTABLE"),
				),
			},
		},
	})
}

// An argument default with a comma, and a type modifier with a comma, read
// back without a diff (#126).
func TestAccPostgresqlFunction_ArgDefaultWithComma(t *testing.T) {
	funcName := testUniqueName("comma_default")
	config := fmt.Sprintf(`
resource "postgresql_function" "f" {
    name = "%s"
    arg {
        name    = "a"
        type    = "text[]"
        default = "ARRAY['a'::text, 'b'::text]"
    }
    arg {
        name    = "b"
        type    = "numeric(10,2)"
        default = "1.5"
    }
    arg {
        name    = "c"
        type    = "text"
        default = "'x, y'::text"
    }
    returns  = "integer"
    language = "sql"
    body     = "SELECT 1"
}
`, funcName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.f", ""),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.#", "3"),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.0.default", "ARRAY['a'::text, 'b'::text]"),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.1.type", "numeric(10,2)"),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.1.default", "1.5"),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.2.default", "'x, y'::text"),
				),
			},
		},
	})
}

// Schema-qualified argument types put extra dots into the resource ID (#127).
func TestAccPostgresqlFunction_QualifiedArgType(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	dbName, _ := getTestDBNames(dbSuffix)

	// A type outside the search_path, like an extension type (citext) in
	// its own schema.
	testConfig := getTestConfig(t)
	dsn := testConfig.connStr(dbName)
	dbExecute(t, dsn, "CREATE SCHEMA fn_types")
	dbExecute(t, dsn, "CREATE DOMAIN fn_types.dom AS text")

	config := fmt.Sprintf(`
resource "postgresql_function" "f" {
    database = "%s"
    name     = "qualified_args"
    arg {
        name = "x"
        type = "fn_types.dom"
    }
    arg {
        name = "y"
        type = "pg_catalog.text"
    }
    returns  = "integer"
    language = "sql"
    body     = "SELECT 1"
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.f", dbName),
					resource.TestCheckResourceAttr("postgresql_function.f", "id",
						dbName+".public.qualified_args(fn_types.dom,pg_catalog.text)"),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.0.type", "fn_types.dom"),
					resource.TestCheckResourceAttr("postgresql_function.f", "arg.1.type", "pg_catalog.text"),
				),
			},
		},
	})
}

// A dot in the function name puts an extra dot into the resource ID (#127).
func TestAccPostgresqlFunction_DotInName(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	dbName, _ := getTestDBNames(dbSuffix)

	config := fmt.Sprintf(`
resource "postgresql_function" "f" {
    database = "%s"
    name     = "dotted.name"
    arg {
        name = "x"
        type = "integer"
    }
    returns  = "integer"
    language = "sql"
    body     = "SELECT x"
}
`, dbName)

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testCheckCompatibleVersion(t, featureFunction)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlFunctionDestroy,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlFunctionExists("postgresql_function.f", dbName),
					resource.TestCheckResourceAttr("postgresql_function.f", "name", "dotted.name"),
					resource.TestCheckResourceAttr("postgresql_function.f", "schema", "public"),
				),
			},
			{
				ResourceName:            "postgresql_function.f",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"drop_cascade"},
			},
		},
	})
}

func testAccCheckPostgresqlFunctionExists(n string, database string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Resource not found: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("No ID is set")
		}

		_, signature, err := expandFunctionID(rs.Primary.ID, nil, nil)
		if err != nil {
			return err
		}

		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, database)
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		exists, err := checkFunctionExists(txn, signature)

		if err != nil {
			return fmt.Errorf("error checking function %s", err)
		}

		if !exists {
			return fmt.Errorf("Function not found")
		}

		return nil
	}
}

func testAccCheckPostgresqlFunctionDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_function" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		_, functionSignature, expandErr := expandFunctionID(rs.Primary.ID, nil, nil)

		if expandErr != nil {
			return fmt.Errorf("Incorrect resource Id %s", expandErr)
		}

		exists, err := checkFunctionExists(txn, functionSignature)

		if err != nil {
			return fmt.Errorf("error checking function %s", err)
		}

		if exists {
			return fmt.Errorf("Function still exists after destroy")
		}
	}

	return nil
}

func checkFunctionExists(txn *sql.Tx, signature string) (bool, error) {
	var _rez bool
	err := txn.QueryRow("SELECT to_regprocedure($1) IS NOT NULL", signature).Scan(&_rez)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error reading info about function: %s", err)
	}

	return _rez, nil
}
