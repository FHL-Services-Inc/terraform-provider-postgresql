package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/lib/pq"
)

// postgresql_role_privileges manages every privilege one role holds in one
// database, for the schemas it lists, in a single resource instance: schema
// USAGE/CREATE, table and sequence privileges (named objects or every current
// object of the schema) and default privileges. One Read is one statement,
// one write is one transaction that applies the difference between the
// configuration and what the role actually holds.
//
// The pure part (expanding the configuration, computing the difference,
// rendering the statements) lives in acl_diff.go.

var defaultPrivilegesObjectTypes = []string{"table", "sequence", "function", "type"}

func rolePrivilegesObjectBlock(kind string) *schema.Resource {
	return &schema.Resource{
		Schema: map[string]*schema.Schema{
			"privileges": {
				Type:        schema.TypeSet,
				Required:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Set:         schema.HashString,
				Description: "The exact privileges the role holds on the objects of this block (ALL allowed); an empty set revokes them",
			},
			"objects": {
				Type:        schema.TypeSet,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Set:         schema.HashString,
				Description: fmt.Sprintf("The %ss of the schema this block applies to; empty means every current %s of the schema", kind, kind),
			},
			"with_grant_option": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
				Description: "Whether the role may grant these privileges to others",
			},
		},
	}
}

func resourcePostgreSQLRolePrivileges() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceFunc(resourcePostgreSQLRolePrivilegesCreate),
		Read:   PGResourceFunc(resourcePostgreSQLRolePrivilegesRead),
		Update: PGResourceFunc(resourcePostgreSQLRolePrivilegesCreate),
		Delete: PGResourceFunc(resourcePostgreSQLRolePrivilegesDelete),
		Importer: &schema.ResourceImporter{
			StateContext: resourcePostgreSQLRolePrivilegesImport,
		},
		CustomizeDiff: resourcePostgreSQLRolePrivilegesCustomizeDiff,

		Schema: map[string]*schema.Schema{
			"database": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The database the privileges are held in",
			},
			"role": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The role holding the privileges (\"public\" for PUBLIC)",
			},
			"exclusive": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
				Description: "Also revoke every privilege the role holds in the listed schemas that the configuration does not mention",
			},
			"schema": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "The privileges held in one schema; a schema block manages the schema-level USAGE/CREATE of the role",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:     schema.TypeString,
							Required: true,
						},
						"usage": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  false,
						},
						"create": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  false,
						},
						"tables": {
							Type:     schema.TypeList,
							Optional: true,
							Elem:     rolePrivilegesObjectBlock("table"),
						},
						"sequences": {
							Type:     schema.TypeList,
							Optional: true,
							Elem:     rolePrivilegesObjectBlock("sequence"),
						},
						"default_privileges": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "Default privileges the owner grants to the role on new objects of the schema",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"owner": {
										Type:     schema.TypeString,
										Required: true,
									},
									"object_type": {
										Type:         schema.TypeString,
										Required:     true,
										ValidateFunc: validation.StringInSlice(defaultPrivilegesObjectTypes, false),
									},
									"privileges": {
										Type:     schema.TypeSet,
										Required: true,
										Elem:     &schema.Schema{Type: schema.TypeString},
										Set:      schema.HashString,
									},
									"with_grant_option": {
										Type:     schema.TypeBool,
										Optional: true,
										Default:  false,
									},
								},
							},
						},
					},
				},
			},
			"drift": {
				Type:        schema.TypeList,
				Computed:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Description: "What the last refresh found different from the configuration, one entry per object (+PRIV held but not configured, -PRIV configured but missing)",
			},
		},
	}
}

// rolePrivilegesID is "<database>/<role>".
func rolePrivilegesID(database, role string) string {
	return database + "/" + role
}

func parseRolePrivilegesID(id string) (database, role string, err error) {
	i := strings.Index(id, "/")
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("invalid ID %q, want <database>/<role>", id)
	}
	return id[:i], id[i+1:], nil
}

