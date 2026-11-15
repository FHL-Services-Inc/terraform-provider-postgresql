package postgresql

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

func TestParseFunctionID(t *testing.T) {
	for _, tc := range []struct {
		id                          string
		database, schema, name, arg string
	}{
		{"db.public.f()", "db", "public", "f", ""},
		{"db.public.f(integer,text)", "db", "public", "f", "integer,text"},
		{"public.f(integer)", "", "public", "f", "integer"},
		// #127: schema-qualified argument types.
		{"db.public.f(public.citext)", "db", "public", "f", "public.citext"},
		{"public.f(public.citext,pg_catalog.text)", "", "public", "f", "public.citext,pg_catalog.text"},
		{"db.public.f(numeric(10,2))", "db", "public", "f", "numeric(10,2)"},
		// #127: a dot in the function name.
		{"db.public.dotted.name(integer)", "db", "public", "dotted.name", "integer"},
	} {
		database, schemaName, name, args, err := parseFunctionID(tc.id)
		if assert.NoError(t, err, tc.id) {
			assert.Equal(t, []string{tc.database, tc.schema, tc.name, tc.arg},
				[]string{database, schemaName, name, args}, tc.id)
		}
	}

	for _, id := range []string{"f()", "db.public.f", "public.f(", ".f()", "db..f()", "public.(integer)"} {
		_, _, _, _, err := parseFunctionID(id)
		assert.Error(t, err, id)
	}
}

func TestExpandFunctionIDQuotesParts(t *testing.T) {
	database, signature, err := expandFunctionID("db.public.dotted.name(public.citext)", nil, nil)
	if assert.NoError(t, err) {
		assert.Equal(t, "db", database)
		assert.Equal(t, `"public"."dotted.name"(public.citext)`, signature)
	}
}

func TestIsFunctionArgTypeMissingError(t *testing.T) {
	assert.True(t, isFunctionArgTypeMissingError(&pq.Error{Code: "3F000"}))
	assert.True(t, isFunctionArgTypeMissingError(fmt.Errorf("wrapped: %w", &pq.Error{Code: "42704"})))
	assert.False(t, isFunctionArgTypeMissingError(&pq.Error{Code: "42501"}))
	assert.False(t, isFunctionArgTypeMissingError(errors.New("3F000")))
	assert.False(t, isFunctionArgTypeMissingError(nil))
}

func TestSplitFunctionArgTypes(t *testing.T) {
	for _, c := range []struct {
		args string
		want []string
	}{
		{"", nil},
		{"integer", []string{"integer"}},
		{"integer, text", []string{"integer", "text"}},
		{"numeric(10,2),character varying(5)", []string{"numeric(10,2)", "character varying(5)"}},
		{`public."my,type", text[]`, []string{`public."my,type"`, "text[]"}},
	} {
		if got := splitFunctionArgTypes(c.args); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitFunctionArgTypes(%q) = %#v, want %#v", c.args, got, c.want)
		}
	}
}
