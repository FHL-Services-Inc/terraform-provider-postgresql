package postgresql

import (
	"reflect"
	"testing"
)

func TestParseACLItem(t *testing.T) {
	cases := []struct {
		in   string
		want aclEntry
	}{
		{"postgres=arwdDxt/postgres", aclEntry{
			Grantee: "postgres", Grantor: "postgres",
			Privileges: []aclPrivilege{{"INSERT", false}, {"SELECT", false}, {"UPDATE", false}, {"DELETE", false}, {"TRUNCATE", false}, {"REFERENCES", false}, {"TRIGGER", false}},
		}},
		// PUBLIC is an empty grantee.
		{"=r/owner", aclEntry{Grantee: "", Grantor: "owner", Privileges: []aclPrivilege{{"SELECT", false}}}},
		// Grant option is the * after the privilege letter.
		{"app=r*w/owner", aclEntry{Grantee: "app", Grantor: "owner", Privileges: []aclPrivilege{{"SELECT", true}, {"UPDATE", false}}}},
		// Identifiers with characters outside [A-Za-z0-9_] are double-quoted; a
		// double quote inside is doubled.
		{`"my role"=U/"the ""owner"""`, aclEntry{Grantee: "my role", Grantor: `the "owner"`, Privileges: []aclPrivilege{{"USAGE", false}}}},
		// A quoted name may contain = and / without ending the field.
		{`"a=b/c"=X/postgres`, aclEntry{Grantee: "a=b/c", Grantor: "postgres", Privileges: []aclPrivilege{{"EXECUTE", false}}}},
		// Every privilege letter PostgreSQL 12-18 can print.
		{"r=arwdDxtXUCTcsAm/g", aclEntry{
			Grantee: "r", Grantor: "g",
			Privileges: []aclPrivilege{
				{"INSERT", false}, {"SELECT", false}, {"UPDATE", false}, {"DELETE", false}, {"TRUNCATE", false},
				{"REFERENCES", false}, {"TRIGGER", false}, {"EXECUTE", false}, {"USAGE", false}, {"CREATE", false},
				{"TEMPORARY", false}, {"CONNECT", false}, {"SET", false}, {"ALTER SYSTEM", false}, {"MAINTAIN", false},
			},
		}},
		// No privilege at all is a valid item (REVOKE left an empty entry).
		{"r=/g", aclEntry{Grantee: "r", Grantor: "g", Privileges: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseACLItem(tc.in)
			if err != nil {
				t.Fatalf("parseACLItem(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseACLItem(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseACLItemRejectsMalformedItems(t *testing.T) {
	for _, in := range []string{
		"",
		"postgres",          // no =
		"postgres=r",        // no grantor
		"postgres=r/",       // empty grantor
		"postgres=q/owner",  // unknown privilege letter
		"postgres=*r/owner", // * without a privilege
		`"unterminated=r/o`, // open quote
		`"a"b=r/o`,          // text after the closing quote
		"unknown (OID=16388)=r/o",
	} {
		if got, err := parseACLItem(in); err == nil {
			t.Errorf("parseACLItem(%q) = %+v, want an error", in, got)
		}
	}
}

func TestParseACL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []aclEntry
	}{
		{"empty array", "{}", nil},
		{"plain", "{postgres=arwdDxt/postgres,=r/postgres}", []aclEntry{
			{Grantee: "postgres", Grantor: "postgres", Privileges: []aclPrivilege{{"INSERT", false}, {"SELECT", false}, {"UPDATE", false}, {"DELETE", false}, {"TRUNCATE", false}, {"REFERENCES", false}, {"TRIGGER", false}}},
			{Grantee: "", Grantor: "postgres", Privileges: []aclPrivilege{{"SELECT", false}}},
		}},
		// An item containing a double quote is itself quoted by array_out,
		// with the inner quotes backslash-escaped.
		{"quoted element", `{"\"my role\"=r/postgres",app=w/postgres}`, []aclEntry{
			{Grantee: "my role", Grantor: "postgres", Privileges: []aclPrivilege{{"SELECT", false}}},
			{Grantee: "app", Grantor: "postgres", Privileges: []aclPrivilege{{"UPDATE", false}}},
		}},
		{"backslash in a name", `{"\"a\\b\"=r/postgres"}`, []aclEntry{
			{Grantee: `a\b`, Grantor: "postgres", Privileges: []aclPrivilege{{"SELECT", false}}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseACL(tc.in)
			if err != nil {
				t.Fatalf("parseACL(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseACL(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
		})
	}

	if _, err := parseACL("{postgres=zz/postgres}"); err == nil {
		t.Error("parseACL with an invalid item returned no error")
	}
	if _, err := parseACL("not an array"); err == nil {
		t.Error("parseACL with an invalid array literal returned no error")
	}
}

func TestACLPrivilegesGrantedTo(t *testing.T) {
	acl := `{owner=arwdDxt/owner,app=r/owner,app=w*/other,=r/owner,"\"my role\"=d/owner"}`

	cases := []struct {
		grantee string
		want    []string
	}{
		{"app", []string{"SELECT", "UPDATE"}}, // every grantor, grant option ignored
		{"", []string{"SELECT"}},              // PUBLIC
		{"my role", []string{"DELETE"}},
		{"nobody", nil},
	}
	for _, tc := range cases {
		got, err := aclPrivilegesGrantedTo(acl, tc.grantee)
		if err != nil {
			t.Fatalf("aclPrivilegesGrantedTo(%q): %v", tc.grantee, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("aclPrivilegesGrantedTo(%q) = %v, want %v", tc.grantee, got, tc.want)
		}
	}
}
