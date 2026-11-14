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
* `database` - (Required) The database to grant privileges on for this role.
* `schema` - The database schema to grant privileges on for this role (Required except if object_type is "database")
* `object_type` - (Required) The PostgreSQL object type to grant the privileges on (one of: database, schema, table, sequence, function, procedure, routine, foreign_data_wrapper, foreign_server, column).
* `privileges` - (Required) The list of privileges to grant. There are different kinds of privileges: SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, CREATE, CONNECT, TEMPORARY, EXECUTE, and USAGE. An empty list could be provided to revoke all privileges for this role.
* `objects` - (Optional) The objects upon which to grant the privileges. An empty list (the default) means to grant permissions on *all* objects of the specified type. You cannot specify this option if the `object_type` is `database` or `schema`. When `object_type` is `column`, only one value is allowed.
* `columns` - (Optional) The columns upon which to grant the privileges. Required when `object_type` is `column`. You cannot specify this option if the `object_type` is not `column`.
* `with_grant_option` - (Optional) Whether the recipient of these privileges can grant the same privileges to others. Defaults to false.


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
the schema).

Grants with `object_type = "column"` have their own ID, which includes the
privilege (two column grants on the same columns differ only by it):
`<role>/<database>/<schema>/column/<table>/<column>[,<column>...]/<privilege>`.
Columns can be listed in any order; `%`, `/` and `,` in names are written as
`%25`, `%2F` and `%2C`. Column grants created with an earlier provider version
had an `_`-joined ID without the privilege; their state is upgraded to the new
ID automatically.

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

import {
  to = postgresql_grant.insert_columns
  id = "test_role/test_db/public/column/table1/col1,col2/INSERT"
}
```

or, with the CLI:

```
$ terraform import postgresql_grant.readonly_tables readonly_role_test_db_public_table
```
