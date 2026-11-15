package postgresql

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/lib/pq"
)

const (
	funcNameAttr            = "name"
	funcSchemaAttr          = "schema"
	funcBodyAttr            = "body"
	funcArgAttr             = "arg"
	funcLanguageAttr        = "language"
	funcReturnsAttr         = "returns"
	funcDropCascadeAttr     = "drop_cascade"
	funcDatabaseAttr        = "database"
	funcParallelAttr        = "parallel"
	funcSecurityDefinerAttr = "security_definer"
	funcStrictAttr          = "strict"
	funcVolatilityAttr      = "volatility"

	funcArgTypeAttr    = "type"
	funcArgNameAttr    = "name"
	funcArgModeAttr    = "mode"
	funcArgDefaultAttr = "default"

	defaultFunctionVolatility = "VOLATILE"
	defaultFunctionParallel   = "UNSAFE"
)

func resourcePostgreSQLFunction() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceWriteFunc(resourcePostgreSQLFunctionCreate),
		Read:   PGResourceFunc(resourcePostgreSQLFunctionRead),
		Update: PGResourceWriteFunc(resourcePostgreSQLFunctionUpdate),
		Delete: PGResourceWriteFunc(resourcePostgreSQLFunctionDelete),
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			funcSchemaAttr: {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
				Description: "Schema where the function is located. If not specified, the provider default schema is used.",

				DiffSuppressFunc: defaultDiffSuppressFunc,
			},
			funcNameAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "Name of the function.",
			},
			funcArgAttr: {
				Type: schema.TypeList,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						funcArgTypeAttr: {
							Type:        schema.TypeString,
							Description: "The argument type.",
							Required:    true,
							ForceNew:    true,
						},
						funcArgNameAttr: {
							Type:        schema.TypeString,
							Description: "The argument name. The name may be required for some languages or depending on the argument mode.",
							Optional:    true,
							ForceNew:    true,
						},
						funcArgModeAttr: {
							Type:        schema.TypeString,
							Description: "The argument mode. One of: IN, OUT, INOUT, or VARIADIC",
							Optional:    true,
							Default:     "IN",
							ForceNew:    true,

							DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
								if (new == "" && old == "IN") || (new == "IN" && old == "") {
									return true
								}
								return old == new
							},
						},
						funcArgDefaultAttr: {
							Type:        schema.TypeString,
							Description: "An expression to be used as default value if the parameter is not specified.",
							Optional:    true,
						},
					},
				},
				Optional:    true,
				ForceNew:    true,
				Description: "Function argument definitions.",
			},
			funcLanguageAttr: {
				Type:        schema.TypeString,
				Optional:    true,
				ForceNew:    true,
				Default:     "plpgsql",
				Description: "Language of the function. One of: internal, sql, c, plpgsql",

				DiffSuppressFunc: defaultDiffSuppressFunc,
			},
			funcReturnsAttr: {
				Type:        schema.TypeString,
				Optional:    true,
				ForceNew:    true,
				Computed:    true,
				Description: "Function return type. If not specified, it will be calculated based on the output arguments",

				DiffSuppressFunc: defaultDiffSuppressFunc,
			},
			funcBodyAttr: {
				Type:        schema.TypeString,
				Required:    true,
				Description: "Body of the function.",

				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					return normalizeFunctionBody(new) == old
				},
				StateFunc: func(val any) string {
					return normalizeFunctionBody(val.(string))
				},
			},
			funcDropCascadeAttr: {
				Type:        schema.TypeBool,
				Description: "Automatically drop objects that depend on the function (such as operators or triggers), and in turn all objects that depend on those objects.",
				Optional:    true,
				Default:     false,
			},
			funcParallelAttr: {
				Type:             schema.TypeString,
				Description:      "If the function can be executed in parallel for a single query execution. One of: UNSAFE, RESTRICTED, SAFE",
				Optional:         true,
				Default:          defaultFunctionParallel,
				DiffSuppressFunc: defaultDiffSuppressFunc,
				ValidateFunc:     validation.StringInSlice([]string{"UNSAFE", "RESTRICTED", "SAFE"}, false),
			},
			funcSecurityDefinerAttr: {
				Type:        schema.TypeBool,
				Description: "If the function should execute with the permissions of the function owner instead of the permissions of the caller.",
				Optional:    true,
				Default:     false,
			},
			funcStrictAttr: {
				Type:        schema.TypeBool,
				Description: "If the function should always return NULL if any of it's inputs is NULL.",
				Optional:    true,
				Default:     false,
			},
			funcVolatilityAttr: {
				Type:             schema.TypeString,
				Description:      "Volatility of the function. One of: VOLATILE, STABLE, IMMUTABLE.",
				Optional:         true,
				Default:          defaultFunctionVolatility,
				DiffSuppressFunc: defaultDiffSuppressFunc,
				ValidateFunc:     validation.StringInSlice([]string{"VOLATILE", "STABLE", "IMMUTABLE"}, false),
			},
			funcDatabaseAttr: {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
				Description: "The database where the function is located. If not specified, the provider default database is used.",

				DiffSuppressFunc: defaultDiffSuppressFunc,
			},
		},
	}
}

