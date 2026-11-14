package postgresql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/lib/pq"
)

// Object names and patterns containing ', \, % and _ used by the
// *_SpecialCharPatterns acceptance tests below.
const specialCharSchema = `we'ird\schema`

func specialCharNames(suffix string) []string {
	return []string{`it's\` + suffix, "pct%" + suffix, "under_" + suffix, "underX" + suffix}
}

func quotedQualifiedNames(schemaName string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, pq.QuoteIdentifier(schemaName)+"."+pq.QuoteIdentifier(n))
	}
	return out
}

func hclStringList(values ...string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// specialCharPatternBlocks returns one data source block per pattern case.
// extra is added to every block (e.g. a schemas filter). Expected matches for
// objects named specialCharNames(suffix):
//
//	quote_like      like_any ["it's%"]                    -> it's\<s>
//	backslash_like  like_all ["%\\<s>"]                   -> it's\<s>
//	percent_like    like_any ["pct\%<s>"]                 -> pct%<s>
//	underscore_like like_any ["under\_<s>"]               -> under_<s>
//	wildcard_like   like_any ["under_<s>"]                -> under_<s>, underX<s>
//	not_like        not_like_all ["%'%", "%\%%", "under\_%"] -> underX<s>
//	quote_regex     regex "^it's\\"                       -> it's\<s>
func specialCharPatternBlocks(dsType, dbName, suffix, extra string) string {
	cases := []struct{ name, attr, value string }{
		{"quote_like", "like_any_patterns", hclStringList("it's%")},
		{"backslash_like", "like_all_patterns", hclStringList(`%\\` + suffix)},
		{"percent_like", "like_any_patterns", hclStringList(`pct\%` + suffix)},
		{"underscore_like", "like_any_patterns", hclStringList(`under\_` + suffix)},
		{"wildcard_like", "like_any_patterns", hclStringList("under_" + suffix)},
		{"not_like", "not_like_all_patterns", hclStringList("%'%", `%\%%`, `under\_%`)},
		{"quote_regex", "regex_pattern", fmt.Sprintf("%q", `^it's\\`)},
	}
	var b strings.Builder
	for _, c := range cases {
		fmt.Fprintf(&b, `
data %q %q {
	database = %q
	%s
	%s = %s
}
`, dsType, c.name, dbName, extra, c.attr, c.value)
	}
	return b.String()
}

func TestAccPostgresqlDataSourceTables_SpecialCharPatterns(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	createTestSchemas(t, dbSuffix, []string{pq.QuoteIdentifier(specialCharSchema)}, "")
	createTestTables(t, dbSuffix, quotedQualifiedNames(specialCharSchema, specialCharNames("tab")), "")
	dbName, _ := getTestDBNames(dbSuffix)

	extra := fmt.Sprintf("schemas = %s\n\ttable_types = [\"BASE TABLE\"]", hclStringList(specialCharSchema))
	ds := "data.postgresql_tables."
	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: specialCharPatternBlocks("postgresql_tables", dbName, "tab", extra),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(ds+"quote_like", "tables.#", "1"),
					resource.TestCheckResourceAttr(ds+"quote_like", "tables.0.object_name", `it's\tab`),
					resource.TestCheckResourceAttr(ds+"quote_like", "tables.0.schema_name", specialCharSchema),
					resource.TestCheckResourceAttr(ds+"backslash_like", "tables.#", "1"),
					resource.TestCheckResourceAttr(ds+"backslash_like", "tables.0.object_name", `it's\tab`),
					resource.TestCheckResourceAttr(ds+"percent_like", "tables.#", "1"),
					resource.TestCheckResourceAttr(ds+"percent_like", "tables.0.object_name", "pct%tab"),
					resource.TestCheckResourceAttr(ds+"underscore_like", "tables.#", "1"),
					resource.TestCheckResourceAttr(ds+"underscore_like", "tables.0.object_name", "under_tab"),
					resource.TestCheckResourceAttr(ds+"wildcard_like", "tables.#", "2"),
					resource.TestCheckResourceAttr(ds+"not_like", "tables.#", "1"),
					resource.TestCheckResourceAttr(ds+"not_like", "tables.0.object_name", "underXtab"),
					resource.TestCheckResourceAttr(ds+"quote_regex", "tables.#", "1"),
					resource.TestCheckResourceAttr(ds+"quote_regex", "tables.0.object_name", `it's\tab`),
				),
			},
		},
	})
}

