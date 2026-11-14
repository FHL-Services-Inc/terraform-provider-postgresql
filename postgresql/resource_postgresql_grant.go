package postgresql

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	// Use Postgres as SQL driver
	"github.com/lib/pq"
)

var allowedObjectTypes = []string{
	"database",
	"function",
	"procedure",
	"routine",
	"schema",
	"sequence",
	"table",
	"foreign_data_wrapper",
	"foreign_server",
	"column",
}

var objectTypes = map[string]string{
	"table":    "r",
	"sequence": "S",
	"function": "f",
	"routine":  "f",
	"type":     "T",
	"schema":   "n",
}

type ResourceSchemeGetter func(string) any

func resourcePostgreSQLGrant() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceWriteFunc(resourcePostgreSQLGrantCreate),
		Update: PGResourceWriteFunc(resourcePostgreSQLGrantUpdate),
		Read:   PGResourceFunc(resourcePostgreSQLGrantRead),
		Delete: PGResourceWriteFunc(resourcePostgreSQLGrantDelete),

		Schema: map[string]*schema.Schema{
			"role": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The name of the role to grant privileges on",
			},
			"database": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The database to grant privileges on for this role",
			},
			"schema": {
				Type:        schema.TypeString,
				Optional:    true,
				ForceNew:    true,
				Description: "The database schema to grant privileges on for this role",
			},
			"object_type": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringInSlice(allowedObjectTypes, false),
				Description:  "The PostgreSQL object type to grant the privileges on (one of: " + strings.Join(allowedObjectTypes, ", ") + ")",
			},
			"objects": {
				Type:        schema.TypeSet,
				Optional:    true,
				ForceNew:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Set:         schema.HashString,
				Description: "The specific objects to grant privileges on for this role (empty means all objects of the requested type)",
			},
			"columns": {
				Type:        schema.TypeSet,
				Optional:    true,
				ForceNew:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Set:         schema.HashString,
				Description: "The specific columns to grant privileges on for this role",
			},
			"privileges": {
				Type:        schema.TypeSet,
				Required:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Set:         schema.HashString,
				Description: "The list of privileges to grant",
			},
			"with_grant_option": {
				Type:        schema.TypeBool,
				Optional:    true,
				ForceNew:    true,
				Default:     false,
				Description: "Permit the grant recipient to grant it to others",
			},
		},
	}
}

func resourcePostgreSQLGrantRead(db *DBConnection, d *schema.ResourceData) error {
	if err := validateFeatureSupport(db, d); err != nil {
		return fmt.Errorf("feature is not supported: %v", err)
	}

	// With catalog_snapshot, the object types the snapshot covers are read
	// from it; the others, and any Read the snapshot cannot serve, query the
	// database as before. Create and Update read directly after their write.
	switch d.Get("object_type").(string) {
	case "column", "foreign_data_wrapper", "foreign_server":
	default:
		snap, gone := grantReadSnapshot(db, d)
		if gone {
			return nil
		}
		if snap != nil {
			err := readRolePrivilegesFromSnapshot(snap, db, d)
			if err == nil {
				return nil
			}
			log.Printf("[WARN] could not read %s from the catalog snapshot, reading directly: %v", d.Id(), err)
		}
	}

	return readRolePrivileges(db, d)
}

func resourcePostgreSQLGrantCreate(db *DBConnection, d *schema.ResourceData) error {
	return resourcePostgreSQLGrantCreateOrUpdate(db, d, false)
}

func resourcePostgreSQLGrantUpdate(db *DBConnection, d *schema.ResourceData) error {
	return resourcePostgreSQLGrantCreateOrUpdate(db, d, true)
}

