package postgresql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lib/pq"
)

// This file is the pure part of postgresql_role_privileges: how the
// configuration of one role in one database is expanded against the objects
// that currently exist, how it is compared with the privileges the role
// actually holds, and how the difference is rendered as GRANT / REVOKE
// statements. Nothing here talks to the database, so every rule has a unit
// test.

// aclObject identifies one thing a role holds privileges on, within one
// database. kind is "schema" (name empty), "table" or "sequence" (name is the
// relation name) or "default:<object type>" for default privileges (name is
// the owner whose default privileges are altered).
type aclObject struct {
	kind, schema, name string
}

func (o aclObject) String() string {
	switch {
	case o.kind == "schema":
		return "schema " + o.schema
	case strings.HasPrefix(o.kind, "default:"):
		return fmt.Sprintf("default %s privileges %s (owner %s)", strings.TrimPrefix(o.kind, "default:"), o.schema, o.name)
	}
	return fmt.Sprintf("%s %s.%s", o.kind, o.schema, o.name)
}

// privilegeSet maps a privilege (SELECT, USAGE, ...) to whether it is held
// with grant option.
type privilegeSet map[string]bool

// aclState is what one role holds on a set of objects. An object present
// with an empty set exists but carries no privilege for the role.
type aclState map[aclObject]privilegeSet

// parsePrivileges parses the privilege entries of the Read query: the
// privilege name, with a trailing "*" when it is held with grant option.
func parsePrivileges(entries []string) privilegeSet {
	set := make(privilegeSet, len(entries))
	for _, e := range entries {
		if p := strings.TrimSuffix(e, "*"); p != e {
			set[p] = true
		} else if _, ok := set[p]; !ok {
			set[p] = false
		}
	}
	return set
}

// rolePrivilegesConfig is the configuration of one postgresql_role_privileges
// instance, as parsed from the resource data.
type rolePrivilegesConfig struct {
	schemas []schemaPrivilegesConfig
}

type schemaPrivilegesConfig struct {
	name          string
	usage, create bool
	tables        []objectPrivilegesConfig
	sequences     []objectPrivilegesConfig
	defaults      []defaultPrivilegesConfig
}

// objectPrivilegesConfig is one tables/sequences block: an exact set of
// privileges on the listed objects, or on every current object of the kind in
// the schema when objects is empty.
type objectPrivilegesConfig struct {
	privileges  []string
	objects     []string
	grantOption bool
}

// defaultPrivilegesConfig is one default_privileges block.
type defaultPrivilegesConfig struct {
	owner, objectType string
	privileges        []string
	grantOption       bool
}

// expandPrivileges turns a configured privilege list into a privilegeSet,
// expanding ALL to the full list of the kind.
func expandPrivileges(privileges []string, kind string, grantOption bool, allowed func(string) []string) privilegeSet {
	set := make(privilegeSet, len(privileges))
	for _, p := range privileges {
		p = strings.ToUpper(p)
		if p == "ALL" || p == "ALL PRIVILEGES" {
			for _, a := range allowed(kind) {
				if a != "ALL" {
					set[a] = grantOption
				}
			}
			continue
		}
		set[p] = grantOption
	}
	return set
}