func setToStrings(v any) []string {
	set, ok := v.(*schema.Set)
	if !ok {
		return nil
	}
	out := make([]string, 0, set.Len())
	for _, e := range set.List() {
		out = append(out, e.(string))
	}
	sort.Strings(out)
	return out
}

func parseObjectBlocks(v any) []objectPrivilegesConfig {
	var blocks []objectPrivilegesConfig
	for _, raw := range v.([]any) {
		b := raw.(map[string]any)
		blocks = append(blocks, objectPrivilegesConfig{
			privileges:  setToStrings(b["privileges"]),
			objects:     setToStrings(b["objects"]),
			grantOption: b["with_grant_option"].(bool),
		})
	}
	return blocks
}

// parseRolePrivilegesConfig reads the configuration blocks of the resource.
func parseRolePrivilegesConfig(get func(string) any) rolePrivilegesConfig {
	var cfg rolePrivilegesConfig
	for _, raw := range get("schema").([]any) {
		s := raw.(map[string]any)
		sc := schemaPrivilegesConfig{
			name:      s["name"].(string),
			usage:     s["usage"].(bool),
			create:    s["create"].(bool),
			tables:    parseObjectBlocks(s["tables"]),
			sequences: parseObjectBlocks(s["sequences"]),
		}
		for _, raw := range s["default_privileges"].([]any) {
			b := raw.(map[string]any)
			sc.defaults = append(sc.defaults, defaultPrivilegesConfig{
				owner:       b["owner"].(string),
				objectType:  b["object_type"].(string),
				privileges:  setToStrings(b["privileges"]),
				grantOption: b["with_grant_option"].(bool),
			})
		}
		cfg.schemas = append(cfg.schemas, sc)
	}
	return cfg
}

func (cfg rolePrivilegesConfig) schemaNames() []string {
	names := make([]string, 0, len(cfg.schemas))
	for _, s := range cfg.schemas {
		names = append(names, s.name)
	}
	return names
}

// validateRolePrivilegesConfig checks what can be checked without a
// connection: unique schema names and the privilege names per kind
// (MAINTAIN is accepted here and version-checked on apply).
func validateRolePrivilegesConfig(cfg rolePrivilegesConfig, role string) error {
	seen := map[string]bool{}
	for _, s := range cfg.schemas {
		if seen[s.name] {
			return fmt.Errorf("schema %q is listed twice", s.name)
		}
		seen[s.name] = true
		for kind, blocks := range map[string][]objectPrivilegesConfig{"table": s.tables, "sequence": s.sequences} {
			for _, b := range blocks {
				if err := checkPrivilegeNames(kind, b.privileges, true); err != nil {
					return fmt.Errorf("schema %q: %w", s.name, err)
				}
			}
		}
		for _, b := range s.defaults {
			if err := checkPrivilegeNames(b.objectType, b.privileges, true); err != nil {
				return fmt.Errorf("schema %q default_privileges for %s: %w", s.name, b.owner, err)
			}
			if b.grantOption && strings.EqualFold(role, publicRole) {
				return fmt.Errorf("schema %q: with_grant_option cannot be true for default privileges of role public", s.name)
			}
		}
	}
	return nil
}

func checkPrivilegeNames(kind string, privileges []string, allowMaintain bool) error {
	allowed := allowedPrivilegesForObjectType(kind, nil)
	for _, p := range privileges {
		if sliceContainsStr(allowed, p) || (allowMaintain && kind == "table" && p == "MAINTAIN") {
			continue
		}
		return fmt.Errorf("%s is not an allowed privilege for %ss", p, kind)
	}
	return nil
}

func resourcePostgreSQLRolePrivilegesCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ any) error {
	if err := validateRolePrivilegesConfig(parseRolePrivilegesConfig(d.Get), d.Get("role").(string)); err != nil {
		return err
	}
	// Drift found by the last refresh plans an update: the write path then
	// re-reads under the locks and applies exactly the difference. Marking the
	// attribute unknown is what makes the plan non-empty.
	if d.Id() != "" && len(d.Get("drift").([]any)) > 0 {
		return d.SetNewComputed("drift")
	}
	return nil
}