func resourcePostgreSQLGrantCreateOrUpdate(db *DBConnection, d *schema.ResourceData, usePrevious bool) error {
	if err := validateFeatureSupport(db, d); err != nil {
		return fmt.Errorf("feature is not supported: %v", err)
	}

	// Validate parameters.
	objectType := d.Get("object_type").(string)
	if d.Get("schema").(string) == "" && !sliceContainsStr([]string{"database", "foreign_data_wrapper", "foreign_server"}, objectType) {
		return fmt.Errorf("parameter 'schema' is mandatory for postgresql_grant resource")
	}
	if d.Get("objects").(*schema.Set).Len() > 0 && (objectType == "database" || objectType == "schema") {
		return fmt.Errorf("cannot specify `objects` when `object_type` is `database` or `schema`")
	}
	if d.Get("columns").(*schema.Set).Len() > 0 && (objectType != "column") {
		return fmt.Errorf("cannot specify `columns` when `object_type` is not `column`")
	}
	if d.Get("columns").(*schema.Set).Len() == 0 && (objectType == "column") {
		return fmt.Errorf("must specify `columns` when `object_type` is `column`")
	}
	if d.Get("privileges").(*schema.Set).Len() != 1 && (objectType == "column") {
		return fmt.Errorf("must specify exactly 1 `privileges` when `object_type` is `column`")
	}
	if (d.Get("objects").(*schema.Set).Len() != 1) && (objectType == "column") {
		return fmt.Errorf("must specify exactly 1 table in the `objects` field when `object_type` is `column`")
	}
	if d.Get("objects").(*schema.Set).Len() != 1 && (objectType == "foreign_data_wrapper" || objectType == "foreign_server") {
		return fmt.Errorf("one element must be specified in `objects` when `object_type` is `foreign_data_wrapper` or `foreign_server`")
	}
	if err := validatePrivileges(db, d); err != nil {
		return err
	}

	database := d.Get("database").(string)

	txn, err := startTransaction(db.client, database)
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	role := d.Get("role").(string)
	if err := pgLockRole(txn, role); err != nil {
		return err
	}

	if objectType == "database" {
		if err := pgLockDatabase(txn, database); err != nil {
			return err
		}
	}

	owners, err := getRolesToGrant(txn, d)
	if err != nil {
		return err
	}
	if err := withRolesGranted(txn, owners, func() error {
		// Revoke all privileges before granting otherwise reducing privileges will not work.
		// We just have to revoke them in the same transaction so the role will not lose its
		// privileges between the revoke and grant statements.
		if err := revokeRolePrivileges(txn, d, usePrevious); err != nil {
			return err
		}
		if err := grantRolePrivileges(txn, d); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	if err = txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}

	d.SetId(generateGrantID(d))

	return readRolePrivileges(db, d)
}

func resourcePostgreSQLGrantDelete(db *DBConnection, d *schema.ResourceData) error {
	if err := validateFeatureSupport(db, d); err != nil {
		return fmt.Errorf("feature is not supported: %v", err)
	}

	database := d.Get("database").(string)
	txn, err := startTransaction(db.client, database)
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	role := d.Get("role").(string)
	if err := pgLockRole(txn, role); err != nil {
		return err
	}

	objectType := d.Get("object_type").(string)
	if objectType == "database" {
		if err := pgLockDatabase(txn, database); err != nil {
			return err
		}
	}

	owners, err := getRolesToGrant(txn, d)
	if err != nil {
		return err
	}

	if err := withRolesGranted(txn, owners, func() error {
		return revokeRolePrivileges(txn, d, false)
	}); err != nil {
		return err
	}

	if err = txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}

	return nil
}

// grantReadRoleCTE resolves the grantee inside the Read query instead of with a
// separate role existence check and OID lookup. It always yields exactly one
// row: PUBLIC is grantee 0 and is not looked up in pg_roles (where it has no
// row), any other role yields its OID, or NULL when it does not exist. $1 is the
// role name.
const grantReadRoleCTE = `WITH r AS (
    SELECT CASE WHEN $1::name = '` + publicRole + `' THEN 0::oid
                ELSE (SELECT oid FROM pg_roles WHERE rolname = $1::name) END AS oid
)`

// grantReadSchemaCTE resolves the schema of the resource ($2). The Read queries
// report whether it exists next to the privileges, so a dropped schema is not
// mistaken for a schema whose objects have no privileges.
const grantReadSchemaCTE = `,
n AS (SELECT oid FROM pg_namespace WHERE nspname = $2)`

// queryGrantRead runs the single Read query of postgresql_grant or
// postgresql_default_privileges on the resource's database, outside of any
// transaction: the query reads only catalogs, so one statement already sees one
// consistent snapshot.
//
// When that database no longer exists (SQLSTATE 3D000, either while connecting
// or, when a pool for it already exists, on the query itself) the resource is
// removed from state and (nil, nil) is returned. That replaces the separate
// check against pg_database on the maintenance database.
func queryGrantRead(db *DBConnection, d *schema.ResourceData, query string, args ...any) (*sql.Rows, error) {
	database := d.Get("database").(string)

	conn, err := initConnection(db.client, database)
	if err == nil {
		var rows *sql.Rows
		if rows, err = conn.Query(query, args...); err == nil {
			return rows, nil
		}
	}
	if isDatabaseDoesNotExistError(err) {
		log.Printf("[WARN] PostgreSQL database (%s) not found, removing %s from state", database, d.Id())
		d.SetId("")
		return nil, nil
	}
	return nil, err
}

