---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_user_mapping"
sidebar_current: "docs-postgresql-resource-postgresql_user_mapping"
description: |-
  Creates and manages a user mapping on a PostgreSQL server.
---

# postgresql\_user\_mapping

The ``postgresql_user_mapping`` resource creates and manages a user mapping on a PostgreSQL server.


## Usage

```hcl
resource "postgresql_extension" "ext_postgres_fdw" {
  name = "postgres_fdw"
}

resource "postgresql_server" "myserver_postgres" {
  server_name = "myserver_postgres"
  fdw_name    = "postgres_fdw"
  options = {
    host   = "foo"
    dbname = "foodb"
    port   = "5432"
  }

  depends_on = [postgresql_extension.ext_postgres_fdw]
}

resource "postgresql_role" "remote" {
  name = "remote"
}

resource "postgresql_user_mapping" "remote" {
  server_name = postgresql_server.myserver_postgres.server_name
  user_name   = postgresql_role.remote.name
  options = {
    user = "admin"
    password = "pass"
  }
}
```

## Argument Reference

* `user_name` - (Required) The name of an existing user that is mapped to foreign server. `CURRENT_USER`, `CURRENT_ROLE` and `USER`, written in upper case, map the role the provider connects as; they are kept in the state as written, and resolved to that role whenever the mapping is read. Any other spelling, such as `"user"`, is a role name. When PUBLIC is specified (in any case, e.g. `"public"` or `"PUBLIC"`), a so-called public mapping is created that is used when no user-specific mapping is applicable. A role whose name is a case variant of `public`, such as `"PUBLIC"`, therefore cannot be mapped with this resource.
Changing this value
  will force the creation of a new resource as this value can only be set
  when the user mapping is created.
* `server_name` - (Required) The name of an existing server for which the user mapping is to be created.
Changing this value
  will force the creation of a new resource as this value can only be set
  when the user mapping is created.
* `options` - (Optional) This clause specifies the options of the user mapping. The options typically define the actual user name and password of the mapping. Option names must be unique. The allowed option names and values are specific to the server's foreign-data wrapper.

  When the provider connects as a non-superuser (e.g. the master user on AWS
  RDS or Cloud SQL), PostgreSQL does not show the options of another role's
  user mapping, even to the owner of the server. In that case the provider
  keeps the options from the Terraform state, so changes made to them outside
  of Terraform are not detected.

## Import

`postgresql_user_mapping` can be imported using the resource ID,
`<user_name>.<server_name>`. For the PUBLIC mapping, use `public` or `PUBLIC`
as `<user_name>`, spelled as in your configuration, and likewise
`CURRENT_USER`, `CURRENT_ROLE` or `USER` for the mapping of the provider role. As role and server names
may contain `.` themselves, the provider resolves the ID against the existing
user mappings; the import fails if no split of the ID matches a mapping, or if
more than one does. When the provider connects as a non-superuser, the options
of another role's mapping are not visible, so they are not imported.

```hcl
import {
  to = postgresql_user_mapping.remote
  id = "remote.myserver_postgres"
}
```
