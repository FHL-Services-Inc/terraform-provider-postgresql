package postgresql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// PGFunction is the model for the database function
type PGFunction struct {
	Schema          string
	Name            string
	Returns         string
	Language        string
	Body            string
	Args            []PGFunctionArg
	Parallel        string
	SecurityDefiner bool
	Strict          bool
	Volatility      string
}

type PGFunctionArg struct {
	Name    string
	Type    string
	Mode    string
	Default string
}

func (pgFunction *PGFunction) FromResourceData(d *schema.ResourceData) error {

	if v, ok := d.GetOk(funcSchemaAttr); ok {
		pgFunction.Schema = v.(string)
	} else {
		pgFunction.Schema = "public"
	}

	pgFunction.Name = d.Get(funcNameAttr).(string)
	pgFunction.Returns = d.Get(funcReturnsAttr).(string)
	if v, ok := d.GetOk(funcLanguageAttr); ok {
		pgFunction.Language = v.(string)
	} else {
		pgFunction.Language = "plpgsql"
	}
	pgFunction.Body = normalizeFunctionBody(d.Get(funcBodyAttr).(string))
	pgFunction.Args = []PGFunctionArg{}

	if v, ok := d.GetOk(funcParallelAttr); ok {
		pgFunction.Parallel = v.(string)
	} else {
		pgFunction.Parallel = defaultFunctionParallel
	}
	if v, ok := d.GetOk(funcStrictAttr); ok {
		pgFunction.Strict = v.(bool)
	} else {
		pgFunction.Strict = false
	}
	if v, ok := d.GetOk(funcSecurityDefinerAttr); ok {
		pgFunction.SecurityDefiner = v.(bool)
	} else {
		pgFunction.SecurityDefiner = false
	}
	if v, ok := d.GetOk(funcVolatilityAttr); ok {
		pgFunction.Volatility = v.(string)
	} else {
		pgFunction.Volatility = defaultFunctionVolatility
	}

	// For the main returns if not provided
	argOutput := "void"

	if args, ok := d.GetOk(funcArgAttr); ok {
		args := args.([]any)

		for _, arg := range args {
			arg := arg.(map[string]any)

			var pgArg PGFunctionArg

			if v, ok := arg[funcArgModeAttr]; ok {
				pgArg.Mode = v.(string)
			}

			if v, ok := arg[funcArgNameAttr]; ok {
				pgArg.Name = v.(string)
			}

			pgArg.Type = arg[funcArgTypeAttr].(string)

			if v, ok := arg[funcArgDefaultAttr]; ok {
				pgArg.Default = v.(string)
			}

			// For the main returns if not provided
			if strings.ToUpper(pgArg.Mode) == "OUT" {
				argOutput = pgArg.Type
			}

			pgFunction.Args = append(pgFunction.Args, pgArg)
		}
	}

	if v, ok := d.GetOk(funcReturnsAttr); ok {
		pgFunction.Returns = v.(string)
	} else {
		pgFunction.Returns = argOutput
	}

	return nil
}

// Parse reads the attributes of a function from its pg_get_functiondef
// output. The arguments are not parsed from it: argument defaults and types
// can contain commas, so they are read from the catalog (functionArgsQuery).
func (pgFunction *PGFunction) Parse(functionDefinition string) error {

	pgFunctionData := findStringSubmatchMap(
		`(?si)CREATE\sOR\sREPLACE\sFUNCTION\s(?P<Schema>[^.]+)\.(?P<Name>[^(]+)\((?P<Args>.*)\).*RETURNS\s(?P<Returns>[^\n]+).*LANGUAGE\s(?P<Language>[^\n\s]+)\s*(?P<Volatility>(STABLE|IMMUTABLE)?)\s*(?P<Parallel>(PARALLEL (SAFE|RESTRICTED))?)\s*(?P<Strict>(STRICT)?)\s*(?P<Security>(SECURITY DEFINER)?).*\$[a-zA-Z]*\$(?P<Body>.*)\$[a-zA-Z]*\$`,
		functionDefinition,
	)

	pgFunction.Schema = pgFunctionData["Schema"]
	pgFunction.Name = pgFunctionData["Name"]
	pgFunction.Returns = pgFunctionData["Returns"]
	pgFunction.Language = pgFunctionData["Language"]
	pgFunction.Body = pgFunctionData["Body"]
	pgFunction.SecurityDefiner = len(pgFunctionData["Security"]) > 0
	pgFunction.Strict = len(pgFunctionData["Strict"]) > 0
	if len(pgFunctionData["Volatility"]) == 0 {
		pgFunction.Volatility = defaultFunctionVolatility
	} else {
		pgFunction.Volatility = pgFunctionData["Volatility"]
	}
	if len(pgFunctionData["Parallel"]) == 0 {
		pgFunction.Parallel = defaultFunctionParallel
	} else {
		pgFunction.Parallel = strings.TrimPrefix(pgFunctionData["Parallel"], "PARALLEL ")
	}

	return nil
}