// roleACLQuery returns, for the role $1 and the schemas $2 (every user schema
// when NULL), one row per schema, relation (tables, views, materialized
// views, foreign and partitioned tables, sequences) and default-ACL entry of
// those schemas, with the privileges the role holds on it (NULL when none)
// and its owner. The first column reports whether the role exists; it is
// returned even when there is no object at all.
const roleACLQuery = grantReadRoleCTE + `,
s AS (
    SELECT n.oid, n.nspname, n.nspowner, n.nspacl
    FROM pg_namespace n
    WHERE CASE WHEN $2::name[] IS NULL THEN n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
               ELSE n.nspname = ANY($2::name[]) END
)
SELECT r.oid IS NOT NULL, o.kind, o.schema, o.name, o.owner, o.privileges
FROM r
LEFT JOIN LATERAL (
    SELECT 'schema' AS kind, s.nspname AS schema, ''::name AS name, ow.rolname AS owner,
           (SELECT array_agg(a.privilege_type || CASE WHEN a.is_grantable THEN '*' ELSE '' END)
            FROM aclexplode(s.nspacl) a WHERE a.grantee = r.oid) AS privileges
    FROM s JOIN pg_roles ow ON ow.oid = s.nspowner
  UNION ALL
    SELECT CASE WHEN c.relkind = 'S' THEN 'sequence' ELSE 'table' END, s.nspname, c.relname, ow.rolname,
           (SELECT array_agg(a.privilege_type || CASE WHEN a.is_grantable THEN '*' ELSE '' END)
            FROM aclexplode(c.relacl) a WHERE a.grantee = r.oid)
    FROM pg_class c
    JOIN s ON s.oid = c.relnamespace
    JOIN pg_roles ow ON ow.oid = c.relowner
    WHERE c.relkind IN ('r', 'v', 'm', 'f', 'p', 'S')
  UNION ALL
    SELECT 'default:' || CASE d.defaclobjtype WHEN 'r' THEN 'table' WHEN 'S' THEN 'sequence'
                                              WHEN 'f' THEN 'function' WHEN 'T' THEN 'type' ELSE d.defaclobjtype::text END,
           s.nspname, ow.rolname, ow.rolname,
           (SELECT array_agg(a.privilege_type || CASE WHEN a.is_grantable THEN '*' ELSE '' END)
            FROM aclexplode(d.defaclacl) a WHERE a.grantee = r.oid)
    FROM pg_default_acl d
    JOIN s ON s.oid = d.defaclnamespace
    JOIN pg_roles ow ON ow.oid = d.defaclrole
) o ON true
`

// roleACL is the result of roleACLQuery: what the role holds, and who owns
// each object (needed to grant the owners to a non-superuser connection).
type roleACL struct {
	roleExists bool
	actual     aclState
	owners     map[aclObject]string
}

func roleACLArgs(role string, schemas []string) []any {
	var schemasArg any
	if schemas != nil {
		schemasArg = pq.Array(schemas)
	}
	return []any{role, schemasArg}
}

