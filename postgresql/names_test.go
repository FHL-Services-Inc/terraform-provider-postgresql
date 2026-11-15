package postgresql

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/lib/pq"
)

// testUniqueName returns name with a random suffix, e.g. "role_simple_k3x9qz".
//
// Acceptance tests run against a shared server and a test whose teardown is
// skipped (failure, interrupted run, dropped connection) leaves its objects
// behind. Cluster-wide objects (roles, databases, replication slots) and
// objects created in the postgres database get a unique name so such
// leftovers can't make later tests fail with "already exists".
func testUniqueName(name string) string {
	return name + "_" + acctest.RandString(6)
}

// testDropExtensionsIfExist drops the extensions (CASCADE) from the postgres
// database if they exist. Extension names can't be made unique and the
// resource creates them with IF NOT EXISTS, so one left behind by an earlier
// run would be adopted as is (in another schema, say), and objects left behind
// that depend on it would make its destroy fail.
func testDropExtensionsIfExist(t *testing.T, names ...string) {
	t.Helper()
	config := getTestConfig(t)
	for _, name := range names {
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("DROP EXTENSION IF EXISTS %s CASCADE", pq.QuoteIdentifier(name)))
	}
}