func TestAccPostgresqlDataSourceSequences_SpecialCharPatterns(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	createTestSchemas(t, dbSuffix, []string{pq.QuoteIdentifier(specialCharSchema)}, "")
	createTestSequences(t, dbSuffix, quotedQualifiedNames(specialCharSchema, specialCharNames("seq")), "")
	dbName, _ := getTestDBNames(dbSuffix)

	extra := fmt.Sprintf("schemas = %s", hclStringList(specialCharSchema))
	ds := "data.postgresql_sequences."
	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: specialCharPatternBlocks("postgresql_sequences", dbName, "seq", extra),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(ds+"quote_like", "sequences.#", "1"),
					resource.TestCheckResourceAttr(ds+"quote_like", "sequences.0.object_name", `it's\seq`),
					resource.TestCheckResourceAttr(ds+"quote_like", "sequences.0.schema_name", specialCharSchema),
					resource.TestCheckResourceAttr(ds+"backslash_like", "sequences.#", "1"),
					resource.TestCheckResourceAttr(ds+"backslash_like", "sequences.0.object_name", `it's\seq`),
					resource.TestCheckResourceAttr(ds+"percent_like", "sequences.#", "1"),
					resource.TestCheckResourceAttr(ds+"percent_like", "sequences.0.object_name", "pct%seq"),
					resource.TestCheckResourceAttr(ds+"underscore_like", "sequences.#", "1"),
					resource.TestCheckResourceAttr(ds+"underscore_like", "sequences.0.object_name", "under_seq"),
					resource.TestCheckResourceAttr(ds+"wildcard_like", "sequences.#", "2"),
					resource.TestCheckResourceAttr(ds+"not_like", "sequences.#", "1"),
					resource.TestCheckResourceAttr(ds+"not_like", "sequences.0.object_name", "underXseq"),
					resource.TestCheckResourceAttr(ds+"quote_regex", "sequences.#", "1"),
					resource.TestCheckResourceAttr(ds+"quote_regex", "sequences.0.object_name", `it's\seq`),
				),
			},
		},
	})
}

func TestAccPostgresqlDataSourceSchemas_SpecialCharPatterns(t *testing.T) {
	skipIfNotAcc(t)

	dbSuffix, teardown := setupTestDatabase(t, true, true)
	defer teardown()
	names := specialCharNames("sch")
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, pq.QuoteIdentifier(n))
	}
	createTestSchemas(t, dbSuffix, quoted, "")
	dbName, _ := getTestDBNames(dbSuffix)

	// not_like would also match public, test_schema, dev_schema: restrict it
	// to the *sch schemas with an additional like_all pattern.
	config := specialCharPatternBlocks("postgresql_schemas", dbName, "sch", "")
	config = strings.Replace(config,
		`not_like_all_patterns = ["%'%", "%\\%%", "under\\_%"]`,
		`not_like_all_patterns = ["%'%", "%\\%%", "under\\_%"]
	like_all_patterns = ["%sch"]`, 1)

	ds := "data.postgresql_schemas."
	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(ds+"quote_like", "schemas.#", "1"),
					resource.TestCheckTypeSetElemAttr(ds+"quote_like", "schemas.*", `it's\sch`),
					resource.TestCheckResourceAttr(ds+"backslash_like", "schemas.#", "1"),
					resource.TestCheckTypeSetElemAttr(ds+"backslash_like", "schemas.*", `it's\sch`),
					resource.TestCheckResourceAttr(ds+"percent_like", "schemas.#", "1"),
					resource.TestCheckTypeSetElemAttr(ds+"percent_like", "schemas.*", "pct%sch"),
					resource.TestCheckResourceAttr(ds+"underscore_like", "schemas.#", "1"),
					resource.TestCheckTypeSetElemAttr(ds+"underscore_like", "schemas.*", "under_sch"),
					resource.TestCheckResourceAttr(ds+"wildcard_like", "schemas.#", "2"),
					resource.TestCheckResourceAttr(ds+"not_like", "schemas.#", "1"),
					resource.TestCheckTypeSetElemAttr(ds+"not_like", "schemas.*", "underXsch"),
					resource.TestCheckResourceAttr(ds+"quote_regex", "schemas.#", "1"),
					resource.TestCheckTypeSetElemAttr(ds+"quote_regex", "schemas.*", `it's\sch`),
				),
			},
		},
	})
}