// closeGrantReadRows closes the rows of queryGrantRead. Without a transaction
// around the Read, rows left open would keep the connection checked out.
func closeGrantReadRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		log.Printf("[ERR] could not close rows: %v", err)
	}
}

// grantTargetGone removes the resource from state when the role or the schema
// it refers to no longer exists, as reported by the role_exists/schema_exists
// columns of the Read query, and reports whether it did.
func grantTargetGone(d *schema.ResourceData, roleExists, schemaExists bool) bool {
	if !roleExists {
		log.Printf("[DEBUG] role %s does not exists", d.Get("role").(string))
		d.SetId("")
		return true
	}
	if !schemaExists {
		log.Printf("[DEBUG] schema %s does not exists", d.Get("schema").(string))
		d.SetId("")
		return true
	}
	return false
}

// buildReadSingleObjectRolePrivilegesQuery builds the Read query for the object
// types granted on one named object (database, schema, foreign data wrapper,
// foreign server). It returns one row: role_exists, schema_exists (always true
// when the object type is not schema-scoped) and the privileges granted to the
// role on the object (NULL when there are none).
func buildReadSingleObjectRolePrivilegesQuery(d *schema.ResourceData) (string, []any) {
	role := d.Get("role").(string)

	if d.Get("object_type").(string) == "schema" {
		return grantReadRoleCTE + `
SELECT r.oid IS NOT NULL, n.oid IS NOT NULL,
    (SELECT array_agg(a.privilege_type) FROM aclexplode(n.nspacl) a WHERE a.grantee = r.oid)
FROM r
LEFT JOIN pg_namespace n ON n.nspname = $2
`, []any{role, d.Get("schema").(string)}
	}

	var catalog, acl, nameColumn, name string
	switch d.Get("object_type").(string) {
	case "database":
		catalog, acl, nameColumn = "pg_database", "datacl", "datname"
		name = d.Get("database").(string)
	case "foreign_data_wrapper":
		catalog, acl, nameColumn = "pg_foreign_data_wrapper", "fdwacl", "fdwname"
		name = d.Get("objects").(*schema.Set).List()[0].(string)
	case "foreign_server":
		catalog, acl, nameColumn = "pg_foreign_server", "srvacl", "srvname"
		name = d.Get("objects").(*schema.Set).List()[0].(string)
	}

	return grantReadRoleCTE + fmt.Sprintf(`
SELECT r.oid IS NOT NULL, true,
    (SELECT pg_catalog.array_agg(a.privilege_type)
     FROM pg_catalog.%s o, pg_catalog.aclexplode(o.%s) a
     WHERE o.%s = $2 AND a.grantee = r.oid)
FROM r
`, catalog, acl, nameColumn), []any{role, name}
}

func readSingleObjectRolePrivileges(db *DBConnection, d *schema.ResourceData) error {
	query, args := buildReadSingleObjectRolePrivilegesQuery(d)
	rows, err := queryGrantRead(db, d, query, args...)
	if err != nil {
		return fmt.Errorf("could not read privileges: %w", err)
	}
	if rows == nil {
		return nil
	}
	defer closeGrantReadRows(rows)

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("could not read privileges: %w", err)
		}
		return fmt.Errorf("could not read privileges: query returned no row")
	}

	var roleExists, schemaExists bool
	var privileges pq.ByteaArray
	if err := rows.Scan(&roleExists, &schemaExists, &privileges); err != nil {
		return fmt.Errorf("could not read privileges: %w", err)
	}
	if grantTargetGone(d, roleExists, schemaExists) {
		return nil
	}
	d.SetId(generateGrantID(d))

	granted := pgArrayToSet(privileges)
	if !resourcePrivilegesEqual(granted, db, d) {
		return d.Set("privileges", granted)
	}
	return nil
}

