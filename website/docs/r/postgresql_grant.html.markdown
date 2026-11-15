---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_grant"
sidebar_current: "docs-postgresql-resource-postgresql_grant"
description: |-
  Creates and manages privileges given to a user for a database schema.
---

# postgresql\_grant

The ``postgresql_grant`` resource creates and manages privileges given to a user for a database schema.

See [PostgreSQL documentation](https://www.postgresql.org/docs/current/sql-grant.html)

~> **Note:** This resource needs Postgresql version 9 or above.
~> **Note:** Using column & table grants on the _same_ table with the _same_ privileges can lead to unexpected behaviours.

## Usage

```hcl
# Grant SELECT privileges on 2 tables
resource "postgresql_grant" "readonly_tables" {
  database    = "test_db"
  role        = "test_role"
  schema      = "public"
  object_type = "table"
  objects     = ["table1", "table2"]
  privileges  = ["SELECT"]
}

# Grant SELECT & INSERT privileges on 2 columns in 1 table
resource "postgresql_grant" "read_insert_column" {
  database    = "test_db"
  role        = "test_role"
  schema      = "public"
  object_type = "column"
  objects     = ["table1"]
  columns     = ["col1", "col2"]
  privileges  = ["UPDATE", "INSERT"]
}
```

## Argument Reference

* `role` - (Required) The name of the role to grant privileges on, Set it to "public" for all roles.
* `database` - (Required) The database to grant privileges on for this role. With `object_type = "database"` the provider does not connect to this database: the grant is read and written on the provider's own `database`, so it keeps working after `CONNECT` is revoked from `public`.
* `schema` - The database schema to grant privileges on for this role (Required except if object_type is "database")
* `object_type` - (Required) The PostgreSQL object type to grant the privileges on (one of: database, schema, table, sequence, function, procedure, routine, foreign_data_wrapper, foreign_server, column).
* `privileges` - (Required) The list of privileges to grant. There are different kinds of privileges: SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, CREATE, CONNECT, TEMPORARY, EXECUTE, and USAGE. An empty list could be provided to revoke all privileges for this role. When the objects covered by the grant drift apart, a refresh records a privilege held by only some of them as `"<PRIVILEGE> (on some objects only)"`, so the plan shows it until the next apply aligns every object. On refresh, an object whose privileges were never granted or revoked explicitly is read with its default privileges (`acldefault`): everything for its owner, `EXECUTE` for `public` on functions, `CONNECT` and `TEMPORARY` for `public` on databases.
* `objects` - (Optional) The objects upon which to grant the privileges. An empty list (the default) means to grant permissions on *all* objects of the specified type. You cannot specify this option if the `object_type` is `database` or `schema`. When `object_type` is `column`, only one value is allowed. For `function`, `procedure` and `routine`, an entry can carry an argument type list to select one overload (e.g. `"my_func(integer, text)"`); a bare name refers to the routine of that name (PostgreSQL requires it to be unique when granting). On refresh, each overload covered by the grant is checked: a signature entry checks that overload (any spelling PostgreSQL accepts for the types, e.g. `int4` or `integer`), a bare name every overload of that name, and an empty list every routine of the schema. `function` covers plain, aggregate and window functions, `procedure` only procedures, `routine` both. A listed routine that does not exist is not checked. Argument modes and names are accepted as in `GRANT` (e.g. `"my_func(IN x integer, OUT y text)"`, where `OUT` arguments are not part of the signature).
* `columns` - (Optional) The columns upon which to grant the privileges. Required when `object_type` is `column`. You cannot specify this option if the `object_type` is not `column`.
* `with_grant_option` - (Optional) Whether the recipient of these privileges can grant the same privileges to others. Defaults to false.

### Notes

* **`object_type = "table"` covers every kind of relation `GRANT ... ON TABLE`
  accepts:** ordinary and partitioned tables, views, materialized views and
  foreign tables, and so does `object_type = "column"`. This applies to
  reading the privileges back too: with `objects` empty, every such relation in
  the schema must have the configured privileges, and a missing privilege on
  any of them (for example on a view created after the grant, or revoked
  outside of Terraform) shows up as a change that the next apply grants
  again. Earlier versions only read back ordinary tables, so the first plan
  after upgrading can show such changes; a column grant on a view is no longer
  replaced on every plan.
* **`object_type = "database"` with a non-superuser provider:** PostgreSQL
  ignores (with only a warning) a `GRANT ... ON DATABASE` from a role that
  neither owns the database nor has grant option on it. As for schemas and
  tables, the provider therefore temporarily grants itself membership in the
  database owner while it runs the statement, and revokes it afterwards. The
  provider's role needs the right to grant the owner role to itself (`ADMIN
  OPTION` on it, or `CREATEROLE` before PostgreSQL 16).
* Privileges are read with a single statement per resource, outside of a
  transaction.


## Examples

Revoke default accesses for public schema:

```hcl
resource "postgresql_grant" "revoke_public" {
  database    = "test_db"
  role        = "public"
  schema      = "public"
  object_type = "schema"
  privileges  = []
}
```

## Concurrency

Several `postgresql_grant` resources on the same objects (for example, one per
role, plus one for `public`) can be applied in parallel. Before it changes
privileges, a grant takes PostgreSQL advisory locks on the grantee role and on
the objects whose privileges it changes. Grants on the same objects then wait
for each other instead of failing with `tuple concurrently updated`:

* a grant on all objects of a type in a schema (no `objects`) waits for every
  other grant on objects of that type in that schema;
* grants on listed `objects` wait for each other only if they share an object;
* `function`, `procedure` and `routine` grants count as one type, and a
  `column` grant counts as a grant on its table.

Advisory locks are scoped to one database. Writers of the same objects always
run in the database the objects are in, so this covers parallel applies and
several Terraform processes. The members of the grantee are not locked. As a
last resort, a write that still fails with `tuple concurrently updated` (for
example because another tool changed the same privileges) is retried once.

## Import

`postgresql_grant` can be imported using the resource ID, which is the
attributes joined with `_`:

* `<role>_<database>_<object_type>` for `object_type = "database"`,
* `<role>_<database>_<object_type>_<object>` for `foreign_data_wrapper` and `foreign_server`,
* `<role>_<database>_<schema>_<object_type>` for `schema`, and for `table`,
  `sequence`, `function`, `procedure` and `routine` grants on all objects of the schema
  (no `objects`),
* `<role>_<database>_<schema>_<object_type>_<object>[_<object>...]` for grants on
  listed `objects`, in any order.

Use `public` as the role for grants to `PUBLIC`. As names may contain `_`
themselves, the provider resolves the ID against the roles, databases, schemas
and objects that exist on the server; the import fails if no split of the ID
matches, or if more than one does (e.g. both `a_b` and `a` + `b` are tables of
the schema). Grants with `object_type = "column"` cannot be imported: their ID
does not identify the privilege.

`privileges` and `with_grant_option` are read from the server. For a grant
without `objects`, `privileges` is read from the objects of the schema the
same way as on refresh, so the imported grant only shows no changes if all of
them have the same privileges.

```hcl
import {
  to = postgresql_grant.readonly_tables
  id = "readonly_role_test_db_public_table"
}

import {
  to = postgresql_grant.readonly_two_tables
  id = "readonly_role_test_db_public_table_table1_table2"
}

import {
  to = postgresql_grant.connect_db
  id = "readonly_role_test_db_database"
}
```

or, with the CLI:

```
$ terraform import postgresql_grant.readonly_tables readonly_role_test_db_public_table
```
