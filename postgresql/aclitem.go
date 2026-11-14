package postgresql

import (
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// aclPrivilege is one privilege of an aclitem, as aclexplode reports it:
// privilege_type and is_grantable.
type aclPrivilege struct {
	Type      string
	Grantable bool
}

// aclEntry is one aclitem of an ACL ("grantee=privileges/grantor"), with role
// names instead of OIDs. Grantee "" is PUBLIC.
type aclEntry struct {
	Grantee    string
	Grantor    string
	Privileges []aclPrivilege
}

// aclPrivilegeNames maps the privilege letters of aclitem's text form to the
// privilege_type names aclexplode returns (PostgreSQL's convert_aclright_to_string).
var aclPrivilegeNames = map[byte]string{
	'a': "INSERT",
	'r': "SELECT",
	'w': "UPDATE",
	'd': "DELETE",
	'D': "TRUNCATE",
	'x': "REFERENCES",
	't': "TRIGGER",
	'X': "EXECUTE",
	'U': "USAGE",
	'C': "CREATE",
	'T': "TEMPORARY",
	'c': "CONNECT",
	's': "SET",
	'A': "ALTER SYSTEM",
	'm': "MAINTAIN",
}

// parseACL parses the text form of an aclitem[] column (e.g. relacl::text):
// an array literal whose elements are aclitems. A NULL ACL must be handled by
// the caller; "{}" yields no entry.
func parseACL(acl string) ([]aclEntry, error) {
	var items pq.StringArray
	if err := items.Scan([]byte(acl)); err != nil {
		return nil, fmt.Errorf("invalid ACL array %q: %w", acl, err)
	}
	var entries []aclEntry
	for _, item := range items {
		entry, err := parseACLItem(item)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// parseACLItem parses one aclitem in its text form, as printed by aclitemout:
// "grantee=privileges/grantor", where each privilege letter may be followed
// by * (grant option), an empty grantee is PUBLIC, and a role name containing
// anything but [A-Za-z0-9_] is double-quoted with inner quotes doubled.
func parseACLItem(item string) (aclEntry, error) {
	var entry aclEntry

	grantee, rest, err := parseACLIdent(item)
	if err != nil {
		return entry, fmt.Errorf("invalid aclitem %q: %w", item, err)
	}
	if len(rest) == 0 || rest[0] != '=' {
		return entry, fmt.Errorf("invalid aclitem %q: expected = after the grantee", item)
	}
	rest = rest[1:]

	var privileges []aclPrivilege
	for len(rest) > 0 && rest[0] != '/' {
		name, ok := aclPrivilegeNames[rest[0]]
		if !ok {
			return entry, fmt.Errorf("invalid aclitem %q: unknown privilege %q", item, rest[0])
		}
		rest = rest[1:]
		grantable := len(rest) > 0 && rest[0] == '*'
		if grantable {
			rest = rest[1:]
		}
		privileges = append(privileges, aclPrivilege{Type: name, Grantable: grantable})
	}
	if len(rest) == 0 {
		return entry, fmt.Errorf("invalid aclitem %q: missing grantor", item)
	}

	grantor, rest, err := parseACLIdent(rest[1:])
	if err != nil {
		return entry, fmt.Errorf("invalid aclitem %q: %w", item, err)
	}
	if grantor == "" || rest != "" {
		return entry, fmt.Errorf("invalid aclitem %q: malformed grantor", item)
	}

	entry.Grantee = grantee
	entry.Grantor = grantor
	entry.Privileges = privileges
	return entry, nil
}

// parseACLIdent reads a role name at the start of s (quoted or not, as putid
// writes it) and returns it with the remainder of s.
func parseACLIdent(s string) (string, string, error) {
	if len(s) == 0 || s[0] != '"' {
		end := 0
		for end < len(s) && s[end] != '=' && s[end] != '/' {
			end++
		}
		name := s[:end]
		if strings.ContainsAny(name, " ()") {
			// aclitemout prints a grantee or grantor whose role no longer
			// exists as `unknown (OID=n)`, unquoted.
			return "", "", fmt.Errorf("unquoted role name %q", name)
		}
		return name, s[end:], nil
	}

	var name strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			name.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			name.WriteByte('"')
			i++
			continue
		}
		return name.String(), s[i+1:], nil
	}
	return "", "", fmt.Errorf("unterminated quoted role name in %q", s)
}

// aclPrivilegesGrantedTo returns the privilege types granted to grantee ("" for
// PUBLIC) in the ACL text, in ACL order, from every grantor and whether or not
// they carry the grant option, like the aclexplode rows the Read queries
// aggregate. nil when the grantee has no entry.
func aclPrivilegesGrantedTo(acl, grantee string) ([]string, error) {
	entries, err := parseACL(acl)
	if err != nil {
		return nil, err
	}
	var privileges []string
	for _, entry := range entries {
		if entry.Grantee != grantee {
			continue
		}
		for _, p := range entry.Privileges {
			privileges = append(privileges, p.Type)
		}
	}
	return privileges, nil
}