func scanRoleACL(rows *sql.Rows) (*roleACL, error) {
	result := &roleACL{actual: aclState{}, owners: map[aclObject]string{}}
	seen := false
	for rows.Next() {
		var kind, schemaName, name, owner sql.NullString
		var privileges pq.StringArray
		if err := rows.Scan(&result.roleExists, &kind, &schemaName, &name, &owner, &privileges); err != nil {
			return nil, err
		}
		seen = true
		if !kind.Valid {
			continue // no object in the listed schemas: only the existence flag
		}
		obj := aclObject{kind: kind.String, schema: schemaName.String, name: name.String}
		result.actual[obj] = parsePrivileges(privileges)
		result.owners[obj] = owner.String
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !seen {
		return nil, fmt.Errorf("could not read privileges: query returned no row")
	}
	return result, nil
}

// readRoleACLTxn runs roleACLQuery inside a transaction (the write path).
func readRoleACLTxn(txn *sql.Tx, role string, schemas []string) (*roleACL, error) {
	rows, err := txn.Query(roleACLQuery, roleACLArgs(role, schemas)...)
	if err != nil {
		return nil, fmt.Errorf("could not read privileges: %w", err)
	}
	defer closeGrantReadRows(rows)
	return scanRoleACL(rows)
}

// readRoleACL runs roleACLQuery without a transaction (Read and import).
// A missing database removes the resource from state and returns nil.
func readRoleACL(db *DBConnection, d *schema.ResourceData, role string, schemas []string) (*roleACL, error) {
	rows, err := queryGrantRead(db, d, roleACLQuery, roleACLArgs(role, schemas)...)
	if err != nil {
		return nil, fmt.Errorf("could not read privileges: %w", err)
	}
	if rows == nil {
		return nil, nil
	}
	defer closeGrantReadRows(rows)
	return scanRoleACL(rows)
}

func allowedPrivilegesFor(db *DBConnection) func(string) []string {
	return func(kind string) []string {
		return allowedPrivilegesForObjectType(strings.TrimPrefix(kind, "default:"), db)
	}
}

// rolePrivilegesChanges expands the configuration against the actual state
// and returns the changes to apply. With remove, the desired privileges are
// empty on everything the configuration covers (Delete).
func rolePrivilegesChanges(db *DBConnection, cfg rolePrivilegesConfig, exclusive, remove bool, acl *roleACL) (changes []aclChange, problems []string) {
	desired, scope, problems := expandDesired(cfg, acl.actual, allowedPrivilegesFor(db))
	if exclusive {
		scope = exclusiveScope(desired, acl.actual)
	}
	if remove {
		desired = aclState{}
	}
	return diffACL(desired, acl.actual, scope), problems
}

func resourcePostgreSQLRolePrivilegesRead(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featurePrivileges) {
		return fmt.Errorf("postgresql_role_privileges is not supported for this Postgres version (%s)", db.version)
	}
	role := d.Get("role").(string)
	cfg := parseRolePrivilegesConfig(d.Get)

	acl, err := readRoleACL(db, d, role, cfg.schemaNames())
	if err != nil || acl == nil {
		return err
	}
	if !acl.roleExists {
		log.Printf("[DEBUG] role %s does not exist, removing %s from state", role, d.Id())
		d.SetId("")
		return nil
	}

	changes, problems := rolePrivilegesChanges(db, cfg, d.Get("exclusive").(bool), false, acl)
	drift := driftEntries(changes, problems)
	entries := make([]any, len(drift))
	for i, e := range drift {
		entries[i] = e
	}
	d.SetId(rolePrivilegesID(d.Get("database").(string), role))
	return d.Set("drift", entries)
}

func resourcePostgreSQLRolePrivilegesCreate(db *DBConnection, d *schema.ResourceData) error {
	if err := applyRolePrivileges(db, d, false); err != nil {
		return err
	}
	d.SetId(rolePrivilegesID(d.Get("database").(string), d.Get("role").(string)))
	return resourcePostgreSQLRolePrivilegesRead(db, d)
}

func resourcePostgreSQLRolePrivilegesDelete(db *DBConnection, d *schema.ResourceData) error {
	return applyRolePrivileges(db, d, true)
}