// expandDesired computes, from the configuration and the objects that
// currently exist (actual), the exact privileges the role must hold on every
// object the configuration covers (desired), the set of objects the
// configuration manages (scope), and the problems that make the configuration
// inapplicable: a listed schema or object that does not exist, or two blocks
// that disagree on with_grant_option for the same object.
//
// Objects covered by several blocks get the union of their privileges. Blocks
// without objects expand to the objects of their kind present in actual, so
// "all tables" means the current tables, as it does for postgresql_grant.
func expandDesired(cfg rolePrivilegesConfig, actual aclState, allowed func(string) []string) (desired aclState, scope map[aclObject]bool, problems []string) {
	desired = aclState{}
	scope = map[aclObject]bool{}
	conflicts := map[aclObject]bool{}

	add := func(obj aclObject, privileges privilegeSet) {
		scope[obj] = true
		set, ok := desired[obj]
		if !ok {
			set = privilegeSet{}
			desired[obj] = set
		}
		for p, g := range privileges {
			if have, ok := set[p]; ok && have != g && !conflicts[obj] {
				conflicts[obj] = true
				problems = append(problems, fmt.Sprintf("%s: with_grant_option differs between blocks", obj))
			}
			set[p] = set[p] || g
		}
	}

	for _, s := range cfg.schemas {
		schemaObj := aclObject{kind: "schema", schema: s.name}
		if _, ok := actual[schemaObj]; !ok {
			problems = append(problems, fmt.Sprintf("%s: does not exist", schemaObj))
		}
		schemaPrivileges := privilegeSet{}
		if s.usage {
			schemaPrivileges["USAGE"] = false
		}
		if s.create {
			schemaPrivileges["CREATE"] = false
		}
		add(schemaObj, schemaPrivileges)

		for _, kb := range []struct {
			kind   string
			blocks []objectPrivilegesConfig
		}{{"table", s.tables}, {"sequence", s.sequences}} {
			kind := kb.kind
			for _, b := range kb.blocks {
				privileges := expandPrivileges(b.privileges, kind, b.grantOption, allowed)
				if len(b.objects) == 0 {
					for obj := range actual {
						if obj.kind == kind && obj.schema == s.name {
							add(obj, privileges)
						}
					}
					continue
				}
				for _, name := range b.objects {
					obj := aclObject{kind: kind, schema: s.name, name: name}
					if _, ok := actual[obj]; !ok {
						problems = append(problems, fmt.Sprintf("%s: does not exist", obj))
						continue
					}
					add(obj, privileges)
				}
			}
		}

		for _, b := range s.defaults {
			kind := "default:" + b.objectType
			add(aclObject{kind: kind, schema: s.name, name: b.owner}, expandPrivileges(b.privileges, kind, b.grantOption, allowed))
		}
	}

	return desired, scope, problems
}

// scopeOf returns the scope that manages exactly the objects of desired.
func scopeOf(desired aclState) map[aclObject]bool {
	scope := make(map[aclObject]bool, len(desired))
	for obj := range desired {
		scope[obj] = true
	}
	return scope
}

// exclusiveScope returns the scope of exclusive = true: everything the
// configuration covers plus everything the role currently holds in the
// schemas the Read returned.
func exclusiveScope(desired, actual aclState) map[aclObject]bool {
	scope := scopeOf(desired)
	for obj := range actual {
		scope[obj] = true
	}
	return scope
}

// aclChange is what has to change on one object: privileges to grant (with
// their grant option), privileges to revoke entirely, and privileges whose
// grant option alone must be revoked.
type aclChange struct {
	obj                         aclObject
	grant, revoke, revokeOption privilegeSet
}

func (c aclChange) empty() bool {
	return len(c.grant) == 0 && len(c.revoke) == 0 && len(c.revokeOption) == 0
}

func lessObject(a, b aclObject) bool {
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	if a.schema != b.schema {
		return a.schema < b.schema
	}
	return a.name < b.name
}

// diffACL compares desired with actual on every object in scope and returns
// the changes needed, sorted by object. Objects outside the scope are never
// touched, whatever the role holds on them.
func diffACL(desired, actual aclState, scope map[aclObject]bool) []aclChange {
	objects := make([]aclObject, 0, len(scope))
	for obj := range scope {
		objects = append(objects, obj)
	}
	sort.Slice(objects, func(i, j int) bool { return lessObject(objects[i], objects[j]) })

	var changes []aclChange
	for _, obj := range objects {
		want, have := desired[obj], actual[obj]
		c := aclChange{obj: obj}
		for p, grantable := range want {
			held, ok := have[p]
			switch {
			case !ok || (grantable && !held):
				if c.grant == nil {
					c.grant = privilegeSet{}
				}
				c.grant[p] = grantable
			case !grantable && held:
				if c.revokeOption == nil {
					c.revokeOption = privilegeSet{}
				}
				c.revokeOption[p] = false
			}
		}
		for p := range have {
			if _, ok := want[p]; !ok {
				if c.revoke == nil {
					c.revoke = privilegeSet{}
				}
				c.revoke[p] = false
			}
		}
		if !c.empty() {
			changes = append(changes, c)
		}
	}
	return changes
}