func resourcePostgreSQLFunctionCreate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureFunction) {
		return fmt.Errorf(
			"postgresql_function resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	if err := createFunction(db, d, false); err != nil {
		return err
	}

	return resourcePostgreSQLFunctionReadImpl(db, d)
}

func resourcePostgreSQLFunctionRead(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureFunction) {
		return fmt.Errorf(
			"postgresql_function resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	return resourcePostgreSQLFunctionReadImpl(db, d)
}

func resourcePostgreSQLFunctionReadImpl(db *DBConnection, d *schema.ResourceData) error {
	functionId := d.Id()

	if functionId == "" {
		// Generate during creation
		generatedFunctionId, err := generateFunctionID(db, d)
		if err != nil {
			return err
		}
		functionId = generatedFunctionId
	}

	databaseName, fn, err := functionRefFromID(functionId, d, db)
	if err != nil {
		return err
	}

	var (
		funcDefinition string
		funcSchema     string
		funcName       string
		funcArgs       []byte
	)

	query := `SELECT pg_get_functiondef(p.oid), n.nspname, p.proname, ` + functionArgsQuery + ` ` +
		`FROM pg_proc p ` +
		`JOIN pg_namespace n ON p.pronamespace = n.oid ` +
		`WHERE ` + functionMatch

	dbc, err := initConnection(db.client, databaseName)
	if err != nil {
		if isDatabaseDoesNotExistError(err) {
			log.Printf("[WARN] PostgreSQL database (%s) not found, removing function (%s) from state", databaseName, functionId)
			d.SetId("")
			return nil
		}
		return err
	}

	err = dbc.QueryRow(query, fn.schema, fn.name, pq.Array(fn.argTypes)).Scan(&funcDefinition, &funcSchema, &funcName, &funcArgs)
	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] PostgreSQL function: %s", functionId)
		d.SetId("")
		return nil
	case isDatabaseDoesNotExistError(err):
		log.Printf("[WARN] PostgreSQL database (%s) not found, removing function (%s) from state", databaseName, functionId)
		d.SetId("")
		return nil
	case isFunctionArgTypeMissingError(err):
		log.Printf("[WARN] PostgreSQL function %s: an argument type no longer exists, removing it from state", functionId)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading function: %w", err)
	}

	var pgFunction PGFunction

	err = pgFunction.Parse(funcDefinition)
	if err != nil {
		return err
	}
	// pg_get_functiondef quotes the schema and the name where needed.
	pgFunction.Schema = funcSchema
	pgFunction.Name = funcName
	pgFunction.Args, err = parseFunctionArgs(funcArgs)
	if err != nil {
		return err
	}

	// Keep the configured spelling of an argument type when it is the type
	// read back (numeric(10,2) for numeric, pg_catalog.text for text).
	configuredArgs, _ := d.Get(funcArgAttr).([]any)

	var args []map[string]any

	for i, a := range pgFunction.Args {
		argType := a.Type
		if i < len(configuredArgs) {
			if configured, ok := configuredArgs[i].(map[string]any); ok {
				if t, ok := configured[funcArgTypeAttr].(string); ok && sameFunctionArgType(t, a.Type) {
					argType = t
				}
			}
		}
		args = append(args, map[string]any{
			funcArgTypeAttr:    argType,
			funcArgNameAttr:    a.Name,
			funcArgModeAttr:    a.Mode,
			funcArgDefaultAttr: a.Default,
		})
	}

	d.Set(funcDatabaseAttr, databaseName)
	d.Set(funcNameAttr, pgFunction.Name)
	d.Set(funcSchemaAttr, pgFunction.Schema)
	d.Set(funcLanguageAttr, pgFunction.Language)
	d.Set(funcReturnsAttr, pgFunction.Returns)
	d.Set(funcBodyAttr, pgFunction.Body)
	d.Set(funcSecurityDefinerAttr, pgFunction.SecurityDefiner)
	d.Set(funcStrictAttr, pgFunction.Strict)
	d.Set(funcParallelAttr, pgFunction.Parallel)
	d.Set(funcVolatilityAttr, pgFunction.Volatility)
	d.Set(funcArgAttr, args)

	d.SetId(functionId)

	return nil
}

