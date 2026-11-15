---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_security_label"
sidebar_current: "docs-postgresql-resource-postgresql_security_label"
description: |-
  Creates and manages security labels.
---

# postgresql\_security\_label

The ``postgresql_security_label`` resource creates and manages security labels.

See [PostgreSQL documentation](https://www.postgresql.org/docs/current/sql-security-label.html)

~> **Note:** This resource needs Postgresql version 11 or above.

## Usage

```hcl
resource "postgresql_role" "my_role" {
  name  = "my_role"
  login = true
}

resource "postgresql_security_label" "workload" {
  object_type    = "role"
  object_name    = postgresql_role.my_role.name
  label_provider = "pgaadauth"
  label          = "aadauth,oid=00000000-0000-0000-0000-000000000000,type=service"
}
```

## Argument Reference

* `object_type` - (Required) The PostgreSQL object type to apply this security label to. One of: `aggregate`, `column`, `database`, `domain`, `event trigger`, `foreign table`, `function`, `language`, `large object`, `materialized view`, `procedural language`, `procedure`, `publication`, `role`, `routine`, `schema`, `sequence`, `subscription`, `table`, `tablespace`, `type`, `view`.
* `object_name` - (Required) The name of the object to be labeled. The name is written as in SQL, but each part is quoted, so names which contain upper case letters or hyphens need no extra quotes:
  * Objects which reside in a schema (tables, views, types, ...) are written `schema.name`, or `name` when the object is in the `public` schema.
  * A column is written `table.column`, or `schema.table.column`.
  * A function, procedure, routine or aggregate is written `name` or `name(argument types)`, where the argument types are SQL type names, for example `schema.func(integer, text)`. The name is written without parentheses when it has no arguments. The argument list is the last group of balanced parentheses, so type modifiers (`varchar(10)`) and schema-qualified types are accepted, and a name can contain a parenthesis.
  * A large object is written as its OID, for example `16400`.
  * The other objects (databases, roles, schemas, ...) are written with their name.

  A dot cannot be written in the name of an object of a schema, of a column or of a routine (the dot separates the schema); the names of the other objects (roles, databases, ...) can contain one.
* `label_provider` - (Optional) The name of the provider with which this label is to be associated. It can be omitted when only one provider is loaded, in which case the provider is read back from the database.
* `label` - (Required) The value of the security label.
* `database` - (Optional) The database in which the object exists. Defaults to the database of the provider. Labels of the shared objects (databases, roles, tablespaces) are stored in the cluster, so any database can be used for them.

## Import

A security label can be imported with the ID `<database>.<provider>.<object type>.<object name>`, for example:

```shell
terraform import postgresql_security_label.workload mydb.pgaadauth.role.my_role
```

A database or a provider whose name contains a dot or a double quote is written between double quotes in the ID
(a double quote inside is doubled), for example `"my.db".pgaadauth.role.my_role`; the other names are written as they are.
The ID in the state is written the same way.