// buildReadColumnRolePrivilegesQuery builds the Read query for column grants.
// It returns role_exists and schema_exists on every row, then one row per
// column of the table that has the wanted privilege explicitly granted to the
// role (attacl holds only explicit column grants), or a single row with NULL
// table/column when there is none.
func buildReadColumnRolePrivilegesQuery(d *schema.ResourceData) (string, []any) {
	query := grantReadRoleCTE + grantReadSchemaCTE + `
SELECT r.oid IS NOT NULL, EXISTS (SELECT 1 FROM n), o.table_name, o.column_name, o.column_privileges
FROM r
LEFT JOIN LATERAL (
    SELECT c.relname AS table_name, att.attname AS column_name, array_agg(a.privilege_type) AS column_privileges
    FROM pg_class c
    JOIN n ON n.oid = c.relnamespace
    JOIN pg_attribute att ON att.attrelid = c.oid
    CROSS JOIN LATERAL aclexplode(att.attacl) a
    WHERE c.relname = $3 AND c.relkind = $4 AND a.grantee = r.oid AND a.privilege_type = $5
    GROUP BY c.relname, att.attname, a.privilege_type
) o ON true
ORDER BY o.column_name
`
	return query, []any{
		d.Get("role").(string),
		d.Get("schema").(string),
		d.Get("objects").(*schema.Set).List()[0],
		objectTypes["table"],
		d.Get("privileges").(*schema.Set).List()[0],
	}
}