// lockSchemas takes a transaction-scoped advisory lock on every listed
// schema, in OID order, so that two instances for different roles touching
// the same schema serialize instead of failing with "tuple concurrently
// updated". The two-key form keeps these locks apart from the single-key
// role and database locks.
func lockSchemas(txn *sql.Tx, schemas []string) error {
	if len(schemas) == 0 {
		return nil
	}
	if _, err := txn.Exec(
		"SELECT pg_advisory_xact_lock('pg_namespace'::regclass::oid::int, oid::int) FROM pg_namespace WHERE nspname = ANY($1) ORDER BY oid",
		pq.Array(schemas),
	); err != nil {
		return fmt.Errorf("could not get advisory lock for schemas: %w", err)
	}
	return nil
}

// applyRolePrivileges brings the role's privileges to the configuration in
// one transaction: lock the role and the listed schemas, read what the role
// actually holds under those locks, compute the difference and send the
// resulting statements as one string. With remove, everything the
// configuration covers is revoked.
func applyRolePrivileges(db *DBConnection, d *schema.ResourceData, remove bool) error {
	if !db.featureSupported(featurePrivileges) {
		return fmt.Errorf("postgresql_role_privileges is not supported for this Postgres version (%s)", db.version)
	}
	database := d.Get("database").(string)
	role := d.Get("role").(string)
	cfg := parseRolePrivilegesConfig(d.Get)
	if err := validateRolePrivilegesConfig(cfg, role); err != nil {
		return err
	}
	for _, s := range cfg.schemas {
		for _, b := range s.tables {
			if sliceContainsStr(b.privileges, "MAINTAIN") && !db.featureSupported(featureMaintainPrivilege) {
				return fmt.Errorf("schema %q: MAINTAIN is not supported for this Postgres version (%s)", s.name, db.version)
			}
		}
	}
	schemas := cfg.schemaNames()

	txn, err := startTransaction(db.client, database)
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if err := pgLockRole(txn, role); err != nil {
		return err
	}
	if err := lockSchemas(txn, schemas); err != nil {
		return err
	}

	acl, err := readRoleACLTxn(txn, role, schemas)
	if err != nil {
		return err
	}
	if !acl.roleExists {
		return fmt.Errorf("role %s does not exist", role)
	}

	changes, problems := rolePrivilegesChanges(db, cfg, d.Get("exclusive").(bool), remove, acl)
	if len(problems) > 0 && !remove {
		return fmt.Errorf("configuration does not match the database: %s", strings.Join(problems, "; "))
	}
	stmts := renderACLStatements(role, changes, acl.actual)
	if len(stmts) == 0 {
		log.Printf("[DEBUG] privileges of role %s in database %s are already as configured", role, database)
		return txn.Commit()
	}

	// A non-superuser connection must act as the owner of every object it
	// changes privileges on; the owners come from the rows just read.
	ownerSet := map[string]bool{}
	for _, c := range changes {
		if owner := acl.owners[c.obj]; owner != "" {
			ownerSet[owner] = true
		}
	}
	owners := make([]string, 0, len(ownerSet))
	for o := range ownerSet {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	owners, err = resolveOwners(txn, owners)
	if err != nil {
		return err
	}

	if err := withRolesGranted(txn, owners, func() error {
		if _, err := txn.Exec(strings.Join(stmts, ";\n")); err != nil {
			return fmt.Errorf("could not apply privileges: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := txn.Commit(); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}
	return nil
}

// resourcePostgreSQLRolePrivilegesImport reads everything the role holds in
// every user schema of the database and lays it out as configuration blocks:
// one tables/sequences block per distinct (privileges, grant option) with
// explicit objects, one default_privileges block per owner and object type.
func resourcePostgreSQLRolePrivilegesImport(_ context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	database, role, err := parseRolePrivilegesID(d.Id())
	if err != nil {
		return nil, err
	}
	client := meta.(*Client)
	db, err := client.Connect()
	if err != nil {
		return nil, err
	}
	if err := d.Set("database", database); err != nil {
		return nil, err
	}
	if err := d.Set("role", role); err != nil {
		return nil, err
	}
	if err := d.Set("exclusive", false); err != nil {
		return nil, err
	}

	acl, err := readRoleACL(db, d, role, nil)
	if err != nil {
		return nil, err
	}
	if acl == nil {
		return nil, fmt.Errorf("database %s does not exist", database)
	}
	if !acl.roleExists {
		return nil, fmt.Errorf("role %s does not exist", role)
	}
	if err := d.Set("schema", importedSchemaBlocks(acl.actual)); err != nil {
		return nil, err
	}
	return []*schema.ResourceData{d}, nil
}

// privilegeGroups splits a privilege set by grant option and returns the
// sorted privilege lists, so that a block can be written per group.
func privilegeGroups(set privilegeSet) map[bool][]string {
	groups := map[bool][]string{}
	for p, grantable := range set {
		groups[grantable] = append(groups[grantable], p)
	}
	for _, list := range groups {
		sort.Strings(list)
	}
	return groups
}

func importedSchemaBlocks(actual aclState) []any {
	type blockKey struct {
		privileges string
		grantable  bool
	}
	schemas := map[string]map[string]any{}
	objectBlocks := map[string]map[string]map[blockKey][]string{} // schema -> kind -> block -> objects

	schemaBlock := func(name string) map[string]any {
		b, ok := schemas[name]
		if !ok {
			b = map[string]any{"name": name, "usage": false, "create": false,
				"tables": []any{}, "sequences": []any{}, "default_privileges": []any{}}
			schemas[name] = b
		}
		return b
	}

	for obj, set := range actual {
		if len(set) == 0 {
			continue
		}
		b := schemaBlock(obj.schema)
		switch {
		case obj.kind == "schema":
			_, b["usage"] = set["USAGE"]
			_, b["create"] = set["CREATE"]
		case strings.HasPrefix(obj.kind, "default:"):
			objectType := strings.TrimPrefix(obj.kind, "default:")
			if !sliceContainsStr(defaultPrivilegesObjectTypes, objectType) {
				continue
			}
			for grantable, privileges := range privilegeGroups(set) {
				b["default_privileges"] = append(b["default_privileges"].([]any), map[string]any{
					"owner": obj.name, "object_type": objectType,
					"privileges": stringsToAny(privileges), "with_grant_option": grantable,
				})
			}
		default:
			if objectBlocks[obj.schema] == nil {
				objectBlocks[obj.schema] = map[string]map[blockKey][]string{}
			}
			if objectBlocks[obj.schema][obj.kind] == nil {
				objectBlocks[obj.schema][obj.kind] = map[blockKey][]string{}
			}
			for grantable, privileges := range privilegeGroups(set) {
				key := blockKey{privileges: strings.Join(privileges, ","), grantable: grantable}
				objectBlocks[obj.schema][obj.kind][key] = append(objectBlocks[obj.schema][obj.kind][key], obj.name)
			}
		}
	}

	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]any, 0, len(names))
	for _, name := range names {
		b := schemas[name]
		for kind, attr := range map[string]string{"table": "tables", "sequence": "sequences"} {
			blocks := objectBlocks[name][kind]
			keys := make([]blockKey, 0, len(blocks))
			for k := range blocks {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if keys[i].grantable != keys[j].grantable {
					return !keys[i].grantable
				}
				return keys[i].privileges < keys[j].privileges
			})
			for _, k := range keys {
				objects := blocks[k]
				sort.Strings(objects)
				b[attr] = append(b[attr].([]any), map[string]any{
					"privileges":        stringsToAny(strings.Split(k.privileges, ",")),
					"objects":           stringsToAny(objects),
					"with_grant_option": k.grantable,
				})
			}
		}
		defaults := b["default_privileges"].([]any)
		sort.Slice(defaults, func(i, j int) bool {
			a, c := defaults[i].(map[string]any), defaults[j].(map[string]any)
			if a["owner"] != c["owner"] {
				return a["owner"].(string) < c["owner"].(string)
			}
			return a["object_type"].(string) < c["object_type"].(string)
		})
		result = append(result, b)
	}
	return result
}

func stringsToAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
