package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/lib/pq"
)

const (
	securityLabelDatabaseAttr   = "database"
	securityLabelObjectNameAttr = "object_name"
	securityLabelObjectTypeAttr = "object_type"
	securityLabelProviderAttr   = "label_provider"
	securityLabelLabelAttr      = "label"
)

// securityLabelNameStyle tells how the object_name of a security label is written
// in the configuration, and so how it is quoted in the SECURITY LABEL statement.
type securityLabelNameStyle int

const (
	// [schema.]name, each part is quoted (tables, types, ...)
	securityLabelNameQualified securityLabelNameStyle = iota
	// [schema.]name[(argument types)]: the name is quoted, the argument types are SQL type names
	securityLabelNameRoutine
	// [schema.]table.column: the table is quoted, the column is quoted
	securityLabelNameColumn
	// a single name which is quoted as a whole (roles, databases, ...)
	securityLabelNameSimple
	// the OID of a large object
	securityLabelNameOID
)

// securityLabelObjectType describes how an object type is found back in the catalogs.
type securityLabelObjectType struct {
	style securityLabelNameStyle
	// catalog is the catalog of the objects, as stored in pg_seclabels.classoid
	catalog string
	// lookup is a query which returns the OID and the objsubid of the object.
	// $1 is the name as written in the statement, $2 the name as stored in the catalog.
	lookup string
}

const (
	securityLabelLookupRelation = `SELECT to_regclass($1)::oid, 0`
	securityLabelLookupType     = `SELECT to_regtype($1)::oid, 0`
	securityLabelLookupRoutine  = `SELECT to_regprocedure($1)::oid, 0`
	// a routine named without argument types
	securityLabelLookupRoutineName = `SELECT to_regproc($1)::oid, 0`
	securityLabelLookupColumn      = `SELECT attrelid, attnum::int FROM pg_attribute WHERE attrelid = to_regclass($1) AND attname = $2`
	securityLabelLookupOID         = `SELECT $1::oid, 0`
)

// securityLabelObjectTypes lists the object types of the SECURITY LABEL statement
// of PostgreSQL 11 and above.
var securityLabelObjectTypes = map[string]securityLabelObjectType{
	"aggregate":           {style: securityLabelNameRoutine, catalog: "pg_proc", lookup: securityLabelLookupRoutine},
	"column":              {style: securityLabelNameColumn, catalog: "pg_class", lookup: securityLabelLookupColumn},
	"database":            {style: securityLabelNameSimple, catalog: "pg_database", lookup: `SELECT oid, 0 FROM pg_database WHERE datname = $2`},
	"domain":              {style: securityLabelNameQualified, catalog: "pg_type", lookup: securityLabelLookupType},
	"event trigger":       {style: securityLabelNameSimple, catalog: "pg_event_trigger", lookup: `SELECT oid, 0 FROM pg_event_trigger WHERE evtname = $2`},
	"foreign table":       {style: securityLabelNameQualified, catalog: "pg_class", lookup: securityLabelLookupRelation},
	"function":            {style: securityLabelNameRoutine, catalog: "pg_proc", lookup: securityLabelLookupRoutine},
	"large object":        {style: securityLabelNameOID, catalog: "pg_largeobject", lookup: securityLabelLookupOID},
	"language":            {style: securityLabelNameSimple, catalog: "pg_language", lookup: `SELECT oid, 0 FROM pg_language WHERE lanname = $2`},
	"materialized view":   {style: securityLabelNameQualified, catalog: "pg_class", lookup: securityLabelLookupRelation},
	"procedural language": {style: securityLabelNameSimple, catalog: "pg_language", lookup: `SELECT oid, 0 FROM pg_language WHERE lanname = $2`},
	"procedure":           {style: securityLabelNameRoutine, catalog: "pg_proc", lookup: securityLabelLookupRoutine},
	"publication":         {style: securityLabelNameSimple, catalog: "pg_publication", lookup: `SELECT oid, 0 FROM pg_publication WHERE pubname = $2`},
	"role":                {style: securityLabelNameSimple, catalog: "pg_authid", lookup: `SELECT oid, 0 FROM pg_roles WHERE rolname = $2`},
	"routine":             {style: securityLabelNameRoutine, catalog: "pg_proc", lookup: securityLabelLookupRoutine},
	"schema":              {style: securityLabelNameSimple, catalog: "pg_namespace", lookup: `SELECT oid, 0 FROM pg_namespace WHERE nspname = $2`},
	"sequence":            {style: securityLabelNameQualified, catalog: "pg_class", lookup: securityLabelLookupRelation},
	"subscription":        {style: securityLabelNameSimple, catalog: "pg_subscription", lookup: `SELECT oid, 0 FROM pg_subscription WHERE subname = $2`},
	"table":               {style: securityLabelNameQualified, catalog: "pg_class", lookup: securityLabelLookupRelation},
	"tablespace":          {style: securityLabelNameSimple, catalog: "pg_tablespace", lookup: `SELECT oid, 0 FROM pg_tablespace WHERE spcname = $2`},
	"type":                {style: securityLabelNameQualified, catalog: "pg_type", lookup: securityLabelLookupType},
	"view":                {style: securityLabelNameQualified, catalog: "pg_class", lookup: securityLabelLookupRelation},
}

