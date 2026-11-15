package postgresql

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/stretchr/testify/assert"
)

func TestFromResourceData(t *testing.T) {
	d := mockFunctionResourceData(t, PGFunction{
		Name: "increment",
		Body: "BEGIN result = i + 1; END;",
		Args: []PGFunctionArg{
			{
				Name: "i",
				Type: "integer",
			},
			{
				Name: "result",
				Type: "integer",
				Mode: "OUT",
			},
		},
	})

	var pgFunction PGFunction

	err := pgFunction.FromResourceData(d)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, pgFunction, PGFunction{
		Schema:          "public",
		Name:            "increment",
		Returns:         "integer",
		Language:        "plpgsql",
		Parallel:        defaultFunctionParallel,
		Strict:          false,
		SecurityDefiner: false,
		Volatility:      defaultFunctionVolatility,
		Body:            "BEGIN result = i + 1; END;",
		Args: []PGFunctionArg{
			{
				Name: "i",
				Type: "integer",
			},
			{
				Name: "result",
				Type: "integer",
				Mode: "OUT",
			},
		},
	})
}

func TestFromResourceDataWithArguments(t *testing.T) {
	d := mockFunctionResourceData(t, PGFunction{
		Name: "increment",
		Body: "BEGIN result = i + 1; END;",
		Args: []PGFunctionArg{
			{
				Name: "i",
				Type: "integer",
			},
			{
				Name: "result",
				Type: "integer",
				Mode: "OUT",
			},
		},
		Parallel:        "SAFE",
		Strict:          true,
		SecurityDefiner: true,
		Volatility:      "IMMUTABLE",
	})

	var pgFunction PGFunction

	err := pgFunction.FromResourceData(d)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, pgFunction, PGFunction{
		Schema:          "public",
		Name:            "increment",
		Returns:         "integer",
		Language:        "plpgsql",
		Parallel:        "SAFE",
		Strict:          true,
		SecurityDefiner: true,
		Volatility:      "IMMUTABLE",
		Body:            "BEGIN result = i + 1; END;",
		Args: []PGFunctionArg{
			{
				Name: "i",
				Type: "integer",
			},
			{
				Name: "result",
				Type: "integer",
				Mode: "OUT",
			},
		},
	})
}

func TestPGFunctionParseWithArguments(t *testing.T) {

	var functionDefinition = `
CREATE OR REPLACE FUNCTION public.pg_func_test(showtext boolean, OUT userid oid, default_null integer DEFAULT NULL::integer, simple_default integer DEFAULT 42, long_default character varying DEFAULT 'foo'::character varying)
RETURNS SETOF record
LANGUAGE c
STABLE PARALLEL SAFE STRICT SECURITY DEFINER
AS $function$pg_func_test_body$function$
	`

	var pgFunction PGFunction

	err := pgFunction.Parse(functionDefinition)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, pgFunction, PGFunction{
		Name:            "pg_func_test",
		Schema:          "public",
		Returns:         "SETOF record",
		Language:        "c",
		Parallel:        "SAFE",
		SecurityDefiner: true,
		Strict:          true,
		Volatility:      "STABLE",
		Body:            "pg_func_test_body",
	})
}

func TestPGFunctionParseWithoutArguments(t *testing.T) {

	var functionDefinition = `
CREATE OR REPLACE FUNCTION public.pg_func_test()
RETURNS SETOF record
LANGUAGE plpgsql
AS $function$
MultiLine Function
$function$
	`

	var pgFunction PGFunction

	err := pgFunction.Parse(functionDefinition)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, pgFunction, PGFunction{
		Name:            "pg_func_test",
		Schema:          "public",
		Returns:         "SETOF record",
		Language:        "plpgsql",
		Parallel:        "UNSAFE",
		SecurityDefiner: false,
		Strict:          false,
		Volatility:      "VOLATILE",
		Body: `
MultiLine Function
`,
	})
}

func TestParseFunctionArgs(t *testing.T) {
	args, err := parseFunctionArgs([]byte(`[
		{"name": "a", "mode": "i", "type": "text[]", "default": "ARRAY['a'::text, 'b'::text]"},
		{"name": "b", "mode": "i", "type": "numeric", "default": "1.5"},
		{"name": "", "mode": "i", "type": "double precision", "default": null},
		{"name": "\"select\"", "mode": "b", "type": "integer", "default": null},
		{"name": "rest", "mode": "v", "type": "text[]", "default": null},
		{"name": "result", "mode": "o", "type": "integer", "default": null}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []PGFunctionArg{
		{Mode: "IN", Name: "a", Type: "text[]", Default: "ARRAY['a'::text, 'b'::text]"},
		{Mode: "IN", Name: "b", Type: "numeric", Default: "1.5"},
		{Mode: "IN", Name: "", Type: "double precision"},
		{Mode: "INOUT", Name: `"select"`, Type: "integer"},
		{Mode: "VARIADIC", Name: "rest", Type: "text[]"},
		{Mode: "OUT", Name: "result", Type: "integer"},
	}, args)

	args, err = parseFunctionArgs([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Empty(t, args)

	_, err = parseFunctionArgs([]byte(`[{"name": "x", "mode": "t", "type": "integer"}]`))
	assert.Error(t, err)
}

func TestSameFunctionArgType(t *testing.T) {
	for _, tc := range []struct {
		configured, read string
		same             bool
	}{
		{"integer", "integer", true},
		{"TEXT", "text", true},
		{"numeric(10,2)", "numeric", true},
		{"numeric(10, 2)[]", "numeric[]", true},
		{"character varying(20)", "character varying", true},
		{"pg_catalog.text", "text", true},
		{"public.citext", "citext", true},
		{"public.citext[]", "citext[]", true},
		{"fn_types.dom", "fn_types.dom", true},
		{"integer", "bigint", false},
		{"public.citext", "text", false},
		{"other.dom", "fn_types.dom", false},
	} {
		assert.Equal(t, tc.same, sameFunctionArgType(tc.configured, tc.read), "%q vs %q", tc.configured, tc.read)
	}
}

func mockFunctionResourceData(t *testing.T, obj PGFunction) *schema.ResourceData {

	state := terraform.InstanceState{}

	state.ID = ""
	// Build the attribute map from ForemanModel
	attributes := make(map[string]any)

	attributes["name"] = obj.Name
	attributes["returns"] = obj.Returns
	attributes["language"] = obj.Language
	attributes["body"] = obj.Body
	attributes["strict"] = obj.Strict
	attributes["security_definer"] = obj.SecurityDefiner
	attributes["parallel"] = obj.Parallel
	attributes["volatility"] = obj.Volatility

	var args []any

	for _, a := range obj.Args {
		args = append(args, map[string]any{
			"type":    a.Type,
			"name":    a.Name,
			"mode":    a.Mode,
			"default": a.Default,
		})
	}

	attributes["arg"] = args

	return schema.TestResourceDataRaw(t, resourcePostgreSQLFunction().Schema, attributes)
}
