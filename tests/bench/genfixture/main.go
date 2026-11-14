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
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	flag.Parse()

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

	write(filepath.Join(*out, "fixture.sql"), p.fixtureSQL)
	write(filepath.Join(*out, "cleanup.sql"), p.cleanupSQL)
	n := 0
	write(filepath.Join(*out, "main.tf"), func(w *bufio.Writer) { n = p.terraform(w) })
	fmt.Printf("wrote %s: %d tables in %d schemas in %d database(s), %d terraform resources\n",
		*out, p.tables, p.schemas, p.fixtureDBs, n)
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

// terraform writes main.tf and returns the number of resources in it.
//
// Per login role i the config has: the role itself, a membership in group i%groups,
// USAGE on schema app_(i%schemas), schema-wide SELECT on its tables (no objects),
// SELECT/INSERT on objectsPerGrnt tables of the next schema (with objects), and for
// every other role a default-privileges entry.
func (p params) terraform(w *bufio.Writer) int {
	n := 0
	_, _ = fmt.Fprint(w, `terraform {
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

	for d := 0; d < p.databases; d++ {
		_, _ = fmt.Fprintf(w, "\nresource \"postgresql_database\" \"db_%d\" {\n  name = \"%s_db_%d\"\n}\n", d, p.prefix, d)
		n++
	}
	for g := 0; g < p.groups; g++ {
		_, _ = fmt.Fprintf(w, "\nresource \"postgresql_role\" \"group_%d\" {\n  name = \"%s_group_%d\"\n}\n", g, p.prefix, g)
		n++
	}
	for s := 0; s < p.tfSchemas; s++ {
		_, _ = fmt.Fprintf(w, `
resource "postgresql_schema" "tf_%d" {
  name     = "tf_%d"
  database = %q
  owner    = postgresql_role.group_%d.name
}
`, s, s, p.dbName(s%p.fixtureDBs), s%p.groups)
		n++
	}

	for i := 0; i < p.roles; i++ {
		own := i % p.schemas
		next := (i + 1) % p.schemas
		_, _ = fmt.Fprintf(w, `
resource "postgresql_role" "user_%[1]d" {
  name  = "%[2]s_user_%[1]d"
  login = true

  # Membership is managed by postgresql_grant_role below.
  lifecycle {
    ignore_changes = [roles]
  }
}

resource "postgresql_grant_role" "user_%[1]d" {
  role       = postgresql_role.user_%[1]d.name
  grant_role = postgresql_role.group_%[3]d.name
}

resource "postgresql_grant" "user_%[1]d_usage" {
  database    = %[4]q
  role        = postgresql_role.user_%[1]d.name
  schema      = "app_%[5]d"
  object_type = "schema"
  privileges  = ["USAGE"]
}

resource "postgresql_grant" "user_%[1]d_all_tables" {
  database    = %[4]q
  role        = postgresql_role.user_%[1]d.name
  schema      = "app_%[5]d"
  object_type = "table"
  privileges  = ["SELECT"]
}

resource "postgresql_grant" "user_%[1]d_some_tables" {
  database    = %[8]q
  role        = postgresql_role.user_%[1]d.name
  schema      = "app_%[6]d"
  object_type = "table"
  objects     = [%[7]s]
  privileges  = ["INSERT", "SELECT"]
}
`, i, p.prefix, i%p.groups, p.schemaDB(own), own, next, p.objectList(i, next), p.schemaDB(next))
		n += 5

		if i%2 == 0 {
			_, _ = fmt.Fprintf(w, `
resource "postgresql_default_privileges" "user_%[1]d" {
  database    = %[2]q
  role        = postgresql_role.user_%[1]d.name
  owner       = "postgres"
  schema      = "app_%[3]d"
  object_type = "table"
  privileges  = ["SELECT"]
}
`, i, p.schemaDB(own), own)
			n++
		}
	}
	return n
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