var securityLabelObjectTypeNames = func() []string {
	names := make([]string, 0, len(securityLabelObjectTypes))
	for name := range securityLabelObjectTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}()

func resourcePostgreSQLSecurityLabel() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceFunc(resourcePostgreSQLSecurityLabelCreate),
		Read:   PGResourceFunc(resourcePostgreSQLSecurityLabelRead),
		Update: PGResourceFunc(resourcePostgreSQLSecurityLabelUpdate),
		Delete: PGResourceFunc(resourcePostgreSQLSecurityLabelDelete),
		Importer: &schema.ResourceImporter{
			StateContext: resourcePostgreSQLSecurityLabelImport,
		},

		Schema: map[string]*schema.Schema{
			securityLabelDatabaseAttr: {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
				Description: "The database in which the labelled object exists. Defaults to the database of the provider",
			},
			securityLabelObjectNameAttr: {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				Description: "The name of the existing object to apply the security label to. " +
					"Schema-qualified names are written schema.name, columns table.column (or schema.table.column), " +
					"functions and procedures name(argument types) and large objects their OID",
			},
			securityLabelObjectTypeAttr: {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringInSlice(securityLabelObjectTypeNames, false),
				Description:  "The type of the existing object to apply the security label to",
			},
			securityLabelProviderAttr: {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
				Description: "The provider to apply the security label for. Can be omitted when only one provider is loaded",
			},
			securityLabelLabelAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    false,
				Description: "The label to be applied",
			},
		},
	}
}

func resourcePostgreSQLSecurityLabelCreate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Create")
	label := d.Get(securityLabelLabelAttr).(string)
	if err := resourcePostgreSQLSecurityLabelUpdateImpl(db, d, pq.QuoteLiteral(label)); err != nil {
		return err
	}

	d.SetId(generateSecurityLabelID(d))

	return resourcePostgreSQLSecurityLabelReadImpl(db, d)
}

// resourcePostgreSQLSecurityLabelUpdateImpl sets the label (a SQL literal, or NULL to remove it)
func resourcePostgreSQLSecurityLabelUpdateImpl(db *DBConnection, d *schema.ResourceData, label string) error {
	objectType := d.Get(securityLabelObjectTypeAttr).(string)
	objectName := d.Get(securityLabelObjectNameAttr).(string)
	provider := d.Get(securityLabelProviderAttr).(string)
	database := getDatabaseForSecurityLabel(d, db.client.databaseName)

	name, _, _, err := securityLabelNames(securityLabelObjectTypes[objectType], objectName)
	if err != nil {
		return err
	}

	statement := "SECURITY LABEL "
	if provider != "" {
		statement += "FOR " + pq.QuoteIdentifier(provider) + " "
	}
	statement += "ON " + strings.ToUpper(objectType) + " " + name + " IS " + label

	txn, err := startTransaction(db.client, database)
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(statement); err != nil {
		log.Printf("[WARN] PostgreSQL security label Create failed %s", err)
		return fmt.Errorf("could not create security label: %w", err)
	}
	return txn.Commit()
}

func resourcePostgreSQLSecurityLabelRead(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Read")

	return resourcePostgreSQLSecurityLabelReadImpl(db, d)
}