// maxDriftEntries caps the drift attribute so state does not grow with the
// size of an incident.
const maxDriftEntries = 50

// driftEntries renders problems and changes as the entries of the drift
// attribute: "+PRIV" is held but not configured, "-PRIV" is configured but
// missing, a trailing "*" marks the grant option.
func driftEntries(changes []aclChange, problems []string) []string {
	entries := append([]string{}, problems...)
	for _, c := range changes {
		var tokens []string
		for _, p := range sortedKeys(c.revoke) {
			tokens = append(tokens, "+"+p)
		}
		for _, p := range sortedKeys(c.revokeOption) {
			tokens = append(tokens, "+"+p+"*")
		}
		for _, p := range sortedKeys(c.grant) {
			if c.grant[p] {
				p += "*"
			}
			tokens = append(tokens, "-"+p)
		}
		entries = append(entries, fmt.Sprintf("%s: %s", c.obj, strings.Join(tokens, " ")))
	}
	if len(entries) > maxDriftEntries {
		more := len(entries) - maxDriftEntries
		entries = append(entries[:maxDriftEntries], fmt.Sprintf("... and %d more", more))
	}
	return entries
}

func sortedKeys(set privilegeSet) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// statementGroup collects, for one (phase, kind, schema, grant option), the
// objects every privilege applies to, so that privileges with the same object
// list are rendered as one statement.
type statementGroup struct {
	kind, schema string
	grantable    bool
	objects      map[string]map[string]bool // privilege -> object names
}

type groupKey struct {
	phase        int
	kind, schema string
	grantable    bool
}

const (
	phaseRevoke = iota
	phaseRevokeOption
	phaseGrant
)

// phaseSets returns the privilege sets of a change for the phases rendered
// together: both revoke phases in one pass, the grant phase in another.
func phaseSets(c aclChange, grant bool) map[int]privilegeSet {
	if grant {
		return map[int]privilegeSet{phaseGrant: c.grant}
	}
	return map[int]privilegeSet{phaseRevoke: c.revoke, phaseRevokeOption: c.revokeOption}
}

// renderACLStatements renders the changes as GRANT / REVOKE statements for
// role: every REVOKE first, then every GRANT, each phase ordered by object
// kind, schema and object name, so the output is deterministic. Privileges
// that apply to the same objects are merged into one statement, and a
// statement that covers every current object of its kind in the schema is
// rendered as ... ALL TABLES IN SCHEMA / ALL SEQUENCES IN SCHEMA. The
// statements carry no bind parameters so they can be sent as one string.
func renderACLStatements(role string, changes []aclChange, actual aclState) []string {
	var stmts []string
	for _, grant := range []bool{false, true} {
		groups := map[groupKey]*statementGroup{}
		var order []groupKey
		for _, c := range changes {
			for phase, set := range phaseSets(c, grant) {
				for p, grantable := range set {
					key := groupKey{phase: phase, kind: c.obj.kind, schema: c.obj.schema, grantable: grantable}
					g, ok := groups[key]
					if !ok {
						g = &statementGroup{kind: key.kind, schema: key.schema, grantable: grantable, objects: map[string]map[string]bool{}}
						groups[key] = g
						order = append(order, key)
					}
					if g.objects[p] == nil {
						g.objects[p] = map[string]bool{}
					}
					g.objects[p][c.obj.name] = true
				}
			}
		}
		sort.Slice(order, func(i, j int) bool {
			a, b := order[i], order[j]
			if a.kind != b.kind {
				return a.kind < b.kind
			}
			if a.schema != b.schema {
				return a.schema < b.schema
			}
			if a.phase != b.phase {
				return a.phase < b.phase
			}
			return !a.grantable && b.grantable
		})
		for _, key := range order {
			stmts = append(stmts, groups[key].render(key.phase, role, actual)...)
		}
	}
	return stmts
}