// catalogFunctionArg is one argument as read from pg_proc by
// functionArgsQuery.
type catalogFunctionArg struct {
	Name    string  `json:"name"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	Default *string `json:"default"`
}

// functionArgsQuery lists the arguments of the function p from the catalog as
// a JSON array, so nothing has to be split out of the function definition.
// The default of each argument comes from pg_get_function_arg_default, the
// same function information_schema.parameters uses. Types are formatted like
// pg_get_functiondef does (format_type), names quoted like it (quote_ident).
// Arguments of a RETURNS TABLE clause (mode 't') are not arguments.
const functionArgsQuery = `COALESCE((` +
	`SELECT json_agg(json_build_object(` +
	`'name', COALESCE(quote_ident(NULLIF(a.name, '')), ''), ` +
	`'mode', COALESCE(a.mode, 'i'), ` +
	`'type', format_type(a.typ, NULL), ` +
	`'default', pg_get_function_arg_default(p.oid, a.n::int)) ORDER BY a.n) ` +
	`FROM unnest(COALESCE(p.proallargtypes, p.proargtypes::oid[]), p.proargmodes::text[], p.proargnames) ` +
	`WITH ORDINALITY AS a(typ, mode, name, n) ` +
	`WHERE COALESCE(a.mode, 'i') <> 't'` +
	`), '[]')`

var functionArgModes = map[string]string{
	"i": "IN",
	"o": "OUT",
	"b": "INOUT",
	"v": "VARIADIC",
}

// parseFunctionArgs decodes the result of functionArgsQuery.
func parseFunctionArgs(data []byte) ([]PGFunctionArg, error) {
	var raw []catalogFunctionArg
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("could not decode function arguments: %w", err)
	}
	args := make([]PGFunctionArg, 0, len(raw))
	for _, a := range raw {
		mode, ok := functionArgModes[a.Mode]
		if !ok {
			return nil, fmt.Errorf("unknown function argument mode %q", a.Mode)
		}
		arg := PGFunctionArg{Name: a.Name, Mode: mode, Type: a.Type}
		if a.Default != nil {
			arg.Default = *a.Default
		}
		args = append(args, arg)
	}
	return args, nil
}

// sameFunctionArgType reports whether the configured argument type is the
// type PostgreSQL reports back. PostgreSQL drops type modifiers of function
// arguments (numeric(10,2) is stored as numeric) and prints a type without
// its schema when the schema is in the search_path (pg_catalog.text is
// printed as text), so these spellings are kept rather than replaced on every
// plan.
func sameFunctionArgType(configured, read string) bool {
	if strings.EqualFold(configured, read) {
		return true
	}
	t := functionArgTypmod.ReplaceAllString(configured, "")
	if strings.EqualFold(t, read) {
		return true
	}
	if i := strings.Index(t, "."); i >= 0 && !strings.ContainsAny(t[:i], `"(`) {
		return strings.EqualFold(t[i+1:], read)
	}
	return false
}

// functionArgTypmod matches a type modifier such as (10,2) in numeric(10,2).
var functionArgTypmod = regexp.MustCompile(`\s*\([^()]*\)`)

func normalizeFunctionBody(body string) string {
	newBodyMap := findStringSubmatchMap(`(?si).*\$[a-zA-Z]*\$\s(?P<Body>.*)\s\$[a-zA-Z]*\$.*`, body)
	if newBody, ok := newBodyMap["Body"]; ok {
		return newBody
	}
	return body
}
