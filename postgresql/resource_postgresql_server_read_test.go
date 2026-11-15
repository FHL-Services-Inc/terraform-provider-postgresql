package postgresql

import (
	"reflect"
	"testing"
)

func TestParseServerOptions(t *testing.T) {
	got := parseServerOptions([]string{"host=foo", "options=-c statement_timeout=0", "flag"})
	want := map[string]any{
		"host":    "foo",
		"options": "-c statement_timeout=0",
		"flag":    "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseServerOptions() = %#v, want %#v", got, want)
	}
}
