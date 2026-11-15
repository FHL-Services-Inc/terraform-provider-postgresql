package postgresql

import (
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

const (
	// tableQuery lists relations from pg_class with the same columns and
	// table_type values as information_schema.tables, but without its
	// privilege filter: information_schema only shows tables the current
	// user owns (or is a member of the owner) or has a privilege on, so a
	// non-superuser provider silently missed tables owned by other roles.
	// It is a subquery so the filters below can use the column names.
	tableQuery = `
	SELECT table_name, table_schema, table_type
	FROM (
		SELECT c.relname AS table_name,
			n.nspname AS table_schema,
			CASE
				WHEN n.oid = pg_my_temp_schema() THEN 'LOCAL TEMPORARY'
				WHEN c.relkind IN ('r', 'p') THEN 'BASE TABLE'
				WHEN c.relkind = 'v' THEN 'VIEW'
				WHEN c.relkind = 'f' THEN 'FOREIGN'
			END AS table_type
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'v', 'f', 'p')
			AND NOT pg_is_other_temp_schema(n.oid)
	) AS tables
	`
	tableQueryOrder            = "ORDER BY table_schema, table_name"
	tablePatternMatchingTarget = "table_name"
	tableSchemaKeyword         = "table_schema"
	tableTypeKeyword           = "table_type"
)

func dataSourcePostgreSQLDatabaseTables() *schema.Resource {
	return &schema.Resource{
		Read: PGResourceFunc(dataSourcePostgreSQLTablesRead),
		Schema: map[string]*schema.Schema{
			"database": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The PostgreSQL database which will be queried for table names",
			},
			"schemas": {
				Type:        schema.TypeList,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				MinItems:    0,
				Description: "The PostgreSQL schema(s) which will be queried for table names. Queries all schemas in the database by default",
			},
			"table_types": {
				Type:        schema.TypeList,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				MinItems:    0,
				Description: "The PostgreSQL table types which will be queried for table names. Includes all table types by default. Use 'BASE TABLE' for normal tables only",
			},
			"like_any_patterns": {
				Type:        schema.TypeList,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				MinItems:    0,
				Description: "Expression(s) which will be pattern matched against table names in the query using the PostgreSQL LIKE ANY operator",
			},
			"like_all_patterns": {
				Type:        schema.TypeList,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				MinItems:    0,
				Description: "Expression(s) which will be pattern matched against table names in the query using the PostgreSQL LIKE ALL operator",
			},
			"not_like_all_patterns": {
				Type:        schema.TypeList,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				MinItems:    0,
				Description: "Expression(s) which will be pattern matched against table names in the query using the PostgreSQL NOT LIKE ALL operator",
			},
			"regex_pattern": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Expression which will be pattern matched against table names in the query using the PostgreSQL ~ (regular expression match) operator",
			},
			"tables": {
				Type:     schema.TypeList,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"object_name": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"schema_name": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"table_type": {
							Type:     schema.TypeString,
							Computed: true,
						},
					},
				},
				Description: "The list of PostgreSQL tables retrieved by this data source. Note that this returns a set, so duplicate table names across different schemas will be consolidated.",
			},
		},
	}
}

func dataSourcePostgreSQLTablesRead(db *DBConnection, d *schema.ResourceData) error {
	database := d.Get("database").(string)

	dbc, err := initConnection(db.client, database)
	if err != nil {
		return err
	}

	query := tableQuery
	queryConcatKeyword := queryConcatKeywordWhere

	query, args := applyTableDataSourceQueryFilters(query, queryConcatKeyword, d)
	query += " " + tableQueryOrder

	rows, err := dbc.Query(query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("error closing rows: %v\n", err)
		}
	}()

	tables := make([]any, 0)
	for rows.Next() {
		var object_name string
		var schema_name string
		var table_type string

		if err = rows.Scan(&object_name, &schema_name, &table_type); err != nil {
			return fmt.Errorf("could not scan table output for database: %w", err)
		}

		result := make(map[string]any)
		result["object_name"] = object_name
		result["schema_name"] = schema_name
		result["table_type"] = table_type
		tables = append(tables, result)
	}

	d.Set("tables", tables)
	d.SetId(generateDataSourceTablesID(d, database))

	return nil
}

func generateDataSourceTablesID(d *schema.ResourceData, databaseName string) string {
	return strings.Join([]string{
		databaseName,
		generatePatternArrayString(d.Get("schemas").([]any), queryArrayKeywordAny),
		generatePatternArrayString(d.Get("table_types").([]any), queryArrayKeywordAny),
		generatePatternArrayString(d.Get("like_any_patterns").([]any), queryArrayKeywordAny),
		generatePatternArrayString(d.Get("like_all_patterns").([]any), queryArrayKeywordAll),
		generatePatternArrayString(d.Get("not_like_all_patterns").([]any), queryArrayKeywordAll),
		d.Get("regex_pattern").(string),
	}, "_")
}

func applyTableDataSourceQueryFilters(query string, queryConcatKeyword string, d *schema.ResourceData) (string, []any) {
	var args queryArgs
	filters := []string{}
	schemasTypeFilter := applyTypeMatchingToQuery(tableSchemaKeyword, d.Get("schemas").([]any), &args)
	if len(schemasTypeFilter) > 0 {
		filters = append(filters, schemasTypeFilter)
	}
	tableTypeFilter := applyTypeMatchingToQuery(tableTypeKeyword, d.Get("table_types").([]any), &args)
	if len(tableTypeFilter) > 0 {
		filters = append(filters, tableTypeFilter)
	}
	filters = append(filters, applyPatternMatchingToQuery(tablePatternMatchingTarget, d, &args)...)

	return finalizeQueryWithFilters(query, queryConcatKeyword, filters), args
}