func resourcePostgreSQLSecurityLabelReadImpl(db *DBConnection, d *schema.ResourceData) error {
	objectType := d.Get(securityLabelObjectTypeAttr).(string)
	objectName := d.Get(securityLabelObjectNameAttr).(string)
	provider := d.Get(securityLabelProviderAttr).(string)
	database := getDatabaseForSecurityLabel(d, db.client.databaseName)

	t, found := securityLabelObjectTypes[objectType]
	if !found {
		return fmt.Errorf("unsupported security label object type %q", objectType)
	}
	_, lookupName, rawName, err := securityLabelNames(t, objectName)
	if err != nil {
		return err
	}
	lookupQuery := t.lookup
	if t.style == securityLabelNameRoutine {
		if _, signature := splitRoutineName(objectName); signature == "" {
			lookupQuery = securityLabelLookupRoutineName
		}
	}

	// The labels of the shared objects (roles, databases, ...) are in pg_shseclabel: the
	// pg_seclabels view lists both kinds, and the objects are matched by their OID.
	// An empty provider matches the label of any provider.
	lookup := strings.NewReplacer("$1", pq.QuoteLiteral(lookupName), "$2", pq.QuoteLiteral(rawName)).Replace(lookupQuery)
	query := fmt.Sprintf(`
WITH object (oid, subid) AS (%s)
SELECT l.provider, l.label
  FROM pg_seclabels l
  JOIN object o ON l.objoid = o.oid AND l.objsubid = o.subid
 WHERE l.classoid = %s AND ($1 = '' OR l.provider = $1)`,
		lookup, pq.QuoteLiteral(t.catalog)+"::regclass")

	dbc, err := initConnection(db.client, database)
	if err != nil {
		if isDatabaseDoesNotExistError(err) {
			log.Printf("[WARN] PostgreSQL database (%s) not found, removing security label from state", database)
			d.SetId("")
			return nil
		}
		return err
	}

	var label, newProvider string
	err = dbc.QueryRow(query, provider).Scan(&newProvider, &label)
	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL security label for (%s '%s') with provider %s not found", objectType, objectName, provider)
		d.SetId("")
		return nil
	case isDatabaseDoesNotExistError(err):
		log.Printf("[WARN] PostgreSQL database (%s) not found, removing security label from state", database)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading security label: %w", err)
	}

	d.Set(securityLabelDatabaseAttr, database)
	d.Set(securityLabelProviderAttr, newProvider)
	d.Set(securityLabelLabelAttr, label)
	d.SetId(generateSecurityLabelID(d))

	return nil
}

func resourcePostgreSQLSecurityLabelDelete(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Delete")

	if err := resourcePostgreSQLSecurityLabelUpdateImpl(db, d, "NULL"); err != nil {
		return err
	}

	d.SetId("")

	return nil
}

func resourcePostgreSQLSecurityLabelUpdate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureSecurityLabel) {
		return fmt.Errorf(
			"security Label is not supported for this Postgres version (%s)",
			db.version,
		)
	}
	log.Printf("[DEBUG] PostgreSQL security label Update")

	label := d.Get(securityLabelLabelAttr).(string)
	if err := resourcePostgreSQLSecurityLabelUpdateImpl(db, d, pq.QuoteLiteral(label)); err != nil {
		return err
	}

	return resourcePostgreSQLSecurityLabelReadImpl(db, d)
}

// resourcePostgreSQLSecurityLabelImport reads the ID written by generateSecurityLabelID:
// <database>.<provider>.<object type>.<object name>. A database or a provider whose name
// contains a dot or a double quote is written between double quotes (a double quote inside
// is doubled), the other names are written as they are. The object type has no dot, and
// the object name is the rest of the ID, so it can contain dots.
func resourcePostgreSQLSecurityLabelImport(_ context.Context, d *schema.ResourceData, _ any) ([]*schema.ResourceData, error) {
	parts, err := parseSecurityLabelID(d.Id())
	if err != nil {
		return nil, err
	}
	d.Set(securityLabelDatabaseAttr, parts[0])
	d.Set(securityLabelProviderAttr, parts[1])
	// The object types are lower case in the configuration (validated) and in
	// the catalog; accept them in any case in the ID.
	d.Set(securityLabelObjectTypeAttr, strings.ToLower(parts[2]))
	d.Set(securityLabelObjectNameAttr, parts[3])

	return []*schema.ResourceData{d}, nil
}

