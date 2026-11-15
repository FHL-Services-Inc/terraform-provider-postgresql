package postgresql

import (
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// grantableDirectlyQuery returns the query telling whether the current user
// holds every privilege of the grant WITH GRANT OPTION on every target object
// (and USAGE on the schema of schema objects), i.e. whether it can run the
// GRANT and REVOKE itself without being a member of the objects' owners.
// ok is false for object types the check does not cover.
func grantableDirectlyQuery(db *DBConnection, d *schema.ResourceData) (query string, args []any, ok bool) {
	objectType := d.Get("object_type").(string)

	var privileges []string
	for _, p := range d.Get("privileges").(*schema.Set).List() {
		privilege := strings.ToUpper(p.(string))
		if privilege == "ALL" {
			for _, a := range allowedPrivilegesForObjectType(objectType, db) {
				if a != "ALL" {
					privileges = append(privileges, a+" WITH GRANT OPTION")
				}
			}
			continue
		}
		privileges = append(privileges, privilege+" WITH GRANT OPTION")
	}

	switch objectType {
	case "database":
		return `SELECT coalesce(bool_and(has_database_privilege($1, p)), true) FROM unnest($2::text[]) p`,
			[]any{d.Get("database").(string), pq.Array(privileges)}, true
	case "schema":
		return `SELECT coalesce(bool_and(has_schema_privilege($1, p)), true) FROM unnest($2::text[]) p`,
			[]any{d.Get("schema").(string), pq.Array(privileges)}, true
	}

	var catalog, namespace, kindColumn, nameColumn, check string
	var kinds []string
	switch objectType {
	case "table":
		catalog, namespace, kindColumn, nameColumn = "pg_class", "relnamespace", "relkind", "relname"
		kinds, check = ownerRelkinds[objectType], "has_table_privilege(o.oid, p)"
	case "sequence":
		catalog, namespace, kindColumn, nameColumn = "pg_class", "relnamespace", "relkind", "relname"
		kinds, check = ownerRelkinds[objectType], "has_sequence_privilege(o.oid, p)"
	case "column":
		catalog, namespace, kindColumn, nameColumn = "pg_class", "relnamespace", "relkind", "relname"
		kinds, check = ownerRelkinds[objectType], "has_column_privilege(o.oid, col, p)"
	case "function", "procedure", "routine":
		catalog, namespace, kindColumn, nameColumn = "pg_proc", "pronamespace", "prokind", "proname"
		kinds, check = ownerProkinds[objectType], "has_function_privilege(o.oid, p)"
	default:
		return "", nil, false
	}

	args = []any{d.Get("schema").(string), pq.Array(privileges), pq.Array(kinds)}
	var filters, joins string
	if objects := d.Get("objects").(*schema.Set).List(); len(objects) > 0 {
		names := make([]string, 0, len(objects))
		for _, object := range objects {
			name := object.(string)
			// Functions are matched by name, every overload is checked.
			if i := strings.Index(name, "("); i >= 0 {
				name = strings.TrimSpace(name[:i])
			}
			names = append(names, name)
		}
		filters = fmt.Sprintf(" AND o.%s = ANY($4)", nameColumn)
		args = append(args, pq.Array(names))
	}
	if objectType == "column" {
		var columns []string
		for _, c := range d.Get("columns").(*schema.Set).List() {
			columns = append(columns, c.(string))
		}
		joins = fmt.Sprintf(" CROSS JOIN unnest($%d::text[]) col", len(args)+1)
		args = append(args, pq.Array(columns))
	}

	query = fmt.Sprintf(`
SELECT has_schema_privilege($1, 'USAGE') AND coalesce((
  SELECT bool_and(%s)
  FROM %s o
  JOIN pg_namespace n ON n.oid = o.%s
  CROSS JOIN unnest($2::text[]) p%s
  WHERE n.nspname = $1 AND o.%s = ANY($3)%s
), true)`, check, catalog, namespace, joins, kindColumn, filters)
	return query, args, true
}

// grantableDirectly tells whether the current user can run the GRANT and
// REVOKE of d itself (see grantableDirectlyQuery). It is the directGrant of
// withRolesGrantedOrDirect for postgresql_grant.
func grantableDirectly(db *DBConnection, txn QueryAble, d *schema.ResourceData) (bool, error) {
	query, args, ok := grantableDirectlyQuery(db, d)
	if !ok {
		return false, nil
	}
	var grantable bool
	if err := txn.QueryRow(query, args...).Scan(&grantable); err != nil {
		return false, fmt.Errorf("could not check the grant options of the current user: %w", err)
	}
	return grantable, nil
}
