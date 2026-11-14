---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_role_privileges"
sidebar_current: "docs-postgresql-resource-postgresql_role_privileges"
description: |-
  Manages all privileges of one role in one database, for the listed schemas, as a single resource.
---

# postgresql\_role\_privileges

The ``postgresql_role_privileges`` resource manages every privilege one role holds in one database, for the schemas it lists: schema `USAGE`/`CREATE`, table and sequence privileges and per-schema default privileges. One instance replaces the set of ``postgresql_grant`` and ``postgresql_default_privileges`` resources you would otherwise write for that role.

It is meant for large configurations. A refresh is one statement per instance and an apply is one transaction per instance that applies only the difference between the configuration and what the role actually holds, instead of one read and one write per grant. With hundreds of roles and thousands of grants this cuts `terraform plan` and `terraform apply` from a per-grant to a per-role cost.

See [PostgreSQL documentation](https://www.postgresql.org/docs/current/sql-grant.html)

~> **Note:** This resource needs Postgresql version 9 or above.

~> **Note:** Function, procedure and routine privileges, database-level privileges (`CONNECT`, `CREATE`, `TEMPORARY`) and database-wide default privileges (without `IN SCHEMA`) are not yet supported by this resource. Keep using ``postgresql_grant`` / ``postgresql_default_privileges`` for those; they do not overlap with what this resource manages.

## Usage

```hcl
resource "postgresql_role_privileges" "app_ro" {
  database  = "app"
  role      = "app_ro"
  exclusive = false

  schema {
    name   = "public"
    usage  = true
    create = false

    # one block = one exact set of privileges on its objects
    tables { privileges = ["SELECT"] }                                  # no objects: every current table of the schema
    tables { privileges = ["SELECT", "INSERT"], objects = ["orders"] }  # union with the block above on "orders"
    sequences { privileges = ["USAGE", "SELECT"] }

    default_privileges {
      owner       = "app_owner"
      object_type = "table"
      privileges  = ["SELECT"]
    }
    default_privileges {
      owner       = "app_owner"
      object_type = "sequence"
      privileges  = ["USAGE"]
    }
  }

  schema {
    name  = "audit"
    usage = true
    tables {
      privileges        = ["SELECT"]
      with_grant_option = true
    }
  }
}
```

## Argument Reference

* `database` - (Required) The database the privileges are held in. Changing it forces a new resource.
* `role` - (Required) The role holding the privileges. Set it to `"public"` for the `PUBLIC` pseudo-role. Changing it forces a new resource.
* `exclusive` - (Optional) When `true`, also revoke every privilege the role holds in the listed schemas that the configuration does not mention. The scope is the listed schemas only, never the whole database. Defaults to `false`.
* `schema` - (Optional) One block per schema. A schema block manages the schema-level privileges of the role exactly: with `usage = false` the resource revokes `USAGE`. Each schema may be listed once.
  * `name` - (Required) The schema name.
  * `usage` - (Optional) Whether the role holds `USAGE` on the schema. Defaults to `false`.
  * `create` - (Optional) Whether the role holds `CREATE` on the schema. Defaults to `false`.
  * `tables` - (Optional) Repeatable. Privileges on tables of the schema, see below.
  * `sequences` - (Optional) Repeatable. Privileges on sequences of the schema, see below.
  * `default_privileges` - (Optional) Repeatable. Default privileges granted to the role on new objects of the schema, see below.

A `tables` or `sequences` block supports:

* `privileges` - (Required) The exact privileges the role holds on the objects of the block (`SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, `MAINTAIN` on PostgreSQL 17+ for tables; `USAGE`, `SELECT`, `UPDATE` for sequences). `ALL` is accepted and expanded to the full list for the object type. An empty set revokes everything on those objects.
* `objects` - (Optional) The object names the block applies to. An empty set (the default) means every object of that kind that exists in the schema at the time of the refresh or apply.
* `with_grant_option` - (Optional) Whether the role may grant these privileges to others. Defaults to `false`.

A `default_privileges` block supports:

* `owner` - (Required) The role whose default privileges are altered: objects it creates in the schema get the privileges below. One block is one `ALTER DEFAULT PRIVILEGES FOR ROLE owner IN SCHEMA name`.
* `object_type` - (Required) One of `table`, `sequence`, `function`, `type`.
* `privileges` - (Required) The exact default privileges. An empty set removes the default privileges for the role.
* `with_grant_option` - (Optional) Whether the default privileges are granted with grant option. Defaults to `false`. Cannot be `true` when `role` is `"public"`.

## Attributes Reference

* `id` - `<database>/<role>`.
* `drift` - Computed. What the last refresh found different from the configuration, one entry per object, for example `table public.orders: -SELECT` or `sequence public.orders_id_seq: +UPDATE`. `-PRIV` is configured but not held, `+PRIV` is held but not configured, a trailing `*` marks the grant option (`-SELECT*`: configured with grant option, held without; `+SELECT*`: held with grant option, configured without). A listed schema or object that is missing shows as `schema audit: does not exist`. The list is empty when the role is in sync and capped at 50 entries. A non-empty `drift` plans an update, so a single revoked table is visible by name in `terraform plan`:

```
  ~ resource "postgresql_role_privileges" "app_ro" {
      ~ drift = [
          - "table public.orders: -SELECT",
        ] -> (known after apply)
    }
```

## Semantics

* **Exact per object.** For every object the configuration covers, the role ends up with exactly the union of the privileges of the blocks that cover it, nothing more. In the example above, `orders` gets `SELECT` and `INSERT`; every other table gets `SELECT` only. Privileges the role holds on a covered object but the configuration does not list are revoked, whatever `exclusive` is set to. Two blocks that cover the same object with different `with_grant_option` are an error.
* **All current objects.** A `tables` or `sequences` block without `objects` expands to the objects that exist in the schema at refresh or apply time, as `postgresql_grant` does with an empty `objects`. A table created afterwards shows up as `-SELECT` drift on the next plan and is granted on the next apply; `default_privileges` is the way to cover objects before they exist.
* **Tables** cover what `GRANT ... ON ALL TABLES IN SCHEMA` covers: ordinary, partitioned and foreign tables, views and materialized views. ``postgresql_grant`` only reads back ordinary tables, so a configuration migrated from it may show drift on views that was always there.
* **`exclusive = true`** extends the comparison to everything the role holds in the listed schemas: every table and sequence, the schema itself and every default-privileges entry of any owner. Whatever is not in the configuration is revoked. Schemas that are not listed are never touched; for whole-database exclusivity, list every schema.
* **`exclusive = false`** (the default) leaves objects, schemas and default-privileges entries the configuration does not mention alone, so the role may hold privileges on them through other resources or manual grants.
* **Missing schema or object.** A listed schema or a named object that does not exist is not a reason to drop the instance or to touch the rest: it appears in `drift` as `... does not exist`, and apply fails until the configuration or the database is fixed. A missing role or database removes the instance from state.
* **Delete** revokes exactly what the blocks cover (everything the role holds in the listed schemas when `exclusive` is `true`).
* **Writes** take an advisory lock on the role and on every listed schema, re-read the actual privileges under those locks and apply only the difference in one transaction. A change made outside Terraform between plan and apply is still handled, and a retry after a failure applies only what is still missing.
* **One resource per (role, object).** Do not manage the same (role, object) pair with a ``postgresql_grant`` (or a second ``postgresql_role_privileges``) at the same time: both apply an exact set and will undo each other on every apply. Migrate every grant of a (database, role) pair in one step, see below. Different roles on the same objects are fine. Membership (``postgresql_grant_role``) is unrelated; privileges inherited through membership are not modelled.
* **Privileges on a non-superuser connection** (for example RDS) are changed as the owner of each object: the resource grants the owners of the listed schemas and objects to the connecting user for the duration of the transaction, as the other grant resources do.

## Import

A ``postgresql_role_privileges`` instance is imported by `<database>/<role>`:

```
terraform import postgresql_role_privileges.app_ro app/app_ro
```

The importer reads everything the role holds in every user schema of the database and lays it out as a `schema` block per schema, one `tables`/`sequences` block per distinct set of privileges with explicit `objects`, and one `default_privileges` block per owner and object type. It cannot know that "SELECT on these 200 tables" was meant as "all tables", so `objects` is always explicit and `exclusive` is `false`; `terraform plan -generate-config-out` works through `import` blocks the same way.

To migrate existing ``postgresql_grant`` / ``postgresql_default_privileges`` resources to one instance, forget the old ones without revoking anything and import the new one in the same apply (`moved` cannot change the resource type):

```hcl
removed {
  from = postgresql_grant.app_ro_usage
  lifecycle { destroy = false }
}
removed {
  from = postgresql_grant.app_ro_tables
  lifecycle { destroy = false }
}
removed {
  from = postgresql_default_privileges.app_ro_tables
  lifecycle { destroy = false }
}

import {
  to = postgresql_role_privileges.app_ro
  id = "app/app_ro"
}

resource "postgresql_role_privileges" "app_ro" {
  database = "app"
  role     = "app_ro"
  schema {
    name  = "public"
    usage = true
    tables { privileges = ["SELECT"] }
    default_privileges {
      owner       = "app_owner"
      object_type = "table"
      privileges  = ["SELECT"]
    }
  }
}
```

The old instances leave the state without a `REVOKE`, the new one is imported, and its first plan is a no-op (or a configuration-shape update that executes no statement when the imported explicit object list differs from an all-in-schema block in the configuration). Remove the `removed` blocks afterwards.
