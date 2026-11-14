// Command genfixture writes the database fixture and the Terraform configuration
// used by tests/bench/run.sh to benchmark large plans.
//
// It produces three files in -out:
//
//	fixture.sql  - creates the benchmark database(s), schemas and tables (run as
//	               superuser against the maintenance database; uses psql's \connect)
//	cleanup.sql  - drops everything fixture.sql and the Terraform config create
//	main.tf      - a provider block plus a realistic mix of roles, schemas, grants,
//	               default privileges, role memberships and databases
//
// Tables are not Terraform resources (the provider does not manage them), so they are
// created by plain SQL; everything else is created by `terraform apply`.
//
// With -fixture-dbs N > 1 the SQL-managed schemas are spread round-robin over N
// databases (-db, -db_1, ..., -db_N-1), and so are the grants, default privileges
// and Terraform-managed schemas on them. The provider then keeps one connection pool
// per database, which is what a multi-database setup looks like.
//
// -shape count writes the same resources, with the same names and attributes, as one
// block with count per kind of resource instead of one block per resource. Terraform
// core time grows much faster than linearly with the instances of one block, so this
// shows what a config built around a few big count blocks costs.
//
// -core-only writes only main.tf, with every resource replaced by a terraform_data
// resource with the same name, instance count and references. Planning it measures
// Terraform core alone, without the provider and without a database.
//
// -aggregate writes the privileges of every role as one postgresql_role_privileges
// resource per role and database instead of its postgresql_grant and
// postgresql_default_privileges resources; the privileges are the same, so the two
// configurations manage the same database and differ only in the number of instances.
//
// -preset sets the sizes in one go; flags given explicitly override the preset.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type params struct {
	db             string
	prefix         string
	schemas        int
	tables         int
	roles          int
	groups         int
	tfSchemas      int
	databases      int
	objectsPerGrnt int
	fixtureDBs     int
	shape          string
	coreOnly       bool
	aggregate      bool
}

// presets are sizes for -preset. "default" is the flag defaults.
var presets = map[string]map[string]string{
	"default": {},
	"large":   {"roles": "2000", "schemas": "50", "tables": "10000"},
}

