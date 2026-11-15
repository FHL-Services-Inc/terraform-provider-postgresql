package postgresql

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeUserMappings returns a lookup over a fixed list of mappings, recording
// the server names it was asked for.
func fakeUserMappings(mappings []userMappingCatalogEntry, asked *[]string) userMappingLookup {
	return func(servers []string) ([]userMappingCatalogEntry, error) {
		if asked != nil {
			*asked = append(*asked, servers...)
		}
		var out []userMappingCatalogEntry
		for _, m := range mappings {
			for _, s := range servers {
				if m.server == s {
					out = append(out, m)
				}
			}
		}
		return out, nil
	}
}

func TestParseUserMappingImportID(t *testing.T) {
	mappings := []userMappingCatalogEntry{
		{user: "alice", server: "srv"},
		{public: true, user: "public", server: "srv"},
		{user: "bob.smith", server: "srv"},
		{user: "carol", server: "srv.eu"},
		{user: "dave", server: "a.b"},
		{user: "dave.a", server: "b"},
		{user: "PUBLIC", server: "quoted"},
		{current: true, user: "postgres", server: "mine"},
		{user: "user", server: "mine"},
	}

	cases := []struct {
		id         string
		wantUser   string
		wantServer string
		wantErr    string
	}{
		{id: "alice.srv", wantUser: "alice", wantServer: "srv"},
		// PUBLIC keeps the spelling of the ID (the spelling in the config).
		{id: "public.srv", wantUser: "public", wantServer: "srv"},
		{id: "PUBLIC.srv", wantUser: "PUBLIC", wantServer: "srv"},
		{id: "Public.srv", wantUser: "Public", wantServer: "srv"},
		// Dots in either name: only one split exists in the catalog.
		{id: "bob.smith.srv", wantUser: "bob.smith", wantServer: "srv"},
		{id: "carol.srv.eu", wantUser: "carol", wantServer: "srv.eu"},
		// Two splits exist.
		{id: "dave.a.b", wantErr: "ambiguous"},
		{id: "alice.other", wantErr: "no user mapping"},
		{id: "nobody.srv", wantErr: "no user mapping"},
		{id: "nodot", wantErr: "no user mapping"},
		{id: "", wantErr: "no user mapping"},
		// A role named "PUBLIC" is not the PUBLIC mapping and can't be
		// managed by this resource.
		{id: "PUBLIC.quoted", wantErr: "no user mapping"},
		// The current user keywords import the provider role's mapping and
		// keep their spelling.
		{id: "CURRENT_USER.mine", wantUser: "CURRENT_USER", wantServer: "mine"},
		{id: "CURRENT_ROLE.mine", wantUser: "CURRENT_ROLE", wantServer: "mine"},
		{id: "USER.mine", wantUser: "USER", wantServer: "mine"},
		{id: "postgres.mine", wantUser: "postgres", wantServer: "mine"},
		{id: "CURRENT_USER.srv", wantErr: "no user mapping"},
		// Not upper case: a role name.
		{id: "user.mine", wantUser: "user", wantServer: "mine"},
		{id: "current_user.mine", wantErr: "no user mapping"},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			user, server, err := parseUserMappingImportID(tc.id, fakeUserMappings(mappings, nil))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got user=%q server=%q err=%v, want error containing %q", user, server, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if user != tc.wantUser || server != tc.wantServer {
				t.Errorf("got user=%q server=%q, want user=%q server=%q", user, server, tc.wantUser, tc.wantServer)
			}
		})
	}
}

func TestParseUserMappingImportIDLooksUpEverySplit(t *testing.T) {
	var asked []string
	if _, _, err := parseUserMappingImportID("a.b.c", fakeUserMappings(nil, &asked)); err == nil {
		t.Fatal("expected an error for an ID with no matching mapping")
	}
	sort.Strings(asked)
	if want := []string{"b.c", "c"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("looked up servers %v, want %v", asked, want)
	}
}

func TestParseUserMappingImportIDLookupError(t *testing.T) {
	boom := errors.New("boom")
	lookup := func([]string) ([]userMappingCatalogEntry, error) { return nil, boom }
	if _, _, err := parseUserMappingImportID("a.b", lookup); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

func TestUserMappingUserSQL(t *testing.T) {
	cases := map[string]string{
		"alice":        `"alice"`,
		"public":       "PUBLIC",
		"PUBLIC":       "PUBLIC",
		"CURRENT_USER": "CURRENT_USER",
		// CURRENT_ROLE is a syntax error there before PostgreSQL 14.
		"CURRENT_ROLE": "CURRENT_USER",
		"USER":         "CURRENT_USER",
		// Only the upper case spelling is a keyword.
		"user":         `"user"`,
		"current_user": `"current_user"`,
		"Current_Role": `"Current_Role"`,
	}
	for username, want := range cases {
		if got := userMappingUserSQL(username); got != want {
			t.Errorf("userMappingUserSQL(%q) = %s, want %s", username, got, want)
		}
	}
}
