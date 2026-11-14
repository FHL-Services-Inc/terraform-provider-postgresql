package postgresql

// Importers for postgresql_grant, postgresql_default_privileges and
// postgresql_grant_role.
//
// The import ID is the resource ID as the provider writes it
// (generateGrantID, generateDefaultPrivilegesID, generateGrantRoleID): the
// attributes joined with "_". Role, database, schema and object names
// commonly contain "_" themselves, so the ID cannot be split syntactically.
// The importers enumerate every way to split it into the expected fields and
// keep the splits whose names exist in the catalog; an ID that matches no
// split, or more than one, is rejected with an error. The attributes that are
// not part of the ID are then filled by Read (privileges) or read here
// (with_grant_option, which Read does not refresh).

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// maxImportCandidates bounds the number of matches collected while parsing an
// import ID; we only need to know whether there is exactly one.
const maxImportCandidates = 10

// importCatalog answers the existence checks the import ID parsers need. Each
// method returns the subset of names that exist.
type importCatalog interface {
	existingRoles(names []string) (map[string]bool, error)
	existingDatabases(names []string) (map[string]bool, error)
	existingSchemas(database string, names []string) (map[string]bool, error)
	existingObjects(database, pgSchema, objectType string, names []string) (map[string]bool, error)
}

func joinIDTokens(tokens []string) string {
	return strings.Join(tokens, "_")
}

// idSubJoins returns every contiguous run of tokens joined with "_".
func idSubJoins(tokens []string) []string {
	out := []string{}
	for i := range tokens {
		for j := i + 1; j <= len(tokens); j++ {
			out = append(out, joinIDTokens(tokens[i:j]))
		}
	}
	return out
}

// importSegmentations returns the ways to split tokens into consecutive runs
// that all satisfy valid, up to limit results.
func importSegmentations(tokens []string, valid func(string) bool, limit int) [][]string {
	var out [][]string
	var walk func(start int, acc []string)
	walk = func(start int, acc []string) {
		if len(out) >= limit {
			return
		}
		if start == len(tokens) {
			out = append(out, append([]string(nil), acc...))
			return
		}
		for end := start + 1; end <= len(tokens); end++ {
			if name := joinIDTokens(tokens[start:end]); valid(name) {
				walk(end, append(acc, name))
			}
		}
	}
	if len(tokens) > 0 {
		walk(0, nil)
	}
	return out
}

// matchObjectType reports the object types of types the tokens start with and
// how many tokens each one takes (object types like foreign_data_wrapper
// contain "_" too).
func matchObjectType(tokens []string, types []string) map[string]int {
	out := map[string]int{}
	for _, t := range types {
		typeTokens := strings.Split(t, "_")
		if len(tokens) < len(typeTokens) {
			continue
		}
		if joinIDTokens(tokens[:len(typeTokens)]) == t {
			out[t] = len(typeTokens)
		}
	}
	return out
}

func importParseError(resource, id, format string, err error) error {
	return fmt.Errorf("could not import %s %q (expected %s): %w", resource, id, format, err)
}

func ambiguousImportError(candidates []string) error {
	return fmt.Errorf("ambiguous ID, it matches several existing objects: %s", strings.Join(candidates, "; "))
}

var errImportNoMatch = fmt.Errorf("the ID does not match existing roles, databases, schemas and objects")

// postgresql_grant

const grantImportFormat = "<role>_<database>_<object_type>[_<object>] for database, foreign_data_wrapper and foreign_server, " +
	"<role>_<database>_<schema>_<object_type>[_<object>...] otherwise"

var grantImportDatabaseLevelTypes = []string{"database", "foreign_data_wrapper", "foreign_server"}

var grantImportSchemaLevelTypes = []string{"schema", "table", "sequence", "function", "procedure", "routine", "column"}

type grantImportID struct {
	role, database, schema, objectType string
	objects                            []string
}