func main() {
	var p params
	out := flag.String("out", ".", "output directory")
	flag.StringVar(&p.db, "db", "bench", "name of the fixture database")
	flag.StringVar(&p.prefix, "prefix", "bench", "prefix for every generated role/database name")
	flag.IntVar(&p.schemas, "schemas", 10, "number of SQL-managed schemas that hold tables")
	flag.IntVar(&p.tables, "tables", 1500, "total number of tables, spread evenly over -schemas")
	flag.IntVar(&p.roles, "roles", 200, "number of login roles; most Terraform resources scale with this")
	flag.IntVar(&p.groups, "groups", 20, "number of group (NOLOGIN) roles")
	flag.IntVar(&p.tfSchemas, "tf-schemas", 20, "number of Terraform-managed (empty) schemas")
	flag.IntVar(&p.databases, "databases", 3, "number of Terraform-managed extra databases")
	flag.IntVar(&p.objectsPerGrnt, "objects-per-grant", 10, "tables listed in each postgresql_grant with objects")
	flag.IntVar(&p.fixtureDBs, "fixture-dbs", 1, "number of databases the schemas, grants and default privileges are spread over")
	flag.StringVar(&p.shape, "shape", "blocks", "blocks: one resource block per object; count: one block with count per kind of resource")
	flag.BoolVar(&p.coreOnly, "core-only", false, "write only main.tf, with terraform_data in place of every provider resource (no database needed)")
	flag.BoolVar(&p.aggregate, "aggregate", false, "one postgresql_role_privileges per role instead of its postgresql_grant / postgresql_default_privileges resources (shape blocks only)")
	preset := flag.String("preset", "default", "size preset: "+presetNames()+"; explicit flags override it")
	flag.Parse()

	sizes, ok := presets[*preset]
	if !ok {
		log.Fatalf("unknown -preset %q, want one of %s", *preset, presetNames())
	}
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for name, value := range sizes {
		if !set[name] {
			if err := flag.Set(name, value); err != nil {
				log.Fatal(err)
			}
		}
	}
	if p.shape != "blocks" && p.shape != "count" {
		log.Fatalf("unknown -shape %q, want blocks or count", p.shape)
	}
	if p.aggregate && p.shape != "blocks" {
		log.Fatal("-aggregate needs -shape blocks: a role whose schemas live in two databases needs two instances, which one count block cannot express")
	}

	if p.schemas < 2 || p.tables < p.schemas || p.roles < 1 || p.groups < 1 {
		log.Fatal("need -schemas >= 2, -tables >= -schemas, -roles >= 1, -groups >= 1")
	}
	if p.fixtureDBs < 1 || p.fixtureDBs > p.schemas {
		log.Fatal("need 1 <= -fixture-dbs <= -schemas")
	}
	if p.objectsPerGrnt > p.tables/p.schemas {
		p.objectsPerGrnt = p.tables / p.schemas
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	n := 0
	if p.coreOnly {
		write(filepath.Join(*out, "main.tf"), func(w *bufio.Writer) { n = p.terraform(w) })
		fmt.Printf("wrote %s: %d terraform_data resources, shape %s\n", *out, n, p.shape)
		return
	}
	write(filepath.Join(*out, "fixture.sql"), p.fixtureSQL)
	write(filepath.Join(*out, "cleanup.sql"), p.cleanupSQL)
	write(filepath.Join(*out, "main.tf"), func(w *bufio.Writer) { n = p.terraform(w) })
	fmt.Printf("wrote %s: %d tables in %d schemas in %d database(s), %d terraform resources, shape %s\n",
		*out, p.tables, p.schemas, p.fixtureDBs, n, p.shape)
}

func presetNames() string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func write(path string, fn func(*bufio.Writer)) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	// Generators ignore the results of their writes: bufio.Writer keeps the first
	// error, and Flush below returns it.
	w := bufio.NewWriter(f)
	fn(w)
	if err := w.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

func (p params) tablesIn(schema int) int {
	n := p.tables / p.schemas
	if schema < p.tables%p.schemas {
		n++
	}
	return n
}

// dbName is the name of fixture database k.
func (p params) dbName(k int) string {
	if k == 0 {
		return p.db
	}
	return fmt.Sprintf("%s_%d", p.db, k)
}

// schemaDB is the database that holds SQL-managed schema app_s.
func (p params) schemaDB(s int) string {
	return p.dbName(s % p.fixtureDBs)
}

func (p params) fixtureSQL(w *bufio.Writer) {
	for k := 0; k < p.fixtureDBs; k++ {
		_, _ = fmt.Fprintf(w, "CREATE DATABASE %s;\n\\connect %s\n", p.dbName(k), p.dbName(k))
		for s := k; s < p.schemas; s += p.fixtureDBs {
			_, _ = fmt.Fprintf(w, "CREATE SCHEMA app_%d;\n", s)
			for t := 0; t < p.tablesIn(s); t++ {
				_, _ = fmt.Fprintf(w, "CREATE TABLE app_%d.t_%d (id bigint PRIMARY KEY, payload text);\n", s, t)
			}
		}
	}
}

// cleanupSQL matches names by pattern (psql's \gexec), so it also removes what a
// fixture generated with other sizes or another -fixture-dbs left behind.
func (p params) cleanupSQL(w *bufio.Writer) {
	// Dropping the databases first removes every privilege and default ACL that
	// references the roles, so the roles can be dropped afterwards.
	_, _ = fmt.Fprintf(w, `SELECT format('DROP DATABASE IF EXISTS %%I WITH (FORCE)', datname) FROM pg_database
WHERE datname = '%[1]s' OR datname ~ '^%[1]s_[0-9]+$' OR datname ~ '^%[2]s_db_[0-9]+$'
\gexec
SELECT format('DROP ROLE IF EXISTS %%I', rolname) FROM pg_roles
WHERE rolname ~ '^%[2]s_(user|group)_[0-9]+$'
\gexec
`, p.db, p.prefix)
}

// res is one resource block in main.tf. With count > 0 it is a single block with
// that many instances, and attrs may use count.index.
type res struct {
	typ, name   string
	count       int
	attrs       [][2]string // name, HCL expression
	blocks      []block     // nested blocks, after the attributes
	ignoreRoles bool        // add lifecycle { ignore_changes = [roles] }
}

// block is a nested block of a resource (schema { tables { ... } }). For the
// provider it is written as a block; for terraform_data, where everything is
// one input value, blocks of the same type become a list of objects.
type block struct {
	typ    string
	attrs  [][2]string
	blocks []block
}

// tfWriter renders resources either for the provider or, with core set, as
// terraform_data resources with the same names, instance counts and references, so
// Terraform core does the same graph work without any provider calls.
type tfWriter struct {
	w    *bufio.Writer
	core bool
	n    int // resource instances written
}

func coreName(typ, name string) string {
	return strings.TrimPrefix(typ, "postgresql_") + "_" + name
}

// ref returns a reference to attribute attr of resource typ.name, index is "" or
// an index expression such as "[count.index]".
func (t *tfWriter) ref(typ, name, index, attr string) string {
	if t.core {
		return fmt.Sprintf("terraform_data.%s%s.output.%s", coreName(typ, name), index, attr)
	}
	return fmt.Sprintf("%s.%s%s.%s", typ, name, index, attr)
}

func (t *tfWriter) resource(r res) {
	typ, name, indent := r.typ, r.name, "  "
	if t.core {
		typ, name, indent = "terraform_data", coreName(r.typ, r.name), "    "
	}
	_, _ = fmt.Fprintf(t.w, "\nresource %q %q {\n", typ, name)
	if r.count > 0 {
		_, _ = fmt.Fprintf(t.w, "  count = %d\n\n", r.count)
		t.n += r.count
	} else {
		t.n++
	}
	if t.core {
		_, _ = fmt.Fprint(t.w, "  input = {\n")
	}
	width := 0
	for _, a := range r.attrs {
		width = max(width, len(a[0]))
	}
	for _, a := range r.attrs {
		_, _ = fmt.Fprintf(t.w, "%s%-*s = %s\n", indent, width, a[0], a[1])
	}
	t.blocks(r.blocks, indent)
	switch {
	case t.core:
		_, _ = fmt.Fprint(t.w, "  }\n")
	case r.ignoreRoles:
		_, _ = fmt.Fprint(t.w, `
  # Membership is managed by postgresql_grant_role below.
  lifecycle {
    ignore_changes = [roles]
  }
`)
	}
	_, _ = fmt.Fprint(t.w, "}\n")
}

// blocks writes nested blocks at the given indent: as blocks for the provider,
// as "<type> = [ { ... }, ... ]" lists of objects inside terraform_data's input.
func (t *tfWriter) blocks(blocks []block, indent string) {
	if t.core {
		var types []string
		byType := map[string][]block{}
		for _, b := range blocks {
			if byType[b.typ] == nil {
				types = append(types, b.typ)
			}
			byType[b.typ] = append(byType[b.typ], b)
		}
		for _, typ := range types {
			_, _ = fmt.Fprintf(t.w, "%s%s = [\n", indent, typ)
			for _, b := range byType[typ] {
				_, _ = fmt.Fprintf(t.w, "%s  {\n", indent)
				t.attrs(b.attrs, indent+"    ")
				t.blocks(b.blocks, indent+"    ")
				_, _ = fmt.Fprintf(t.w, "%s  },\n", indent)
			}
			_, _ = fmt.Fprintf(t.w, "%s]\n", indent)
		}
		return
	}
	for _, b := range blocks {
		_, _ = fmt.Fprintf(t.w, "%s%s {\n", indent, b.typ)
		t.attrs(b.attrs, indent+"  ")
		t.blocks(b.blocks, indent+"  ")
		_, _ = fmt.Fprintf(t.w, "%s}\n", indent)
	}
}

func (t *tfWriter) attrs(attrs [][2]string, indent string) {
	width := 0
	for _, a := range attrs {
		width = max(width, len(a[0]))
	}
	for _, a := range attrs {
		_, _ = fmt.Fprintf(t.w, "%s%-*s = %s\n", indent, width, a[0], a[1])
	}
}

func (p params) header(t *tfWriter) {
	if t.core {
		// terraform_data is built into Terraform 1.4+, no provider and no init needed.
		_, _ = fmt.Fprint(t.w, "terraform {\n  required_version = \">= 1.4\"\n}\n")
		return
	}
	_, _ = fmt.Fprint(t.w, `terraform {
  required_providers {
    postgresql = {
      source = "cyrilgdn/postgresql"
    }
  }
}

variable "pg_host" {
  type    = string
  default = "127.0.0.1"
}

variable "pg_port" {
  type    = number
  default = 5432
}

variable "pg_password" {
  type      = string
  default   = "postgres"
  sensitive = true
}

provider "postgresql" {
  host            = var.pg_host
  port            = var.pg_port
  username        = "postgres"
  password        = var.pg_password
  sslmode         = "disable"
  superuser       = true
  connect_timeout = 15
}
`)
}

// terraform writes main.tf and returns the number of resource instances in it.
//
// Per login role i the config has: the role itself, a membership in group i%groups,
// USAGE on schema app_(i%schemas), schema-wide SELECT on its tables (no objects),
// SELECT/INSERT on objectsPerGrnt tables of the next schema (with objects), and for
// every other role a default-privileges entry.
//
// With shape "blocks" every instance is its own resource block; with shape "count"
// each kind of resource above is one block with count.
func (p params) terraform(w *bufio.Writer) int {
	t := &tfWriter{w: w, core: p.coreOnly}
	p.header(t)
	if p.shape == "count" {
		p.terraformCount(t)
	} else {
		p.terraformBlocks(t)
	}
	return t.n
}

func (p params) terraformBlocks(t *tfWriter) {
	for d := 0; d < p.databases; d++ {
		t.resource(res{typ: "postgresql_database", name: fmt.Sprintf("db_%d", d), attrs: [][2]string{
			{"name", fmt.Sprintf("%q", fmt.Sprintf("%s_db_%d", p.prefix, d))},
		}})
	}
	for g := 0; g < p.groups; g++ {
		t.resource(res{typ: "postgresql_role", name: fmt.Sprintf("group_%d", g), attrs: [][2]string{
			{"name", fmt.Sprintf("%q", fmt.Sprintf("%s_group_%d", p.prefix, g))},
		}})
	}
	for s := 0; s < p.tfSchemas; s++ {
		t.resource(res{typ: "postgresql_schema", name: fmt.Sprintf("tf_%d", s), attrs: [][2]string{
			{"name", fmt.Sprintf("%q", fmt.Sprintf("tf_%d", s))},
			{"database", fmt.Sprintf("%q", p.dbName(s%p.fixtureDBs))},
			{"owner", t.ref("postgresql_role", fmt.Sprintf("group_%d", s%p.groups), "", "name")},
		}})
	}

	for i := 0; i < p.roles; i++ {
		own := i % p.schemas
		next := (i + 1) % p.schemas
		user := fmt.Sprintf("user_%d", i)
		role := t.ref("postgresql_role", user, "", "name")
		t.resource(res{typ: "postgresql_role", name: user, ignoreRoles: true, attrs: [][2]string{
			{"name", fmt.Sprintf("%q", fmt.Sprintf("%s_user_%d", p.prefix, i))},
			{"login", "true"},
		}})
		t.resource(res{typ: "postgresql_grant_role", name: user, attrs: [][2]string{
			{"role", role},
			{"grant_role", t.ref("postgresql_role", fmt.Sprintf("group_%d", i%p.groups), "", "name")},
		}})
		if p.aggregate {
			p.aggregateRole(t, i)
			continue
		}
		t.resource(res{typ: "postgresql_grant", name: user + "_usage", attrs: [][2]string{
			{"database", fmt.Sprintf("%q", p.schemaDB(own))},
			{"role", role},
			{"schema", fmt.Sprintf("\"app_%d\"", own)},
			{"object_type", `"schema"`},
			{"privileges", `["USAGE"]`},
		}})
		t.resource(res{typ: "postgresql_grant", name: user + "_all_tables", attrs: [][2]string{
			{"database", fmt.Sprintf("%q", p.schemaDB(own))},
			{"role", role},
			{"schema", fmt.Sprintf("\"app_%d\"", own)},
			{"object_type", `"table"`},
			{"privileges", `["SELECT"]`},
		}})
		t.resource(res{typ: "postgresql_grant", name: user + "_some_tables", attrs: [][2]string{
			{"database", fmt.Sprintf("%q", p.schemaDB(next))},
			{"role", role},
			{"schema", fmt.Sprintf("\"app_%d\"", next)},
			{"object_type", `"table"`},
			{"objects", "[" + p.objectList(i, next) + "]"},
			{"privileges", `["INSERT", "SELECT"]`},
		}})
		if i%2 == 0 {
			t.resource(res{typ: "postgresql_default_privileges", name: user, attrs: [][2]string{
				{"database", fmt.Sprintf("%q", p.schemaDB(own))},
				{"role", role},
				{"owner", `"postgres"`},
				{"schema", fmt.Sprintf("\"app_%d\"", own)},
				{"object_type", `"table"`},
				{"privileges", `["SELECT"]`},
			}})
		}
	}
}

// aggregateRole writes the privileges of login role i as postgresql_role_privileges:
// the same privileges the per-object resources of terraformBlocks grant (USAGE and
// schema-wide SELECT on its own schema, SELECT/INSERT on objectsPerGrnt tables of the
// next schema, default privileges for every other role), as one instance per
// database the role has privileges in.
func (p params) aggregateRole(t *tfWriter, i int) {
	own := i % p.schemas
	next := (i + 1) % p.schemas
	user := fmt.Sprintf("user_%d", i)
	role := t.ref("postgresql_role", user, "", "name")

	ownSchema := block{typ: "schema", attrs: [][2]string{
		{"name", fmt.Sprintf("\"app_%d\"", own)},
		{"usage", "true"},
	}, blocks: []block{
		{typ: "tables", attrs: [][2]string{{"privileges", `["SELECT"]`}}},
	}}
	if i%2 == 0 {
		ownSchema.blocks = append(ownSchema.blocks, block{typ: "default_privileges", attrs: [][2]string{
			{"owner", `"postgres"`},
			{"object_type", `"table"`},
			{"privileges", `["SELECT"]`},
		}})
	}
	nextSchema := block{typ: "schema", attrs: [][2]string{
		{"name", fmt.Sprintf("\"app_%d\"", next)},
	}, blocks: []block{
		{typ: "tables", attrs: [][2]string{
			{"privileges", `["INSERT", "SELECT"]`},
			{"objects", "[" + p.objectList(i, next) + "]"},
		}},
	}}

	schemas := []block{ownSchema, nextSchema}
	if p.schemaDB(own) != p.schemaDB(next) {
		// The next schema lives in another database: a second instance.
		schemas = schemas[:1]
		t.resource(res{typ: "postgresql_role_privileges", name: user + "_next", attrs: [][2]string{
			{"database", fmt.Sprintf("%q", p.schemaDB(next))},
			{"role", role},
		}, blocks: []block{nextSchema}})
	}
	t.resource(res{typ: "postgresql_role_privileges", name: user, attrs: [][2]string{
		{"database", fmt.Sprintf("%q", p.schemaDB(own))},
		{"role", role},
	}, blocks: schemas})
}

// terraformCount writes the same instances as terraformBlocks, grouped into one
// block with count per kind of resource. Names and attribute values are the same;
// only the resource addresses differ (postgresql_role.user[3] instead of user_3).
func (p params) terraformCount(t *tfWriter) {
	dbs := make([]string, p.fixtureDBs)
	for k := range dbs {
		dbs[k] = fmt.Sprintf("%q", p.dbName(k))
	}
	tables := make([]string, p.schemas)
	for s := range tables {
		tables[s] = strconv.Itoa(p.tablesIn(s))
	}
	_, _ = fmt.Fprintf(t.w, "\nlocals {\n  fixture_dbs = [%s]\n  tables_in   = [%s]\n}\n",
		strings.Join(dbs, ", "), strings.Join(tables, ", "))

	// db is the fixture database that holds schema app_<schema>.
	db := func(schema string) string {
		if strings.Contains(schema, " ") {
			schema = "(" + schema + ")"
		}
		return fmt.Sprintf("local.fixture_dbs[%s %% %d]", schema, p.fixtureDBs)
	}
	own := fmt.Sprintf("count.index %% %d", p.schemas)
	next := fmt.Sprintf("(count.index + 1) %% %d", p.schemas)
	group := func(index string) string {
		return t.ref("postgresql_role", "group", fmt.Sprintf("[%s %% %d]", index, p.groups), "name")
	}

	t.resource(res{typ: "postgresql_database", name: "db", count: p.databases, attrs: [][2]string{
		{"name", fmt.Sprintf("\"%s_db_${count.index}\"", p.prefix)},
	}})
	t.resource(res{typ: "postgresql_role", name: "group", count: p.groups, attrs: [][2]string{
		{"name", fmt.Sprintf("\"%s_group_${count.index}\"", p.prefix)},
	}})
	t.resource(res{typ: "postgresql_schema", name: "tf", count: p.tfSchemas, attrs: [][2]string{
		{"name", `"tf_${count.index}"`},
		{"database", db("count.index")},
		{"owner", group("count.index")},
	}})

	role := t.ref("postgresql_role", "user", "[count.index]", "name")
	t.resource(res{typ: "postgresql_role", name: "user", count: p.roles, ignoreRoles: true, attrs: [][2]string{
		{"name", fmt.Sprintf("\"%s_user_${count.index}\"", p.prefix)},
		{"login", "true"},
	}})
	t.resource(res{typ: "postgresql_grant_role", name: "user", count: p.roles, attrs: [][2]string{
		{"role", role},
		{"grant_role", group("count.index")},
	}})
	t.resource(res{typ: "postgresql_grant", name: "usage", count: p.roles, attrs: [][2]string{
		{"database", db(own)},
		{"role", role},
		{"schema", fmt.Sprintf("\"app_${%s}\"", own)},
		{"object_type", `"schema"`},
		{"privileges", `["USAGE"]`},
	}})
	t.resource(res{typ: "postgresql_grant", name: "all_tables", count: p.roles, attrs: [][2]string{
		{"database", db(own)},
		{"role", role},
		{"schema", fmt.Sprintf("\"app_${%s}\"", own)},
		{"object_type", `"table"`},
		{"privileges", `["SELECT"]`},
	}})
	t.resource(res{typ: "postgresql_grant", name: "some_tables", count: p.roles, attrs: [][2]string{
		{"database", db(next)},
		{"role", role},
		{"schema", fmt.Sprintf("\"app_${%s}\"", next)},
		{"object_type", `"table"`},
		{"objects", fmt.Sprintf("[for k in range(%d) : \"t_${(count.index * 7 + k) %% local.tables_in[%s]}\"]", p.objectsPerGrnt, next)},
		{"privileges", `["INSERT", "SELECT"]`},
	}})
	// Every second role: instance k belongs to role 2k.
	every2nd := fmt.Sprintf("count.index * 2 %% %d", p.schemas)
	t.resource(res{typ: "postgresql_default_privileges", name: "user", count: (p.roles + 1) / 2, attrs: [][2]string{
		{"database", db(every2nd)},
		{"role", t.ref("postgresql_role", "user", "[count.index * 2]", "name")},
		{"owner", `"postgres"`},
		{"schema", fmt.Sprintf("\"app_${%s}\"", every2nd)},
		{"object_type", `"table"`},
		{"privileges", `["SELECT"]`},
	}})
}

// objectList picks objectsPerGrnt consecutive tables of schema s, rotating the
// starting point by role so different roles hit different tables.
func (p params) objectList(role, s int) string {
	total := p.tablesIn(s)
	list := ""
	for k := 0; k < p.objectsPerGrnt; k++ {
		if k > 0 {
			list += ", "
		}
		list += fmt.Sprintf("%q", fmt.Sprintf("t_%d", (role*7+k)%total))
	}
	return list
}
