package postgresql

import (
	"errors"
	"strings"
	"testing"
)

// fakeSecurityLabels returns a lookup that reports the labels in existing,
// keyed by "<provider>|<object_type>|<object_name>".
func fakeSecurityLabels(existing ...string) securityLabelLookup {
	set := map[string]bool{}
	for _, e := range existing {
		set[e] = true
	}
	return func(objType, objName, provider string) (bool, error) {
		return set[provider+"|"+objType+"|"+objName], nil
	}
}

func TestParseSecurityLabelImportID(t *testing.T) {
	lookup := fakeSecurityLabels(
		"dummy|role|alice",
		"my.prov|role|r",
		"p|table|a.b",
		"p|table|x",
		"p|event trigger|trg",
		"q.role.a|role|a",
	)

	cases := []struct {
		id                           string
		wantProvider, wantType, want string
		wantErr                      string
	}{
		{id: "dummy.role.alice", wantProvider: "dummy", wantType: "role", want: "alice"},
		// The object type is matched in any case and returned in lower case.
		{id: "dummy.ROLE.alice", wantProvider: "dummy", wantType: "role", want: "alice"},
		// A dot in the provider.
		{id: "my.prov.ROLE.r", wantProvider: "my.prov", wantType: "role", want: "r"},
		// A dot in the object name: only "a.b" exists, not "b".
		{id: "p.TABLE.a.b", wantProvider: "p", wantType: "table", want: "a.b"},
		// Object types with a space.
		{id: "p.event trigger.trg", wantProvider: "p", wantType: "event trigger", want: "trg"},
		// Two dot pairs enclose a type, but only one split names a label.
		{id: "q.role.a.role.a", wantProvider: "q.role.a", wantType: "role", want: "a"},
		{id: "dummy.role.bob", wantErr: "no security label matches the ID"},
		{id: "dummy.nosuchtype.alice", wantErr: "no security label matches the ID"},
		{id: "dummy", wantErr: "no security label matches the ID"},
		{id: "", wantErr: "no security label matches the ID"},
	}

	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			provider, objType, objName, err := parseSecurityLabelImportID(c.id, lookup)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != c.wantProvider || objType != c.wantType || objName != c.want {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					provider, objType, objName, c.wantProvider, c.wantType, c.want)
			}
		})
	}
}

func TestParseSecurityLabelImportIDAmbiguous(t *testing.T) {
	lookup := fakeSecurityLabels("p|table|a.table.b", "p.table.a|table|b")
	_, _, _, err := parseSecurityLabelImportID("p.table.a.table.b", lookup)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
}

func TestParseSecurityLabelImportIDLookupError(t *testing.T) {
	boom := errors.New("boom")
	lookup := func(string, string, string) (bool, error) { return false, boom }
	if _, _, _, err := parseSecurityLabelImportID("p.role.r", lookup); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}