func readColumnRolePrivileges(db *DBConnection, d *schema.ResourceData) error {
	objects := d.Get("objects").(*schema.Set)

	missingColumns := d.Get("columns").(*schema.Set) // Getting columns from state.
	// If the query returns a column, it is a removed from the missingColumns.

	query, args := buildReadColumnRolePrivilegesQuery(d)
	rows, err := queryGrantRead(db, d, query, args...)
	if err != nil {
		return err
	}
	if rows == nil {
		return nil
	}
	defer closeGrantReadRows(rows)

	checked := false
	for rows.Next() {
		var roleExists, schemaExists bool
		var objName, colName sql.NullString
		var privileges pq.ByteaArray

		if err := rows.Scan(&roleExists, &schemaExists, &objName, &colName, &privileges); err != nil {
			return err
		}

		if !checked {
			if grantTargetGone(d, roleExists, schemaExists) {
				return nil
			}
			d.SetId(generateGrantID(d))
			checked = true
		}

		if !objName.Valid {
			// No column has the privilege: only the existence flags were returned.
			continue
		}

		if objects.Len() > 0 && !objects.Contains(objName.String) {
			continue
		}

		if missingColumns.Contains(colName.String) {
			missingColumns.Remove(colName.String)
		}

		privilegesSet := pgArrayToSet(privileges)

		if !privilegesSet.Equal(d.Get("privileges").(*schema.Set)) {
			// If any object doesn't have the same privileges as saved in the state,
			// we return its privileges to force an update.
			log.Printf(
				"[DEBUG] %s %s has not the expected privileges %v for role %s",
				strings.ToTitle("column"), objName.String, privileges, d.Get("role"),
			)
			d.Set("privileges", privilegesSet)
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !checked {
		return fmt.Errorf("could not read column privileges: query returned no row")
	}

	if missingColumns.Len() > 0 {
		// If missingColumns is not empty by the end of the result processing loop
		// it means that a column is missing
		remainingColumns := d.Get("columns").(*schema.Set).Difference(missingColumns)
		log.Printf(
			"[DEBUG] Role %s does not have the expected privileges on columns",
			d.Get("role"),
		)
		d.Set("columns", remainingColumns)
	}

	return nil
}

// buildReadTableRolePrivilegesQuery builds the Read query and bind arguments
// for the table/sequence object types. It returns role_exists and
// schema_exists on every row, then one row per relation of the relkind in the
// schema with the privileges granted to the role, or a single row with a NULL
// relation when there is none (or when the role or schema is missing).
//
// When the resource lists explicit objects, the relname filter is pushed into
// the query (relname = ANY) instead of fetching every relation of this relkind
// in the schema and discarding the surplus afterwards; on schemas with many
// relations this avoids transferring rows that are immediately dropped. An
// empty objects set means "all objects in the schema", so the predicate is only
// added when objects are listed — the empty case returns the query without it
// and a three-argument bind list.
//
// The ACL is exploded per selected relation through LEFT JOIN LATERAL rather
// than in a subquery over the whole pg_class joined back by name: with the
// relname filter the planner turned that join into a nested loop that re-ran
// aclexplode over every relation in the catalog once per matched object.
// Relations without an ACL entry for the role (including a NULL relacl) still
// produce a row with an empty privilege array.
func buildReadTableRolePrivilegesQuery(role, schemaName, relkind string, objects *schema.Set) (string, []interface{}) {
	query := grantReadRoleCTE + grantReadSchemaCTE + `
SELECT r.oid IS NOT NULL, EXISTS (SELECT 1 FROM n), o.relname, o.privileges
FROM r
LEFT JOIN LATERAL (
    SELECT c.relname, array_remove(array_agg(a.privilege_type), NULL) AS privileges
    FROM pg_class c
    JOIN n ON n.oid = c.relnamespace
    LEFT JOIN LATERAL (
        SELECT privilege_type FROM aclexplode(c.relacl) WHERE grantee = r.oid
    ) a ON true
    WHERE c.relkind = $3`
	args := []interface{}{role, schemaName, relkind}

	if objects.Len() > 0 {
		objectNames := make([]string, 0, objects.Len())
		for _, o := range objects.List() {
			objectNames = append(objectNames, o.(string))
		}
		query += " AND c.relname = ANY($4)"
		args = append(args, pq.Array(objectNames))
	}
	query += `
    GROUP BY c.relname
) o ON true
`

	return query, args
}

// buildReadFunctionRolePrivilegesQuery builds the Read query for the
// function/procedure/routine object types: role_exists and schema_exists on
// every row, then one row per routine name in the schema (overloads merged)
// with the privileges granted to the role, or a single row with a NULL name.
//
// With an explicit objects list only the listed names are returned
// ("proname = ANY($3)"), so the planner can use the pg_proc name index instead
// of exploding the ACL of every routine in the schema. The entries are bound
// verbatim: rows are matched to objects by name, so an entry carrying an
// argument signature (e.g. "f(integer)") selects no row, as it never matched
// the name-based filter in readRolePrivileges either.
func buildReadFunctionRolePrivilegesQuery(role, schemaName string, objects *schema.Set) (string, []interface{}) {
	query := grantReadRoleCTE + grantReadSchemaCTE + `
SELECT r.oid IS NOT NULL, EXISTS (SELECT 1 FROM n), o.proname, o.privileges
FROM r
LEFT JOIN LATERAL (
    SELECT p.proname, array_remove(array_agg(a.privilege_type), NULL) AS privileges
    FROM pg_proc p
    JOIN n ON n.oid = p.pronamespace
    LEFT JOIN LATERAL (
        SELECT privilege_type FROM aclexplode(p.proacl) WHERE grantee = r.oid
    ) a ON true`
	args := []interface{}{role, schemaName}

	if objects.Len() > 0 {
		objectNames := make([]string, 0, objects.Len())
		for _, o := range objects.List() {
			objectNames = append(objectNames, o.(string))
		}
		query += `
    WHERE p.proname = ANY($3)`
		args = append(args, pq.Array(objectNames))
	}
	query += `
    GROUP BY p.proname
) o ON true
`
	return query, args
}

// readRolePrivileges reads the privileges granted to the role with a single
// statement on the target database, without a transaction. The same statement
// reports whether the role and the schema still exist: a missing database,
// role or schema removes the resource from state, so a dropped object is never
// confused with an object without privileges.
func readRolePrivileges(db *DBConnection, d *schema.ResourceData) error {
	role := d.Get("role").(string)
	objectType := d.Get("object_type").(string)
	objects := d.Get("objects").(*schema.Set)

	var query string
	var args []interface{}

	switch objectType {
	case "database", "schema", "foreign_data_wrapper", "foreign_server":
		return readSingleObjectRolePrivileges(db, d)

	case "column":
		return readColumnRolePrivileges(db, d)

	case "function", "procedure", "routine":
		query, args = buildReadFunctionRolePrivilegesQuery(role, d.Get("schema").(string), objects)

	default:
		query, args = buildReadTableRolePrivilegesQuery(
			role, d.Get("schema").(string), objectTypes[objectType], objects,
		)
	}

	// This returns, for the specified role (rolname),
	// the list of all object of the specified type (relkind) in the specified schema (namespace)
	// with the list of the currently applied privileges (aggregation of privilege_type)
	//
	// Our goal is to check that every object has the same privileges as saved in the state.
	rows, err := queryGrantRead(db, d, query, args...)
	if err != nil {
		return err
	}
	if rows == nil {
		return nil
	}
	defer closeGrantReadRows(rows)

	checked := false
	for rows.Next() {
		var roleExists, schemaExists bool
		var objName sql.NullString
		var privileges pq.ByteaArray

		if err := rows.Scan(&roleExists, &schemaExists, &objName, &privileges); err != nil {
			return err
		}

		if !checked {
			if grantTargetGone(d, roleExists, schemaExists) {
				return nil
			}
			d.SetId(generateGrantID(d))
			checked = true
		}

		if !objName.Valid {
			// The schema has no object of this type: only the existence flags
			// were returned.
			continue
		}

		// The objects filter already runs in SQL for every object type; this
		// check only keeps the Read independent of the exact query shape.
		if objects.Len() > 0 && !objects.Contains(objName.String) {
			continue
		}

		privilegesSet := pgArrayToSet(privileges)
		if !resourcePrivilegesEqual(privilegesSet, db, d) {
			// If any object doesn't have the same privileges as saved in the state,
			// we return its privileges to force an update.
			log.Printf(
				"[DEBUG] %s %s has not the expected privileges %v for role %s",
				strings.ToTitle(objectType), objName.String, privileges, d.Get("role"),
			)
			d.Set("privileges", privilegesSet)
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !checked {
		return fmt.Errorf("could not read privileges: query returned no row")
	}

	return nil
}

// readRolePrivilegesFromSnapshot is readRolePrivileges (and the single-object
// Read of schema and database grants) served from the catalog snapshot, with
// the same outcome for every input: a missing role or schema removes the
// resource from state, objects are matched by name, routine overloads are
// merged by name, and the privileges of the first object (by name) whose
// privileges differ from the configured ones are written to state. Nothing is
// written to d before every ACL it needs has been parsed, so on an error the
// caller can fall back to the direct Read.
func readRolePrivilegesFromSnapshot(snap *catalogSnapshot, db *DBConnection, d *schema.ResourceData) error {
	objectType := d.Get("object_type").(string)
	schemaName := d.Get("schema").(string)
	objects := d.Get("objects").(*schema.Set)
	grantee, roleExists := snap.granteeOf(d.Get("role").(string))

	// drift is the privilege set to write to state, nil when every object
	// has the configured privileges.
	var drift *schema.Set
	schemaExists := true

	single := func(acl sql.NullString) error {
		granted, err := privilegesOf(acl, grantee)
		if err != nil {
			return err
		}
		if !resourcePrivilegesEqual(granted, db, d) {
			drift = granted
		}
		return nil
	}

	switch objectType {
	case "database":
		if err := single(snap.datACL); err != nil {
			return err
		}

	case "schema":
		ns, ok := snap.namespaces[schemaName]
		schemaExists = ok
		if ok {
			if err := single(ns.acl); err != nil {
				return err
			}
		}

	default:
		ns, ok := snap.namespaces[schemaName]
		schemaExists = ok
		// candidates are the routines of the schema (no prokind filter, as
		// the direct Read) or its relations, filtered by relkind.
		var candidates []catalogObject
		var relkind string
		if ok {
			switch objectType {
			case "function", "procedure", "routine":
				var err error
				if candidates, err = snap.routinesOf(ns.oid); err != nil {
					return err
				}
			default:
				candidates = snap.relations[ns.oid]
				relkind = objectTypes[objectType]
			}
		}

		for i := 0; i < len(candidates); i++ {
			object := candidates[i]
			if relkind != "" && object.kind != relkind {
				continue
			}
			if objects.Len() > 0 && !objects.Contains(object.name) {
				continue
			}

			// The direct Read aggregates the privileges of every routine
			// with this name: a name reads as granted when any overload is.
			granted, err := privilegesOf(object.acl, grantee)
			if err != nil {
				return err
			}
			for i+1 < len(candidates) && candidates[i+1].name == object.name {
				i++
				more, err := privilegesOf(candidates[i].acl, grantee)
				if err != nil {
					return err
				}
				granted = granted.Union(more)
			}

			if !resourcePrivilegesEqual(granted, db, d) {
				log.Printf(
					"[DEBUG] %s %s has not the expected privileges %v for role %s",
					strings.ToTitle(objectType), object.name, granted.List(), d.Get("role"),
				)
				drift = granted
				break
			}
		}
	}

	if grantTargetGone(d, roleExists, schemaExists) {
		return nil
	}
	d.SetId(generateGrantID(d))
	if drift != nil {
		return d.Set("privileges", drift)
	}
	return nil
}

func createGrantQuery(d *schema.ResourceData, privileges []string) string {
	var query string

	switch strings.ToUpper(d.Get("object_type").(string)) {
	case "DATABASE":
		query = fmt.Sprintf(
			"GRANT %s ON DATABASE %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(d.Get("database").(string)),
			pq.QuoteIdentifier(d.Get("role").(string)),
		)
	case "SCHEMA":
		query = fmt.Sprintf(
			"GRANT %s ON SCHEMA %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(d.Get("schema").(string)),
			pq.QuoteIdentifier(d.Get("role").(string)),
		)
	case "FOREIGN_DATA_WRAPPER":
		fdwName := d.Get("objects").(*schema.Set).List()[0]
		query = fmt.Sprintf(
			"GRANT %s ON FOREIGN DATA WRAPPER %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(fdwName.(string)),
			pq.QuoteIdentifier(d.Get("role").(string)),
		)
	case "FOREIGN_SERVER":
		srvName := d.Get("objects").(*schema.Set).List()[0]
		query = fmt.Sprintf(
			"GRANT %s ON FOREIGN SERVER %s TO %s",
			strings.Join(privileges, ","),
			pq.QuoteIdentifier(srvName.(string)),
			pq.QuoteIdentifier(d.Get("role").(string)),
		)
	case "COLUMN":
		objects := d.Get("objects").(*schema.Set)
		query = fmt.Sprintf(
			"GRANT %s (%s) ON TABLE %s TO %s",
			strings.Join(privileges, ","),
			setToPgIdentListWithoutSchema(d.Get("columns").(*schema.Set)),
			setToPgIdentList(d.Get("schema").(string), objects),
			pq.QuoteIdentifier(d.Get("role").(string)),
		)
	case "TABLE", "SEQUENCE", "FUNCTION", "PROCEDURE", "ROUTINE":
		objects := d.Get("objects").(*schema.Set)
		if objects.Len() > 0 {
			query = fmt.Sprintf(
				"GRANT %s ON %s %s TO %s",
				strings.Join(privileges, ","),
				strings.ToUpper(d.Get("object_type").(string)),
				setToPgIdentList(d.Get("schema").(string), objects),
				pq.QuoteIdentifier(d.Get("role").(string)),
			)
		} else {
			query = fmt.Sprintf(
				"GRANT %s ON ALL %sS IN SCHEMA %s TO %s",
				strings.Join(privileges, ","),
				strings.ToUpper(d.Get("object_type").(string)),
				pq.QuoteIdentifier(d.Get("schema").(string)),
				pq.QuoteIdentifier(d.Get("role").(string)),
			)
		}
	}

	if d.Get("with_grant_option").(bool) {
		query = query + " WITH GRANT OPTION"
	}

	return query
}

func createRevokeQuery(getter ResourceSchemeGetter) string {
	var query string

	switch strings.ToUpper(getter("object_type").(string)) {
	case "DATABASE":
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON DATABASE %s FROM %s",
			pq.QuoteIdentifier(getter("database").(string)),
			pq.QuoteIdentifier(getter("role").(string)),
		)
	case "SCHEMA":
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON SCHEMA %s FROM %s",
			pq.QuoteIdentifier(getter("schema").(string)),
			pq.QuoteIdentifier(getter("role").(string)),
		)
	case "FOREIGN_DATA_WRAPPER":
		fdwName := getter("objects").(*schema.Set).List()[0]
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON FOREIGN DATA WRAPPER %s FROM %s",
			pq.QuoteIdentifier(fdwName.(string)),
			pq.QuoteIdentifier(getter("role").(string)),
		)
	case "FOREIGN_SERVER":
		srvName := getter("objects").(*schema.Set).List()[0]
		query = fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON FOREIGN SERVER %s FROM %s",
			pq.QuoteIdentifier(srvName.(string)),
			pq.QuoteIdentifier(getter("role").(string)),
		)
	case "COLUMN":
		objects := getter("objects").(*schema.Set)
		columns := getter("columns").(*schema.Set)
		privileges := getter("privileges").(*schema.Set)
		if privileges.Len() == 0 || columns.Len() == 0 {
			// No privileges to revoke, so don't revoke anything
			query = "SELECT NULL"
		} else {
			query = fmt.Sprintf(
				"REVOKE %s (%s) ON TABLE %s FROM %s",
				setToPgIdentSimpleList(privileges),
				setToPgIdentListWithoutSchema(columns),
				setToPgIdentList(getter("schema").(string), objects),
				pq.QuoteIdentifier(getter("role").(string)),
			)
		}
	case "TABLE", "SEQUENCE", "FUNCTION", "PROCEDURE", "ROUTINE":
		objects := getter("objects").(*schema.Set)
		privileges := getter("privileges").(*schema.Set)
		if objects.Len() > 0 {
			if privileges.Len() > 0 {
				// Revoking specific privileges instead of all privileges
				// to avoid messing with column level grants
				query = fmt.Sprintf(
					"REVOKE %s ON %s %s FROM %s",
					setToPgIdentSimpleList(privileges),
					strings.ToUpper(getter("object_type").(string)),
					setToPgIdentList(getter("schema").(string), objects),
					pq.QuoteIdentifier(getter("role").(string)),
				)
			} else {
				query = fmt.Sprintf(
					"REVOKE ALL PRIVILEGES ON %s %s FROM %s",
					strings.ToUpper(getter("object_type").(string)),
					setToPgIdentList(getter("schema").(string), objects),
					pq.QuoteIdentifier(getter("role").(string)),
				)
			}
		} else {
			query = fmt.Sprintf(
				"REVOKE ALL PRIVILEGES ON ALL %sS IN SCHEMA %s FROM %s",
				strings.ToUpper(getter("object_type").(string)),
				pq.QuoteIdentifier(getter("schema").(string)),
				pq.QuoteIdentifier(getter("role").(string)),
			)
		}
	}

	return query
}

