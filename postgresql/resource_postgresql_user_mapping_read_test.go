package postgresql

import (
	"database/sql/driver"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// singleValueRows is a one-row, one-column result set.
type singleValueRows struct {
	value driver.Value
	done  bool
}

func (*singleValueRows) Columns() []string { return []string{"umoptions"} }
func (*singleValueRows) Close() error      { return nil }
func (r *singleValueRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

// userMappingResponder answers the information_schema._pg_user_mappings query
// with primaryErr (or primaryValue) and the pg_user_mappings fallback with
// fallbackValue. A nil value is SQL NULL.
func userMappingResponder(primaryErr error, primaryValue, fallbackValue driver.Value) func(string) (driver.Rows, error) {
	return func(query string) (driver.Rows, error) {
		if strings.Contains(query, "information_schema._pg_user_mappings") {
			if primaryErr != nil {
				return nil, primaryErr
			}
			return &singleValueRows{value: primaryValue}, nil
		}
		return &singleValueRows{value: fallbackValue}, nil
	}
}

func TestUserMappingReadOptions(t *testing.T) {
	denied := &pq.Error{Code: "42501", Message: "permission denied for view _pg_user_mappings"}
	stateOptions := map[string]any{"user": "admin", "password": "pass"}

	cases := []struct {
		name    string
		respond func(string) (driver.Rows, error)
		want    map[string]any
	}{
		{
			// Non-superuser: pg_user_mappings hides umoptions of other roles'
			// mappings, so the options in state are kept instead of showing
			// a permanent diff.
			name:    "fallback masks options",
			respond: userMappingResponder(denied, nil, nil),
			want:    stateOptions,
		},
		{
			name:    "fallback shows options",
			respond: userMappingResponder(denied, nil, "{user=other,password=secret}"),
			want:    map[string]any{"user": "other", "password": "secret"},
		},
		{
			// information_schema._pg_user_mappings is not masked: NULL there
			// means the mapping has no options, so drift is still reported.
			name:    "primary query without options",
			respond: userMappingResponder(nil, nil, nil),
			want:    map[string]any{},
		},
		{
			name:    "primary query with options",
			respond: userMappingResponder(nil, "{user=other}", nil),
			want:    map[string]any{"user": "other"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newStmtCountingConnection(t, nil)
			stmtCounter.mu.Lock()
			stmtCounter.respond = tc.respond
			stmtCounter.mu.Unlock()

			d := schema.TestResourceDataRaw(t, resourcePostgreSQLUserMapping().Schema, map[string]any{
				userMappingUserNameAttr:   "u",
				userMappingServerNameAttr: "srv",
				userMappingOptionsAttr:    stateOptions,
			})
			d.SetId("u.srv")

			if err := resourcePostgreSQLUserMappingRead(conn, d); err != nil {
				t.Fatalf("Read returned error: %v", err)
			}
			if d.Id() == "" {
				t.Fatal("Read cleared the ID of an existing mapping")
			}

			begins, _ := stmtCounter.counts()
			if begins != 0 {
				t.Errorf("Read opened %d transaction(s), want 0", begins)
			}
			got := d.Get(userMappingOptionsAttr).(map[string]any)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("options = %v, want %v", got, tc.want)
			}
		})
	}
}
