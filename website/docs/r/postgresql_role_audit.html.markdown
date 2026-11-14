---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_role_audit"
sidebar_current: "docs-postgresql-resource-postgresql_role_audit"
description: |-
  Manages the pgaudit.log setting of an existing PostgreSQL role.
---

# postgresql\_role\_audit

The ``postgresql_role_audit`` resource manages the [pgAudit](https://github.com/pgaudit/pgaudit)
`pgaudit.log` setting of an existing role:

* Create/Update: `ALTER ROLE <role> SET pgaudit.log TO '<value>'`
* Delete: `ALTER ROLE <role> RESET pgaudit.log`

The role does not have to be managed by Terraform, so the resource also works for roles
created outside of it, such as the administrative user of a managed database service.

~> **Note:** `pgaudit.log` is a superuser-only parameter. The provider user must be a superuser
or, on PostgreSQL 15 and above, have been granted `SET` on `pgaudit.log`. Managed services
usually allow it for their administrative role. The `pgaudit` library must be loaded via
`shared_preload_libraries` for the setting to have any effect.

## Usage

```hcl
resource "postgresql_role" "app" {
  name  = "app_user"
  login = true
}

resource "postgresql_role_audit" "app" {
  role_name   = postgresql_role.app.name
  log_classes = "read, write"
}

resource "postgresql_role_audit" "admin" {
  role_name   = "admin_user"
  log_classes = "all"
}
```

## Argument Reference

* `role_name` - (Required) The name of an existing role. Changing it forces a new resource.
* `log_classes` - (Required) The value of `pgaudit.log` for the role: a comma-separated
  list of statement classes, e.g. `all`, `none`, `ddl` or `read, write`. Classes can be
  excluded with a `-` prefix, e.g. `all, -misc`.

## Behavior

* If the setting is changed or reset outside of Terraform, the next plan shows the drift and
  apply restores it.
* If the role no longer exists, the resource is removed from the state.
* Destroying the resource resets `pgaudit.log` for the role, so the role falls back to the
  database or server default. Destroying it when the role is already gone is a no-op.

~> **Note:** Do not manage the same role with more than one `postgresql_role_audit` resource.

## Import

The setting can be imported using the role name:

```
$ terraform import postgresql_role_audit.admin admin_user
```