func (g grantImportID) String() string {
	s := fmt.Sprintf("role=%s database=%s", g.role, g.database)
	if g.schema != "" {
		s += " schema=" + g.schema
	}
	s += " object_type=" + g.objectType
	if len(g.objects) > 0 {
		s += " objects=" + strings.Join(g.objects, ",")
	}
	return s
}

func parseGrantImportID(id string, cat importCatalog) (grantImportID, error) {
	fail := func(err error) (grantImportID, error) {
		return grantImportID{}, importParseError("postgresql_grant", id, grantImportFormat, err)
	}

	tokens := strings.Split(id, "_")
	n := len(tokens)

	prefixes := []string{}
	for i := 1; i < n; i++ {
		prefixes = append(prefixes, joinIDTokens(tokens[:i]))
	}
	roles, err := cat.existingRoles(prefixes)
	if err != nil {
		return fail(err)
	}
	databases, err := cat.existingDatabases(idSubJoins(tokens[1:]))
	if err != nil {
		return fail(err)
	}

	// Candidates whose schema and objects still have to be checked in the
	// target database.
	type pending struct {
		grantImportID
		rest []string
	}
	var candidates []grantImportID
	var schemaLevel []pending
	sawColumn := false

	for i := 1; i < n; i++ {
		role := joinIDTokens(tokens[:i])
		if !roles[role] {
			continue
		}
		for j := i + 1; j < n; j++ {
			database := joinIDTokens(tokens[i:j])
			if !databases[database] {
				continue
			}
			rest := tokens[j:]

			for objectType, k := range matchObjectType(rest, grantImportDatabaseLevelTypes) {
				after := rest[k:]
				switch {
				case objectType == "database" && len(after) == 0:
					candidates = append(candidates, grantImportID{role: role, database: database, objectType: objectType})
				case objectType != "database" && len(after) > 0:
					candidates = append(candidates, grantImportID{
						role: role, database: database, objectType: objectType,
						objects: []string{joinIDTokens(after)},
					})
				}
			}

			for k := 1; k < len(rest); k++ {
				for objectType, m := range matchObjectType(rest[k:], grantImportSchemaLevelTypes) {
					if objectType == "column" {
						sawColumn = true
						continue
					}
					schemaLevel = append(schemaLevel, pending{
						grantImportID: grantImportID{
							role: role, database: database, schema: joinIDTokens(rest[:k]), objectType: objectType,
						},
						rest: rest[k+m:],
					})
				}
			}
		}
	}

	// Foreign data wrappers and servers must exist in the database.
	var checked []grantImportID
	for _, c := range candidates {
		if c.objectType == "database" {
			checked = append(checked, c)
			continue
		}
		ok, err := cat.existingObjects(c.database, "", c.objectType, c.objects)
		if err != nil {
			return fail(err)
		}
		if ok[c.objects[0]] {
			checked = append(checked, c)
		}
	}
	candidates = checked

	// Check the schemas, one lookup per database.
	schemasByDB := map[string][]string{}
	for _, p := range schemaLevel {
		schemasByDB[p.database] = append(schemasByDB[p.database], p.schema)
	}
	existingSchemas := map[string]map[string]bool{}
	for database, names := range schemasByDB {
		if existingSchemas[database], err = cat.existingSchemas(database, names); err != nil {
			return fail(err)
		}
	}

	for _, p := range schemaLevel {
		if !existingSchemas[p.database][p.schema] {
			continue
		}
		if p.objectType == "schema" {
			if len(p.rest) == 0 {
				candidates = append(candidates, p.grantImportID)
			}
			continue
		}
		if len(p.rest) == 0 {
			// No objects: the grant covers every object of this type in the schema.
			candidates = append(candidates, p.grantImportID)
			continue
		}
		objects, err := cat.existingObjects(p.database, p.schema, p.objectType, idSubJoins(p.rest))
		if err != nil {
			return fail(err)
		}
		for _, seg := range importSegmentations(p.rest, func(s string) bool { return objects[s] }, maxImportCandidates) {
			c := p.grantImportID
			c.objects = seg
			candidates = append(candidates, c)
		}
	}

	switch {
	case len(candidates) == 1:
		return candidates[0], nil
	case len(candidates) > 1:
		descr := make([]string, 0, len(candidates))
		for _, c := range candidates {
			descr = append(descr, c.String())
		}
		return fail(ambiguousImportError(descr))
	case sawColumn:
		return fail(fmt.Errorf("importing grants with object_type column is not supported: the ID does not identify the privilege"))
	default:
		return fail(errImportNoMatch)
	}
}