func parseSecurityLabelID(id string) ([4]string, error) {
	var parts [4]string
	rest := id
	for i := 0; i < 3; i++ {
		var part string
		var ok bool
		if i < 2 && strings.HasPrefix(rest, `"`) {
			part, rest, ok = cutQuotedIDPart(rest)
		} else {
			part, rest, ok = strings.Cut(rest, ".")
		}
		if !ok {
			return parts, fmt.Errorf("invalid security label ID %q, expected <database>.<provider>.<object type>.<object name>", id)
		}
		parts[i] = part
	}
	parts[3] = rest

	return parts, nil
}

// cutQuotedIDPart reads a "quoted" part and the dot which follows it.
func cutQuotedIDPart(s string) (part, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '.' {
			return b.String(), s[i+2:], true
		}
		return "", "", false
	}
	return "", "", false
}

// quoteSecurityLabelIDPart quotes a part of the ID which contains a dot or a double quote.
func quoteSecurityLabelIDPart(part string) string {
	if !strings.ContainsAny(part, `."`) {
		return part
	}
	return `"` + strings.ReplaceAll(part, `"`, `""`) + `"`
}

func getDatabaseForSecurityLabel(d *schema.ResourceData, databaseName string) string {
	if v, ok := d.GetOk(securityLabelDatabaseAttr); ok {
		databaseName = v.(string)
	}

	return databaseName
}

func generateSecurityLabelID(d *schema.ResourceData) string {
	return strings.Join([]string{
		quoteSecurityLabelIDPart(d.Get(securityLabelDatabaseAttr).(string)),
		quoteSecurityLabelIDPart(d.Get(securityLabelProviderAttr).(string)),
		d.Get(securityLabelObjectTypeAttr).(string),
		d.Get(securityLabelObjectNameAttr).(string),
	}, ".")
}

// securityLabelNames returns the name of the object as written in the SECURITY LABEL
// statement, the name to look the object up with (the same name, except for a column
// which is looked up by its table) and the name as stored in the catalog (used by the
// catalogs which are not schema-qualified, and for the column name of a column).
func securityLabelNames(t securityLabelObjectType, objectName string) (name, lookupName, rawName string, err error) {
	switch t.style {
	case securityLabelNameSimple:
		quoted := pq.QuoteIdentifier(objectName)
		return quoted, quoted, objectName, nil
	case securityLabelNameOID:
		oid, err := strconv.ParseUint(objectName, 10, 32)
		if err != nil {
			return "", "", "", fmt.Errorf("the name of a large object must be its OID: %w", err)
		}
		return strconv.FormatUint(oid, 10), strconv.FormatUint(oid, 10), objectName, nil
	case securityLabelNameColumn:
		parts := strings.Split(objectName, ".")
		if len(parts) < 2 {
			return "", "", "", fmt.Errorf("the name of a column must be table.column, got %q", objectName)
		}
		table := quoteQualifiedName(strings.Join(parts[:len(parts)-1], "."))
		column := parts[len(parts)-1]
		return table + "." + pq.QuoteIdentifier(column), table, column, nil
	case securityLabelNameRoutine:
		qualified, signature := splitRoutineName(objectName)
		quoted := quoteQualifiedName(qualified) + signature
		return quoted, quoted, objectName, nil
	default:
		quoted := quoteQualifiedName(objectName)
		return quoted, quoted, objectName, nil
	}
}

// splitRoutineName splits name(argument types) into the name and the list of argument
// types, with its parentheses. The list is the first parenthesised group which is balanced
// up to the end of the string: the argument types can contain parentheses (varchar(10),
// numeric(10,2)) and the name itself can contain one. Without such a group, the whole
// string is the name.
func splitRoutineName(objectName string) (name, signature string) {
	for i := 0; i < len(objectName); i++ {
		if objectName[i] == '(' && balancedGroup(objectName[i:]) {
			return objectName[:i], objectName[i:]
		}
	}
	return objectName, ""
}

// balancedGroup tells if s is one group "(...)" whose parentheses are balanced, the
// parentheses inside double quotes being ignored.
func balancedGroup(s string) bool {
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i == len(s)-1
			}
		}
	}
	return false
}

// quoteQualifiedName quotes each part of a name written as schema.name
func quoteQualifiedName(name string) string {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		parts[i] = pq.QuoteIdentifier(part)
	}
	return strings.Join(parts, ".")
}