func grantRolePrivileges(txn *sql.Tx, d *schema.ResourceData) error {
	privileges := []string{}
	for _, priv := range d.Get("privileges").(*schema.Set).List() {
		privileges = append(privileges, priv.(string))
	}

	if len(privileges) == 0 {
		log.Printf("[DEBUG] no privileges to grant for role %s in database: %s,", d.Get("role").(string), d.Get("database"))
		return nil
	}

	query := createGrantQuery(d, privileges)

	_, err := txn.Exec(query)
	return err
}

func revokeRolePrivileges(txn *sql.Tx, d *schema.ResourceData, usePrevious bool) error {
	getter := d.Get

	if usePrevious {
		getter = func(name string) any {
			if d.HasChange(name) {
				old, _ := d.GetChange(name)
				return old
			}

			return d.Get(name)
		}
	}

	query := createRevokeQuery(getter)
	if len(query) == 0 {
		// Query is empty, don't run anything
		return nil
	}
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not execute revoke query: %w", err)
	}
	return nil
}

func generateGrantID(d *schema.ResourceData) string {
	parts := []string{d.Get("role").(string), d.Get("database").(string)}

	objectType := d.Get("object_type").(string)
	if objectType != "database" && objectType != "foreign_data_wrapper" && objectType != "foreign_server" {
		parts = append(parts, d.Get("schema").(string))
	}
	parts = append(parts, objectType)

	for _, object := range d.Get("objects").(*schema.Set).List() {
		parts = append(parts, object.(string))
	}

	for _, column := range d.Get("columns").(*schema.Set).List() {
		parts = append(parts, column.(string))
	}

	return strings.Join(parts, "_")
}