// render emits the statements of one group: privileges sharing an object
// list become one statement; statements are ordered by their first object
// name, then by privileges.
func (g *statementGroup) render(phase int, role string, actual aclState) []string {
	type merged struct {
		objectKey  string
		objects    []string
		privileges []string
	}
	byObjects := map[string]*merged{}
	for p, objs := range g.objects {
		names := make([]string, 0, len(objs))
		for n := range objs {
			names = append(names, n)
		}
		sort.Strings(names)
		key := strings.Join(names, "\x00")
		m, ok := byObjects[key]
		if !ok {
			m = &merged{objectKey: key, objects: names}
			byObjects[key] = m
		}
		m.privileges = append(m.privileges, p)
	}
	list := make([]*merged, 0, len(byObjects))
	for _, m := range byObjects {
		sort.Strings(m.privileges)
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].objectKey != list[j].objectKey {
			return list[i].objectKey < list[j].objectKey
		}
		return strings.Join(list[i].privileges, ",") < strings.Join(list[j].privileges, ",")
	})

	var stmts []string
	for _, m := range list {
		if strings.HasPrefix(g.kind, "default:") {
			// Default privileges are altered per owner: one statement each.
			for _, owner := range m.objects {
				stmts = append(stmts, g.statement(phase, role, m.privileges, []string{owner}, actual))
			}
			continue
		}
		stmts = append(stmts, g.statement(phase, role, m.privileges, m.objects, actual))
	}
	return stmts
}

func (g *statementGroup) statement(phase int, role string, privileges, objects []string, actual aclState) string {
	privs := strings.Join(privileges, ",")
	quotedRole := pq.QuoteIdentifier(role)

	verb, tail := "GRANT", " TO "+quotedRole
	switch phase {
	case phaseRevoke:
		verb, tail = "REVOKE", " FROM "+quotedRole
	case phaseRevokeOption:
		verb, tail = "REVOKE GRANT OPTION FOR", " FROM "+quotedRole
	}
	if phase == phaseGrant && g.grantable {
		tail += " WITH GRANT OPTION"
	}

	switch {
	case g.kind == "schema":
		return fmt.Sprintf("%s %s ON SCHEMA %s%s", verb, privs, pq.QuoteIdentifier(g.schema), tail)
	case strings.HasPrefix(g.kind, "default:"):
		// One owner per group entry: default privileges are keyed by owner.
		return fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s %s %s ON %sS%s",
			pq.QuoteIdentifier(objects[0]), pq.QuoteIdentifier(g.schema), verb, privs,
			strings.ToUpper(strings.TrimPrefix(g.kind, "default:")), tail)
	}

	if g.coversAll(objects, actual) {
		return fmt.Sprintf("%s %s ON ALL %sS IN SCHEMA %s%s", verb, privs, strings.ToUpper(g.kind), pq.QuoteIdentifier(g.schema), tail)
	}
	quoted := make([]string, len(objects))
	for i, o := range objects {
		quoted[i] = pq.QuoteIdentifier(g.schema) + "." + pq.QuoteIdentifier(o)
	}
	return fmt.Sprintf("%s %s ON %s %s%s", verb, privs, strings.ToUpper(g.kind), strings.Join(quoted, ","), tail)
}

// coversAll reports whether objects are all the current objects of the
// group's kind in its schema, and there is more than one: a single object is
// clearer named than as ALL ... IN SCHEMA.
func (g *statementGroup) coversAll(objects []string, actual aclState) bool {
	total := 0
	for obj := range actual {
		if obj.kind == g.kind && obj.schema == g.schema {
			total++
		}
	}
	return total > 1 && len(objects) == total
}
