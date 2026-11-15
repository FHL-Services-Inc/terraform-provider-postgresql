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

func resourcePostgreSQLDefaultPrivileges() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceWriteFunc(retryOnConcurrentCatalogUpdate(resourcePostgreSQLDefaultPrivilegesCreate)),
		Update: PGResourceWriteFunc(retryOnConcurrentCatalogUpdate(resourcePostgreSQLDefaultPrivilegesCreate)),
		Read:   PGResourceFunc(resourcePostgreSQLDefaultPrivilegesRead),
		Delete: PGResourceWriteFunc(retryOnConcurrentCatalogUpdate(resourcePostgreSQLDefaultPrivilegesDelete)),
		Importer: &schema.ResourceImporter{
			StateContext: resourcePostgreSQLDefaultPrivilegesImport,
		},

		Schema: map[string]*schema.Schema{
			"role": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The name of the role to which grant default privileges on",
			},
			"database": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringIsNotEmpty,
				Description:  "The database to grant default privileges for this role",
			},
			"owner": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "Target role for which to alter default privileges.",
			},
			"schema": {
				Type:        schema.TypeString,
				Optional:    true,
				ForceNew:    true,
				Description: "The database schema to set default privileges for this role",
			},
			"object_type": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				ValidateFunc: validation.StringInSlice([]string{
					"table",
					"sequence",
					"function",
					"routine",
					"type",
					"schema",
				}, false),
				Description: "The PostgreSQL object type to set the default privileges on (one of: table, sequence, function, routine, type, schema)",
			},
			"privileges": {
				Type:        schema.TypeSet,
				Required:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Set:         schema.HashString,
				Description: "The list of privileges to apply as default privileges",
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

func resourcePostgreSQLDefaultPrivilegesRead(db *DBConnection, d *schema.ResourceData) error {
	pgSchema := d.Get("schema").(string)
	objectType := d.Get("object_type").(string)

	if pgSchema != "" && objectType == "schema" && !db.featureSupported(featurePrivilegesOnSchemas) {
		return fmt.Errorf(
			"changing default privileges for schemas is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	if objectType == "routine" && !db.featureSupported(featureRoutine) {
		return fmt.Errorf(
			"object type ROUTINE is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	// With catalog_snapshot the Read is served from the snapshot of the
	// database; Create and Update read directly after their write.
	snap, gone := grantReadSnapshot(db, d)
	if gone {
		return nil
	}
	if snap != nil {
		err := readRoleDefaultPrivilegesFromSnapshot(snap, db, d)
		if err == nil {
			return nil
		}
		log.Printf("[WARN] could not read %s from the catalog snapshot, reading directly: %v", d.Id(), err)
	}

	return readRoleDefaultPrivileges(db, d)
}

func resourcePostgreSQLDefaultPrivilegesCreate(db *DBConnection, d *schema.ResourceData) error {
	pgSchema := d.Get("schema").(string)
	objectType := d.Get("object_type").(string)

	if pgSchema != "" && objectType == "schema" {
		if !db.featureSupported(featurePrivilegesOnSchemas) {
			return fmt.Errorf(
				"changing default privileges for schemas is not supported for this Postgres version (%s)",
				db.version,
			)
		}
		return fmt.Errorf("cannot specify `schema` when `object_type` is `schema`")
	}

	if objectType == "routine" && !db.featureSupported(featureRoutine) {
		return fmt.Errorf(
			"object type ROUTINE is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	if d.Get("with_grant_option").(bool) && strings.ToLower(d.Get("role").(string)) == "public" {
		return fmt.Errorf("with_grant_option cannot be true for role 'public'")
	}

	if err := validatePrivileges(db, d); err != nil {
		return err
	}

	database := d.Get("database").(string)
	owner := d.Get("owner").(string)

	txn, err := startTransaction(db.client, database)
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if err := lockDefaultPrivileges(txn, d); err != nil {
		return err
	}

	// Needed in order to set the owner of the db if the connection user is not a superuser
	if err := withRolesGranted(db, txn, []string{owner}, func() error {

		// Revoke all privileges before granting otherwise reducing privileges will not work.
		// We just have to revoke them in the same transaction so role will not lose its privileges
		// between revoke and grant.
		if err = revokeRoleDefaultPrivileges(txn, d); err != nil {
			return err
		}

		if err = grantRoleDefaultPrivileges(txn, d); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	if err := commitTxn(txn); err != nil {
		return err
	}

	d.SetId(generateDefaultPrivilegesID(d))

	return readRoleDefaultPrivileges(db, d)
}

func resourcePostgreSQLDefaultPrivilegesDelete(db *DBConnection, d *schema.ResourceData) error {
	owner := d.Get("owner").(string)
	pgSchema := d.Get("schema").(string)
	objectType := d.Get("object_type").(string)

	if pgSchema != "" && objectType == "schema" && !db.featureSupported(featurePrivilegesOnSchemas) {
		return fmt.Errorf(
			"changing default privileges for schemas is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	txn, err := startTransaction(db.client, d.Get("database").(string))
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if err := lockDefaultPrivileges(txn, d); err != nil {
		return err
	}

	// Needed in order to set the owner of the db if the connection user is not a superuser
	if err := withRolesGranted(db, txn, []string{owner}, func() error {
		if err := revokeRoleDefaultPrivileges(txn, d); err != nil {
			return err
		}
		return restoreBuiltinDefaultPrivileges(db, txn, d)
	}); err != nil {
		return err
	}

	if err := commitTxn(txn); err != nil {
		return err
	}

	return nil
}

// lockDefaultPrivileges locks the owner role (not its members: default
// privileges change no membership), then the pg_default_acl row of the
// resource (see grant_locks.go).
func lockDefaultPrivileges(txn *sql.Tx, d *schema.ResourceData) error {
	return pgLockGranteeAndObjects(txn, d.Get("owner").(string), "",
		[]advisoryLockKey{defaultPrivilegesLockKey(d)})
}

// buildReadRoleDefaultPrivilegesQuery builds the default privileges Read query.
// It returns one row: role_exists, schema_exists (true when no schema is set)
// and the default privileges the owner grants to the role (NULL when none).
//
// pg_default_acl is filtered on its unique key (defaclrole, defaclnamespace,
// defaclobjtype) before the ACL is exploded, and the owner is matched by OID.
// aclexplode(defaclacl) always reports defaclrole as the grantor, so this is the
// same entry the previous filter on pg_get_userbyid(grantor) = owner selected,
// without calling that function for every default ACL item. A missing owner
// matches no entry, exactly as before.
//
// Without a schema, a missing entry reads as acldefault(type, owner): this is
// what the owner's new objects get, and PostgreSQL deletes a global entry that
// becomes equal to it (e.g. ALL on tables for the owner itself, or EXECUTE on
// functions for PUBLIC), so such an entry is present, not gone (#117). A
// per-schema entry is only added to the global one, so a missing one grants
// nothing.
func buildReadRoleDefaultPrivilegesQuery(d *schema.ResourceData) (string, []any) {
	pgSchema := d.Get("schema").(string)
	args := []any{
		d.Get("role").(string),
		d.Get("owner").(string),
		objectTypes[d.Get("object_type").(string)],
	}

	const entry = `SELECT da.defaclacl FROM pg_default_acl da
        WHERE da.defaclrole = (SELECT oid FROM o)
          AND da.defaclnamespace = %s
          AND da.defaclobjtype = $3`

	acl, schemaExists, withSchema := "", "true", ""
	if pgSchema != "" {
		withSchema = `,
n AS (SELECT oid FROM pg_namespace WHERE nspname = $4)`
		schemaExists = "EXISTS (SELECT 1 FROM n)"
		acl = "(" + fmt.Sprintf(entry, "(SELECT oid FROM n)") + ")"
		args = append(args, pgSchema)
	} else {
		acl = "COALESCE((" + fmt.Sprintf(entry, "0") + "),\n        acldefault(" +
			pq.QuoteLiteral(defaultACLBuiltinType(d.Get("object_type").(string))) + ", (SELECT oid FROM o)))"
	}

	query := grantReadRoleCTE + `,
o AS (SELECT oid FROM pg_roles WHERE rolname = $2)` + withSchema + `
SELECT r.oid IS NOT NULL, ` + schemaExists + `, (
    SELECT array_agg(a.privilege_type)
    FROM aclexplode(` + acl + `) a
    WHERE a.grantee = r.oid
)
FROM r
`
	return query, args
}

// readRoleDefaultPrivileges reads the default privileges with a single
// statement on the target database: no transaction, no separate existence
// checks and no advisory lock on the owner (only the write paths lock it), so
// Reads of default privileges sharing an owner no longer wait for each other
// or for a concurrent apply on that owner.
func readRoleDefaultPrivileges(db *DBConnection, d *schema.ResourceData) error {
	role := d.Get("role").(string)
	pgSchema := d.Get("schema").(string)
	privilegesInput := d.Get("privileges").(*schema.Set).List()

	query, args := buildReadRoleDefaultPrivilegesQuery(d)
	rows, err := queryGrantRead(db, d, query, args...)
	if err != nil {
		return fmt.Errorf("could not read default privileges: %w", err)
	}
	if rows == nil {
		return nil
	}
	defer closeGrantReadRows(rows)

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("could not read default privileges: %w", err)
		}
		return fmt.Errorf("could not read default privileges: query returned no row")
	}

	// This query aggregates the list of default privileges type (prtype)
	// for the role (grantee), owner (grantor), schema (namespace name)
	// and the specified object type (defaclobjtype).
	var roleExists, schemaExists bool
	var privileges pq.ByteaArray
	if err := rows.Scan(&roleExists, &schemaExists, &privileges); err != nil {
		return fmt.Errorf("could not read default privileges: %w", err)
	}
	if grantTargetGone(d, roleExists, schemaExists) {
		return nil
	}

	// We consider no privileges as "not exists" unless no privileges were provided as input
	if len(privileges) == 0 {
		log.Printf("[DEBUG] no default privileges for role %s in schema %s", role, pgSchema)
		if len(privilegesInput) != 0 {
			d.SetId("")
			return nil
		}
	}

	privilegesSet := pgArrayToSet(privileges)
	privilegesEqual := resourcePrivilegesEqual(privilegesSet, db, d)

	if !privilegesEqual {
		d.Set("privileges", privilegesSet)
	}
	d.SetId(generateDefaultPrivilegesID(d))

	return nil
}

// readRoleDefaultPrivilegesFromSnapshot is readRoleDefaultPrivileges served
// from the catalog snapshot, with the same outcome: the pg_default_acl entry
// is the one keyed by the owner's OID, the schema's OID (0 without a schema)
// and the object type; a missing role or schema removes the resource from
// state; a missing owner reads as no privileges, and so does a missing entry
// in a schema, while a missing global entry reads as the built-in defaults
// (builtinDefaultPrivileges).
func readRoleDefaultPrivilegesFromSnapshot(snap *catalogSnapshot, db *DBConnection, d *schema.ResourceData) error {
	role := d.Get("role").(string)
	pgSchema := d.Get("schema").(string)
	privilegesInput := d.Get("privileges").(*schema.Set).List()

	grantee, roleExists := snap.granteeOf(role)
	schemaExists := true
	var namespace uint32
	if pgSchema != "" {
		ns, ok := snap.namespaces[pgSchema]
		schemaExists = ok
		namespace = ns.oid
	}

	var privileges []string
	if owner, ok := snap.roles[d.Get("owner").(string)]; ok {
		key := defaultACLKey{owner: owner, namespace: namespace, objType: objectTypes[d.Get("object_type").(string)]}
		if acl, ok := snap.defaultACL[key]; ok {
			var err error
			if privileges, err = aclPrivilegesGrantedTo(acl, grantee); err != nil {
				return err
			}
		} else if pgSchema == "" {
			privileges = builtinDefaultPrivileges(db, d.Get("object_type").(string), role == d.Get("owner").(string), role == publicRole)
		}
	}

	if grantTargetGone(d, roleExists, schemaExists) {
		return nil
	}

	// We consider no privileges as "not exists" unless no privileges were provided as input
	if len(privileges) == 0 {
		log.Printf("[DEBUG] no default privileges for role %s in schema %s", role, pgSchema)
		if len(privilegesInput) != 0 {
			d.SetId("")
			return nil
		}
	}

	privilegesSet := stringSliceToSet(privileges)
	if !resourcePrivilegesEqual(privilegesSet, db, d) {
		d.Set("privileges", privilegesSet)
	}
	d.SetId(generateDefaultPrivilegesID(d))

	return nil
}

// defaultACLBuiltinType returns the acldefault object type of a default
// privileges object type. It is the pg_default_acl.defaclobjtype of the object
// type except for sequences: 'S' there, 's' for acldefault ('S' is a foreign
// server).
func defaultACLBuiltinType(objectType string) string {
	if t := objectTypes[objectType]; t != "S" {
		return t
	}
	return "s"
}

// builtinDefaultPrivileges returns the privileges acldefault(type, owner)
// grants to the grantee of a default privileges resource, as the direct Read
// gets them from the server: every privilege of the object type for the owner,
// EXECUTE on functions and USAGE on types for PUBLIC, nothing for other roles.
func builtinDefaultPrivileges(db *DBConnection, objectType string, isOwner, isPublic bool) []string {
	var privileges []string
	switch {
	case isOwner:
		for _, p := range allowedPrivilegesForObjectType(objectType, db) {
			if p != "ALL" {
				privileges = append(privileges, p)
			}
		}
	case isPublic && (objectType == "function" || objectType == "routine"):
		privileges = []string{"EXECUTE"}
	case isPublic && objectType == "type":
		privileges = []string{"USAGE"}
	}
	return privileges
}

func grantRoleDefaultPrivileges(txn *sql.Tx, d *schema.ResourceData) error {
	role := d.Get("role").(string)
	pgSchema := d.Get("schema").(string)

	privileges := []string{}
	for _, priv := range d.Get("privileges").(*schema.Set).List() {
		privileges = append(privileges, priv.(string))
	}

	if len(privileges) == 0 {
		log.Printf("[DEBUG] no default privileges to grant for role %s, owner %s in database: %s,", d.Get("role").(string), d.Get("owner").(string), d.Get("database").(string))
		return nil
	}

	var inSchema string

	// If a schema is specified we need to build the part of the query string to action this
	if pgSchema != "" {
		inSchema = fmt.Sprintf("IN SCHEMA %s", pq.QuoteIdentifier(pgSchema))
	}

	query := fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s %s GRANT %s ON %sS TO %s",
		pq.QuoteIdentifier(d.Get("owner").(string)),
		inSchema,
		strings.Join(privileges, ","),
		strings.ToUpper(d.Get("object_type").(string)),
		pq.QuoteIdentifier(role),
	)

	if d.Get("with_grant_option").(bool) {
		query = query + " WITH GRANT OPTION"
	}

	_, err := txn.Exec(
		query,
	)
	if err != nil {
		return fmt.Errorf("could not alter default privileges: %w", err)
	}

	return nil
}

// restoreBuiltinDefaultPrivileges grants back, after the REVOKE ALL of a
// destroy, the built-in defaults (acldefault) a global entry gives the role:
// everything for the owner itself, EXECUTE on functions and USAGE on types for
// PUBLIC. Without it the destroy leaves the owner or PUBLIC with fewer
// privileges on new objects than before the resource existed, and the
// pg_default_acl row it leaves behind blocks DROP ROLE of the owner.
// PostgreSQL deletes a global entry once it equals acldefault again. Per-schema
// entries only add to the global one, so they have nothing to restore.
func restoreBuiltinDefaultPrivileges(db *DBConnection, txn *sql.Tx, d *schema.ResourceData) error {
	if d.Get("schema").(string) != "" {
		return nil
	}
	role := d.Get("role").(string)
	owner := d.Get("owner").(string)
	objectType := d.Get("object_type").(string)
	privileges := builtinDefaultPrivileges(db, objectType, role == owner, role == publicRole)
	if len(privileges) == 0 {
		return nil
	}

	query := fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s GRANT %s ON %sS TO %s",
		pq.QuoteIdentifier(owner),
		strings.Join(privileges, ","),
		strings.ToUpper(objectType),
		pq.QuoteIdentifier(role),
	)
	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not restore the built-in default privileges: %w", err)
	}
	return nil
}

func revokeRoleDefaultPrivileges(txn *sql.Tx, d *schema.ResourceData) error {
	pgSchema := d.Get("schema").(string)

	var inSchema string

	// If a schema is specified we need to build the part of the query string to action this
	if pgSchema != "" {
		inSchema = fmt.Sprintf("IN SCHEMA %s", pq.QuoteIdentifier(pgSchema))
	}
	query := fmt.Sprintf(
		"ALTER DEFAULT PRIVILEGES FOR ROLE %s %s REVOKE ALL ON %sS FROM %s",
		pq.QuoteIdentifier(d.Get("owner").(string)),
		inSchema,
		strings.ToUpper(d.Get("object_type").(string)),
		pq.QuoteIdentifier(d.Get("role").(string)),
	)

	if _, err := txn.Exec(query); err != nil {
		return fmt.Errorf("could not revoke default privileges: %w", err)
	}
	return nil
}

func generateDefaultPrivilegesID(d *schema.ResourceData) string {
	pgSchema := d.Get("schema").(string)
	if pgSchema == "" {
		pgSchema = "noschema"
	}

	return strings.Join([]string{
		d.Get("role").(string), d.Get("database").(string), pgSchema,
		d.Get("owner").(string), d.Get("object_type").(string),
	}, "_")

}