func resourcePostgreSQLFunctionDelete(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureFunction) {
		return fmt.Errorf(
			"postgresql_function resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	databaseName, functionSignature, err := expandFunctionID(d.Id(), d, db)
	if err != nil {
		return err
	}

	dropMode := "RESTRICT"
	if v, ok := d.GetOk(funcDropCascadeAttr); ok && v.(bool) {
		dropMode = "CASCADE"
	}

	sql := fmt.Sprintf("DROP FUNCTION IF EXISTS %s %s", functionSignature, dropMode)

	txn, err := startTransaction(db.client, databaseName)
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	// Dropping the function needs its owner, and USAGE on its schema to
	// resolve the name (#209).
	_, fn, err := functionRefFromID(d.Id(), d, db)
	if err != nil {
		return err
	}
	roles, err := functionOwnerRoles(txn, fn.schema, "USAGE", &fn)
	if err != nil {
		return err
	}
	if err := withRolesGranted(db, txn, roles, func() error {
		_, err := txn.Exec(sql)
		return err
	}); err != nil {
		return err
	}

	if err := commitTxn(txn); err != nil {
		return err
	}

	d.SetId("")

	return nil
}

func resourcePostgreSQLFunctionUpdate(db *DBConnection, d *schema.ResourceData) error {
	if !db.featureSupported(featureFunction) {
		return fmt.Errorf(
			"postgresql_function resource is not supported for this Postgres version (%s)",
			db.version,
		)
	}

	if err := createFunction(db, d, true); err != nil {
		return err
	}

	return resourcePostgreSQLFunctionReadImpl(db, d)
}

func createFunction(db *DBConnection, d *schema.ResourceData, replace bool) error {

	var pgFunction PGFunction
	err := pgFunction.FromResourceData(d)
	if err != nil {
		return err
	}

	b := bytes.NewBufferString("CREATE ")

	if replace {
		b.WriteString(" OR REPLACE ")
	}

	b.WriteString("FUNCTION ")

	fmt.Fprint(b, pq.QuoteIdentifier(pgFunction.Schema), ".")

	fmt.Fprint(b, pq.QuoteIdentifier(pgFunction.Name), " (")

	for i, arg := range pgFunction.Args {
		if i > 0 {
			b.WriteRune(',')
		}

		b.WriteString("\n    ")

		if arg.Mode != "" {
			fmt.Fprint(b, arg.Mode, " ")
		}

		if arg.Name != "" {
			fmt.Fprint(b, arg.Name, " ")
		}

		b.WriteString(arg.Type)

		if arg.Default != "" {
			fmt.Fprint(b, " DEFAULT ", arg.Default)
		}
	}

	if len(pgFunction.Args) > 0 {
		b.WriteRune('\n')
	}

	b.WriteString(")")

	fmt.Fprint(b, "\nRETURNS ", pgFunction.Returns)
	fmt.Fprint(b, "\nLANGUAGE ", pgFunction.Language)
	if pgFunction.Volatility != defaultFunctionVolatility {
		fmt.Fprint(b, "\n", pgFunction.Volatility)
	}
	if pgFunction.SecurityDefiner {
		fmt.Fprint(b, "\nSECURITY DEFINER")
	}
	if pgFunction.Parallel != defaultFunctionParallel {
		fmt.Fprint(b, "\nPARALLEL ", pgFunction.Parallel)
	}
	if pgFunction.Strict {
		fmt.Fprint(b, "\nSTRICT")
	}

	fmt.Fprint(b, "\nAS $function$", pgFunction.Body, "$function$;")

	sql := b.String()

	txn, err := startTransaction(db.client, d.Get(funcDatabaseAttr).(string))
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	// Creating the function needs CREATE on its schema (its owner), and
	// replacing it also needs the function's owner (#209).
	var existing *functionRef
	if replace {
		_, fn, err := functionRefFromID(d.Id(), d, db)
		if err != nil {
			return err
		}
		existing = &fn
	}
	roles, err := functionOwnerRoles(txn, pgFunction.Schema, "CREATE", existing)
	if err != nil {
		return err
	}
	if err := withRolesGranted(db, txn, roles, func() error {
		_, err := txn.Exec(sql)
		return err
	}); err != nil {
		return err
	}

	return commitTxn(txn)
}

// functionOwnerRoles returns the roles a non-superuser provider user has to
// take temporarily to write a function and does not act as already: the owner
// of schemaName when the user lacks schemaPrivilege on it (CREATE to create or
// replace, USAGE to resolve the name in DROP), and the owner of the existing
// function fn, to replace or drop it. fn can be nil. pg_database_owner, which
// cannot have explicit members, is resolved to the database owner. The
// function is still owned by the provider user, as it is for a superuser.
func functionOwnerRoles(txn *sql.Tx, schemaName, schemaPrivilege string, fn *functionRef) ([]string, error) {
	var owners []string
	var schemaOwner string
	var hasPrivilege bool
	err := txn.QueryRow(
		"SELECT pg_get_userbyid(nspowner), has_schema_privilege(oid, $2) FROM pg_namespace WHERE nspname = $1",
		schemaName, schemaPrivilege,
	).Scan(&schemaOwner, &hasPrivilege)
	switch {
	case err == sql.ErrNoRows:
		// The schema is gone, and the function with it (DROP ... IF EXISTS).
	case err != nil:
		return nil, fmt.Errorf("could not check %s privilege on schema %s: %w", schemaPrivilege, schemaName, err)
	case !hasPrivilege:
		owners = append(owners, schemaOwner)
	}
	if fn != nil {
		var owner string
		var actsAsOwner bool
		err := txn.QueryRow(
			"SELECT pg_get_userbyid(p.proowner), pg_has_role(p.proowner, 'USAGE') "+
				"FROM pg_proc p JOIN pg_namespace n ON p.pronamespace = n.oid WHERE "+functionMatch,
			fn.schema, fn.name, pq.Array(fn.argTypes),
		).Scan(&owner, &actsAsOwner)
		switch {
		case err == sql.ErrNoRows:
		case err != nil:
			return nil, fmt.Errorf("could not read the owner of function %s.%s: %w", fn.schema, fn.name, err)
		case !actsAsOwner:
			owners = append(owners, owner)
		}
	}

	owners, err = resolveOwners(txn, owners)
	if err != nil {
		return nil, err
	}
	var roles []string
	for _, owner := range owners {
		if owner != "" && !slices.Contains(roles, owner) {
			roles = append(roles, owner)
		}
	}
	return roles, nil
}

// functionMatch finds a function in pg_proc p joined to pg_namespace n by its
// schema ($1), name ($2) and argument types ($3, text[]). Unlike
// to_regprocedure it needs no USAGE on the schema of the function, which the
// provider user does not have when another role owns the schema (#209).
// proargtypes prints as space-separated OIDs; a type that does not exist
// prints as "-" and matches nothing.
const functionMatch = `n.nspname = $1 AND p.proname = $2 AND p.proargtypes::text = array_to_string(ARRAY(` +
	`SELECT to_regtype(t)::oid FROM unnest($3::text[]) WITH ORDINALITY AS a(t, i) ORDER BY i), ' ', '-')`

// functionRef is a function as its ID names it: schema, name and the types of
// its arguments (OUT arguments excluded), as written in the configuration.
type functionRef struct {
	schema, name string
	argTypes     []string
}

// functionRefFromID parses a function ID like expandFunctionID.
func functionRefFromID(functionId string, d *schema.ResourceData, db *DBConnection) (string, functionRef, error) {
	databaseName, schemaName, name, args, err := parseFunctionID(functionId)
	if err != nil {
		return "", functionRef{}, err
	}
	if databaseName == "" {
		clientDatabaseName := "postgres"
		if db != nil {
			clientDatabaseName = db.client.databaseName
		}
		databaseName = getDatabase(d, clientDatabaseName)
	}
	return databaseName, functionRef{schema: schemaName, name: name, argTypes: splitFunctionArgTypes(args)}, nil
}

// splitFunctionArgTypes splits the argument list of a function ID at the
// commas that are not inside parentheses or double quotes, e.g.
// "integer, numeric(10,2)".
func splitFunctionArgTypes(args string) []string {
	var types []string
	depth, quoted, start := 0, false, 0
	for i, c := range args {
		switch {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			types = append(types, strings.TrimSpace(args[start:i]))
			start = i + 1
		}
	}
	if last := strings.TrimSpace(args[start:]); last != "" || len(types) > 0 {
		types = append(types, last)
	}
	return types
}

func generateFunctionID(db *DBConnection, d *schema.ResourceData) (string, error) {

	b := bytes.NewBufferString("")

	if dbAttr, ok := d.GetOk(funcDatabaseAttr); ok {
		fmt.Fprint(b, dbAttr.(string), ".")
	} else {
		fmt.Fprint(b, db.client.databaseName, ".")
	}

	var pgFunction PGFunction
	err := pgFunction.FromResourceData(d)
	if err != nil {
		return "", err
	}

	fmt.Fprint(b, pgFunction.Schema, ".", pgFunction.Name, "(")

	argCount := 0

	for _, arg := range pgFunction.Args {
		mode := "IN"
		if arg.Mode != "" {
			mode = arg.Mode
		}

		if mode != "OUT" {
			if argCount > 0 {
				b.WriteRune(',')
			}

			b.WriteString(arg.Type)
			argCount += 1
		}
	}

	b.WriteRune(')')

	return b.String(), nil
}

// isFunctionArgTypeMissingError reports whether to_regprocedure failed because
// the schema or the type of an argument does not exist (any more). Before
// PostgreSQL 16 it raises instead of returning NULL: a function whose argument
// type was dropped, schema and all, is gone too.
func isFunctionArgTypeMissingError(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	return pqErr.Code == "3F000" || pqErr.Code == "42704" // invalid_schema_name, undefined_object
}

// parseFunctionID splits a function ID, database.schema.name(arguments) or
// schema.name(arguments), into its parts. Only the dots before the argument
// list separate the parts: argument types can be schema-qualified
// (public.citext). The database and the schema cannot contain a dot, the
// function name can (database.schema.my.func(integer)).
func parseFunctionID(functionId string) (database, schemaName, name, args string, err error) {
	open := strings.Index(functionId, "(")
	if open < 0 || !strings.HasSuffix(functionId, ")") {
		return "", "", "", "", fmt.Errorf("function ID %s has not the expected format 'database.schema.function_name(arguments)'", functionId)
	}
	args = functionId[open+1 : len(functionId)-1]

	parts := strings.Split(functionId[:open], ".")
	switch {
	case len(parts) == 2:
		schemaName, name = parts[0], parts[1]
	case len(parts) > 2:
		database, schemaName, name = parts[0], parts[1], strings.Join(parts[2:], ".")
	}
	if schemaName == "" || name == "" || (len(parts) > 2 && database == "") {
		return "", "", "", "", fmt.Errorf("function ID %s has not the expected format 'database.schema.function_name(arguments)'", functionId)
	}
	return database, schemaName, name, args, nil
}

func expandFunctionID(functionId string, d *schema.ResourceData, db *DBConnection) (databaseName string, functionSignature string, err error) {
	databaseName, schemaName, name, args, err := parseFunctionID(functionId)
	if err != nil {
		return "", "", err
	}

	if databaseName == "" {
		clientDatabaseName := "postgres"
		if db != nil {
			clientDatabaseName = db.client.databaseName
		}
		databaseName = getDatabase(d, clientDatabaseName)
	}

	return databaseName, fmt.Sprintf("%s.%s(%s)", pq.QuoteIdentifier(schemaName), pq.QuoteIdentifier(name), args), nil
}
