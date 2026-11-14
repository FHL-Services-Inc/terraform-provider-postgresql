package postgresql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

func TestApplyTableDataSourceQueryFiltersBindsUserInput(t *testing.T) {
	d := schema.TestResourceDataRaw(t, dataSourcePostgreSQLDatabaseTables().Schema, map[string]any{
		"database":              "db",
		"schemas":               []any{`we'ird\schema`, "public"},
		"table_types":           []any{"BASE TABLE"},
		"like_any_patterns":     []any{"it's%", `pct\%x`},
		"like_all_patterns":     []any{`%\\x`},
		"not_like_all_patterns": []any{"%'; DROP TABLE x; --"},
		"regex_pattern":         `^it's\\`,
	})

	query, args := applyTableDataSourceQueryFilters(tableQuery, queryConcatKeywordWhere, d)

	wantQuery := tableQuery + " WHERE " + strings.Join([]string{
		"table_schema = ANY ($1::text[])",
		"table_type = ANY ($2::text[])",
		"table_name LIKE ANY ($3::text[])",
		"table_name LIKE ALL ($4::text[])",
		"table_name NOT LIKE ALL ($5::text[])",
		"table_name ~ $6",
	}, " AND ")
	if query != wantQuery {
		t.Errorf("query:\n got: %q\nwant: %q", query, wantQuery)
	}

	wantArgs := []any{
		pq.Array([]string{`we'ird\schema`, "public"}),
		pq.Array([]string{"BASE TABLE"}),
		pq.Array([]string{"it's%", `pct\%x`}),
		pq.Array([]string{`%\\x`}),
		pq.Array([]string{"%'; DROP TABLE x; --"}),
		`^it's\\`,
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args:\n got: %#v\nwant: %#v", args, wantArgs)
	}
}

func TestApplySchemaDataSourceQueryFiltersWithoutFilters(t *testing.T) {
	d := schema.TestResourceDataRaw(t, dataSourcePostgreSQLDatabaseSchemas().Schema, map[string]any{
		"database": "db",
	})

	base := schemaQueries["query_exclude_system_schemas"]
	query, args := applySchemaDataSourceQueryFilters(base, queryConcatKeywordAnd, d)
	if query != base {
		t.Errorf("query changed without filters: %q", query)
	}
	if len(args) != 0 {
		t.Errorf("unexpected args: %#v", args)
	}
}

func TestApplySequenceDataSourceQueryFiltersBindsUserInput(t *testing.T) {
	d := schema.TestResourceDataRaw(t, dataSourcePostgreSQLDatabaseSequences().Schema, map[string]any{
		"database":      "db",
		"schemas":       []any{`we'ird\schema`},
		"regex_pattern": "^it's",
	})

	query, args := applySequenceDataSourceQueryFilters(sequenceQuery, queryConcatKeywordWhere, d)

	wantQuery := sequenceQuery + " WHERE sequence_schema = ANY ($1::text[]) AND sequence_name ~ $2"
	if query != wantQuery {
		t.Errorf("query:\n got: %q\nwant: %q", query, wantQuery)
	}
	wantArgs := []any{pq.Array([]string{`we'ird\schema`}), "^it's"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args:\n got: %#v\nwant: %#v", args, wantArgs)
	}
}