func getRolesToGrant(txn *sql.Tx, d *schema.ResourceData) ([]string, error) {
	// If user we use for Terraform is not a superuser (e.g.: in RDS)
	// we need to grant owner of the schema and owners of tables in the schema
	// in order to change theirs permissions.
	owners := []string{}
	objectType := d.Get("object_type")

	if objectType == "database" || objectType == "foreign_data_wrapper" || objectType == "foreign_server" {
		return owners, nil
	}

	schemaName := d.Get("schema").(string)

	if objectType != "schema" {
		var err error
		owners, err = getTablesOwner(txn, schemaName)
		if err != nil {
			return nil, err
		}
	}

	schemaOwner, err := getSchemaOwner(txn, schemaName)
	if err != nil {
		return nil, err
	}
	if !sliceContainsStr(owners, schemaOwner) {
		owners = append(owners, schemaOwner)
	}

	owners, err = resolveOwners(txn, owners)
	if err != nil {
		return nil, err
	}

	return owners, nil
}

func validateFeatureSupport(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featurePrivileges) {
		return fmt.Errorf(
			"postgresql_grant resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	if d.Get("object_type") == "procedure" && !db.featureSupported(featureProcedure) {
		return fmt.Errorf(
			"object type PROCEDURE is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	if d.Get("object_type") == "routine" && !db.featureSupported(featureRoutine) {
		return fmt.Errorf(
			"object type ROUTINE is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	return nil
}