func resourcePostgreSQLGrantImport(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	client := meta.(*Client)
	db, err := client.Connect()
	if err != nil {
		return nil, err
	}

	g, err := parseGrantImportID(d.Id(), &sqlImportCatalog{db: db})
	if err != nil {
		return nil, err
	}

	withGrantOption, err := readImportedGrantOption(db, g)
	if err != nil {
		return nil, err
	}

	objects := make([]any, 0, len(g.objects))
	for _, o := range g.objects {
		objects = append(objects, o)
	}

	for k, v := range map[string]any{
		"role":              g.role,
		"database":          g.database,
		"schema":            g.schema,
		"object_type":       g.objectType,
		"objects":           objects,
		"with_grant_option": withGrantOption,
	} {
		if v == "" {
			// leave optional attributes unset, as in a config without them
			continue
		}
		if err := d.Set(k, v); err != nil {
			return nil, fmt.Errorf("could not set %s: %w", k, err)
		}
	}
	d.SetId(generateGrantID(d))

	return []*schema.ResourceData{d}, nil
}

// readImportedGrantOption reports whether the privileges of the imported
// grant were granted WITH GRANT OPTION (all of them, for a grant spanning
// several objects). Read does not refresh with_grant_option, and it forces a
// replacement, so it has to be right after import.
func readImportedGrantOption(db *DBConnection, g grantImportID) (bool, error) {
	conn, err := initConnection(db.client, g.database)
	if err != nil {
		return false, err
	}

	roleOID, err := getRoleOID(conn, g.role)
	if err != nil {
		return false, err
	}

	var query string
	args := []any{roleOID}

	switch g.objectType {
	case "database":
		query = `FROM pg_database d, aclexplode(d.datacl) a WHERE d.datname = $2`
		args = append(args, g.database)
	case "schema":
		query = `FROM pg_namespace n, aclexplode(n.nspacl) a WHERE n.nspname = $2`
		args = append(args, g.schema)
	case "foreign_data_wrapper":
		query = `FROM pg_foreign_data_wrapper f, aclexplode(f.fdwacl) a WHERE f.fdwname = $2`
		args = append(args, g.objects[0])
	case "foreign_server":
		query = `FROM pg_foreign_server s, aclexplode(s.srvacl) a WHERE s.srvname = $2`
		args = append(args, g.objects[0])
	case "function", "procedure", "routine":
		query = `FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace, aclexplode(p.proacl) a WHERE n.nspname = $2`
		args = append(args, g.schema)
		if len(g.objects) > 0 {
			// Read matches functions by name; listed objects may carry an
			// argument signature.
			names := make([]string, 0, len(g.objects))
			for _, o := range g.objects {
				names = append(names, functionImportBaseName(o))
			}
			query += ` AND p.proname = ANY($3)`
			args = append(args, pq.Array(names))
		}
	default: // table, sequence
		query = `FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace, aclexplode(c.relacl) a WHERE n.nspname = $2 AND c.relkind = $3`
		args = append(args, g.schema, objectTypes[g.objectType])
		if len(g.objects) > 0 {
			query += ` AND c.relname = ANY($4)`
			args = append(args, pq.Array(g.objects))
		}
	}

	var withGrantOption bool
	if err := conn.QueryRow(
		`SELECT coalesce(bool_and(a.is_grantable), false) `+query+` AND a.grantee = $1`, args...,
	).Scan(&withGrantOption); err != nil {
		return false, fmt.Errorf("could not read the grant option of %s: %w", g, err)
	}
	return withGrantOption, nil
}

