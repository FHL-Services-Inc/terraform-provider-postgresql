package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
)

var (
	defaultACLGuardOnce sync.Once
	defaultACLGuardErr  error
)

// testAccCheckNoDefaultACLs fails the test if the postgres database has
// default privileges (pg_default_acl entries). The acceptance tests expect the
// server defaults there, e.g. functions executable by PUBLIC. Older versions of
// the function grant tests left ALTER DEFAULT PRIVILEGES ... REVOKE ALL ON
// FUNCTIONS FROM PUBLIC behind, which then changed the outcome of later tests
// and runs depending on their order. The check runs once per test binary,
// from the first acceptance test's PreCheck.
func testAccCheckNoDefaultACLs(t *testing.T) {
	t.Helper()

	defaultACLGuardOnce.Do(func() {
		defaultACLGuardErr = queryDefaultACLs(getTestConfig(t))
	})
	if defaultACLGuardErr != nil {
		t.Fatal(defaultACLGuardErr)
	}
}

func queryDefaultACLs(config Config) error {
	db, err := sql.Open("postgres", config.connStr("postgres"))
	if err != nil {
		return fmt.Errorf("could not create connection pool: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`
SELECT pg_get_userbyid(d.defaclrole), coalesce(n.nspname, ''), d.defaclobjtype, d.defaclacl::text
  FROM pg_default_acl d
  LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace
 ORDER BY 1, 2, 3`)
	if err != nil {
		return fmt.Errorf("could not read pg_default_acl: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []string
	for rows.Next() {
		var role, schema, objType, acl string
		if err := rows.Scan(&role, &schema, &objType, &acl); err != nil {
			return fmt.Errorf("could not read pg_default_acl: %w", err)
		}
		entries = append(entries, fmt.Sprintf("role=%s schema=%q type=%s acl=%s", role, schema, objType, acl))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("could not read pg_default_acl: %w", err)
	}

	if len(entries) > 0 {
		return fmt.Errorf(
			"the postgres database has default privileges, the acceptance tests expect none "+
				"(left by an earlier run?). Reset them with ALTER DEFAULT PRIVILEGES or use a fresh server:\n  %s",
			strings.Join(entries, "\n  "),
		)
	}
	return nil
}