// functionImportBaseName strips an argument signature: "f(int, text)" -> "f".
func functionImportBaseName(object string) string {
	if i := strings.Index(object, "("); i > 0 && strings.HasSuffix(object, ")") {
		return object[:i]
	}
	return object
}

// postgresql_default_privileges

const defaultPrivilegesImportFormat = "<role>_<database>_<schema|noschema>_<owner>_<object_type>"

var defaultPrivilegesImportTypes = []string{"table", "sequence", "function", "routine", "type", "schema"}

type defaultPrivilegesImportID struct {
	role, database, schema, owner, objectType string
}

func (p defaultPrivilegesImportID) String() string {
	pgSchema := p.schema
	if pgSchema == "" {
		pgSchema = "(none)"
	}
	return fmt.Sprintf("role=%s database=%s schema=%s owner=%s object_type=%s", p.role, p.database, pgSchema, p.owner, p.objectType)
}

func parseDefaultPrivilegesImportID(id string, cat importCatalog) (defaultPrivilegesImportID, error) {
	fail := func(err error) (defaultPrivilegesImportID, error) {
		return defaultPrivilegesImportID{}, importParseError("postgresql_default_privileges", id, defaultPrivilegesImportFormat, err)
	}

	tokens := strings.Split(id, "_")
	n := len(tokens)
	if n < 5 {
		return fail(errImportNoMatch)
	}
	objectType := tokens[n-1]
	if !sliceContainsStr(defaultPrivilegesImportTypes, objectType) {
		return fail(fmt.Errorf("unknown object type %q", objectType))
	}
	body := tokens[:n-1]
	n = len(body)

	roleNames := []string{}
	for i := 1; i < n; i++ {
		roleNames = append(roleNames, joinIDTokens(body[:i]), joinIDTokens(body[i:]))
	}
	roles, err := cat.existingRoles(roleNames)
	if err != nil {
		return fail(err)
	}
	databases, err := cat.existingDatabases(idSubJoins(body[1:]))
	if err != nil {
		return fail(err)
	}

	var pending []defaultPrivilegesImportID
	for i := 1; i < n; i++ {
		role := joinIDTokens(body[:i])
		if !roles[role] {
			continue
		}
		for j := i + 1; j < n; j++ {
			database := joinIDTokens(body[i:j])
			if !databases[database] {
				continue
			}
			for k := j + 1; k < n; k++ {
				owner := joinIDTokens(body[k:])
				if !roles[owner] || owner == publicRole {
					continue
				}
				pgSchema := joinIDTokens(body[j:k])
				if pgSchema == "noschema" {
					pgSchema = ""
				}
				pending = append(pending, defaultPrivilegesImportID{
					role: role, database: database, schema: pgSchema, owner: owner, objectType: objectType,
				})
			}
		}
	}

	schemasByDB := map[string][]string{}
	for _, p := range pending {
		if p.schema != "" {
			schemasByDB[p.database] = append(schemasByDB[p.database], p.schema)
		}
	}
	existingSchemas := map[string]map[string]bool{}
	for database, names := range schemasByDB {
		if existingSchemas[database], err = cat.existingSchemas(database, names); err != nil {
			return fail(err)
		}
	}

	var candidates []defaultPrivilegesImportID
	for _, p := range pending {
		if p.schema == "" || existingSchemas[p.database][p.schema] {
			candidates = append(candidates, p)
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return fail(errImportNoMatch)
	default:
		descr := make([]string, 0, len(candidates))
		for _, c := range candidates {
			descr = append(descr, c.String())
		}
		return fail(ambiguousImportError(descr))
	}
}

func resourcePostgreSQLDefaultPrivilegesImport(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	client := meta.(*Client)
	db, err := client.Connect()
	if err != nil {
		return nil, err
	}

	p, err := parseDefaultPrivilegesImportID(d.Id(), &sqlImportCatalog{db: db})
	if err != nil {
		return nil, err
	}

	withGrantOption, err := readImportedDefaultPrivilegesGrantOption(db, p)
	if err != nil {
		return nil, err
	}

	for k, v := range map[string]any{
		"role":              p.role,
		"database":          p.database,
		"schema":            p.schema,
		"owner":             p.owner,
		"object_type":       p.objectType,
		"with_grant_option": withGrantOption,
	} {
		if v == "" {
			// leave optional attributes unset, as in a config without them
			continue
		}
		if err := d.Set(k, v); err != nil {
			return nil, fmt.Errorf("could not set %s: %w", k, err)
		}
	}
	d.SetId(generateDefaultPrivilegesID(d))

	return []*schema.ResourceData{d}, nil
}

func readImportedDefaultPrivilegesGrantOption(db *DBConnection, p defaultPrivilegesImportID) (bool, error) {
	conn, err := initConnection(db.client, p.database)
	if err != nil {
		return false, err
	}

	roleOID, err := getRoleOID(conn, p.role)
	if err != nil {
		return false, err
	}

	var withGrantOption bool
	if err := conn.QueryRow(`
SELECT coalesce(bool_and(a.is_grantable), false)
FROM pg_default_acl d
JOIN pg_roles o ON o.oid = d.defaclrole
LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace,
aclexplode(d.defaclacl) a
WHERE a.grantee = $1 AND o.rolname = $2 AND d.defaclobjtype = $3
  AND CASE WHEN $4::text = '' THEN d.defaclnamespace = 0 ELSE n.nspname = $4::text END`,
		roleOID, p.owner, objectTypes[p.objectType], p.schema,
	).Scan(&withGrantOption); err != nil {
		return false, fmt.Errorf("could not read the grant option of default privileges %s: %w", p, err)
	}
	return withGrantOption, nil
}

// postgresql_grant_role

const grantRoleImportFormat = "<role>_<grant_role>_<with_admin_option: true|false>"

type grantRoleImportID struct {
	role, grantRole string
	withAdminOption bool
}

func parseGrantRoleImportID(id string, cat importCatalog) (grantRoleImportID, error) {
	fail := func(err error) (grantRoleImportID, error) {
		return grantRoleImportID{}, importParseError("postgresql_grant_role", id, grantRoleImportFormat, err)
	}

	tokens := strings.Split(id, "_")
	n := len(tokens)
	if n < 3 {
		return fail(errImportNoMatch)
	}

	var withAdminOption bool
	switch tokens[n-1] {
	case "true":
		withAdminOption = true
	case "false":
	default:
		return fail(fmt.Errorf("the last part must be true or false, got %q", tokens[n-1]))
	}
	body := tokens[:n-1]
	n = len(body)

	names := []string{}
	for i := 1; i < n; i++ {
		names = append(names, joinIDTokens(body[:i]), joinIDTokens(body[i:]))
	}
	roles, err := cat.existingRoles(names)
	if err != nil {
		return fail(err)
	}

	var candidates []grantRoleImportID
	for i := 1; i < n; i++ {
		role, grantRole := joinIDTokens(body[:i]), joinIDTokens(body[i:])
		if role == publicRole || grantRole == publicRole {
			continue
		}
		if roles[role] && roles[grantRole] {
			candidates = append(candidates, grantRoleImportID{role: role, grantRole: grantRole, withAdminOption: withAdminOption})
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return fail(errImportNoMatch)
	default:
		descr := make([]string, 0, len(candidates))
		for _, c := range candidates {
			descr = append(descr, fmt.Sprintf("role=%s grant_role=%s", c.role, c.grantRole))
		}
		return fail(ambiguousImportError(descr))
	}
}

func resourcePostgreSQLGrantRoleImport(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	client := meta.(*Client)
	db, err := client.Connect()
	if err != nil {
		return nil, err
	}

	g, err := parseGrantRoleImportID(d.Id(), &sqlImportCatalog{db: db})
	if err != nil {
		return nil, err
	}

	// with_admin_option is refreshed by Read.
	for k, v := range map[string]any{
		"role":              g.role,
		"grant_role":        g.grantRole,
		"with_admin_option": g.withAdminOption,
	} {
		if err := d.Set(k, v); err != nil {
			return nil, fmt.Errorf("could not set %s: %w", k, err)
		}
	}

	return []*schema.ResourceData{d}, nil
}

// sqlImportCatalog implements importCatalog with catalog queries. Lookups run
// one at a time without a transaction, so an import never holds more than one
// connection.
type sqlImportCatalog struct {
	db *DBConnection
}

func queryExistingNames(conn QueryAble, query string, args ...any) (map[string]bool, error) {
	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func closeRows(rows interface{ Close() error }) {
	if err := rows.Close(); err != nil {
		log.Printf("[WARN] could not close rows: %v", err)
	}
}

func (c *sqlImportCatalog) existingRoles(names []string) (map[string]bool, error) {
	out, err := queryExistingNames(c.db, `SELECT rolname FROM pg_roles WHERE rolname = ANY($1)`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("could not look up roles: %w", err)
	}
	if sliceContainsStr(names, publicRole) {
		out[publicRole] = true
	}
	return out, nil
}

func (c *sqlImportCatalog) existingDatabases(names []string) (map[string]bool, error) {
	out, err := queryExistingNames(c.db, `SELECT datname FROM pg_database WHERE datname = ANY($1)`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("could not look up databases: %w", err)
	}
	return out, nil
}

func (c *sqlImportCatalog) existingSchemas(database string, names []string) (map[string]bool, error) {
	conn, err := initConnection(c.db.client, database)
	if err != nil {
		return nil, err
	}
	out, err := queryExistingNames(conn, `SELECT nspname FROM pg_namespace WHERE nspname = ANY($1)`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("could not look up schemas in database %s: %w", database, err)
	}
	return out, nil
}

func (c *sqlImportCatalog) existingObjects(database, pgSchema, objectType string, names []string) (map[string]bool, error) {
	conn, err := initConnection(c.db.client, database)
	if err != nil {
		return nil, err
	}

	var out map[string]bool
	switch objectType {
	case "foreign_data_wrapper":
		out, err = queryExistingNames(conn, `SELECT fdwname FROM pg_foreign_data_wrapper WHERE fdwname = ANY($1)`, pq.Array(names))
	case "foreign_server":
		out, err = queryExistingNames(conn, `SELECT srvname FROM pg_foreign_server WHERE srvname = ANY($1)`, pq.Array(names))
	case "function", "procedure", "routine":
		baseNames := make([]string, 0, len(names))
		for _, name := range names {
			baseNames = append(baseNames, functionImportBaseName(name))
		}
		var existing map[string]bool
		existing, err = queryExistingNames(conn, `
SELECT proname FROM pg_proc JOIN pg_namespace n ON n.oid = pronamespace
WHERE nspname = $1 AND proname = ANY($2)`, pgSchema, pq.Array(baseNames))
		out = map[string]bool{}
		for _, name := range names {
			if existing[functionImportBaseName(name)] {
				out[name] = true
			}
		}
	default: // table, sequence
		relkinds := []string{"r", "p", "v", "m", "f"}
		if objectType == "sequence" {
			relkinds = []string{"S"}
		}
		out, err = queryExistingNames(conn, `
SELECT relname FROM pg_class JOIN pg_namespace n ON n.oid = relnamespace
WHERE nspname = $1 AND relkind::text = ANY($2) AND relname = ANY($3)`, pgSchema, pq.Array(relkinds), pq.Array(names))
	}
	if err != nil {
		return nil, fmt.Errorf("could not look up %s objects in database %s: %w", objectType, database, err)
	}
	return out, nil
}
