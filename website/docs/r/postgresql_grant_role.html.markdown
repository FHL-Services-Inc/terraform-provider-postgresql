---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_grant_role"
sidebar_current: "docs-postgresql-resource-postgresql_grant_role"
description: |-
  Creates and manages membership in a role to one or more other roles.
---

# postgresql\_grant\_role

The ``postgresql_grant_role`` resource creates and manages membership in a role to one or more other roles in a non-authoritative way.

When using ``postgresql_grant_role`` resource it is likely because the PostgreSQL role you are modifying was created outside of this provider.

~> **Note:** This resource needs PostgreSQL version 9 or above.

## Usage

```hcl
resource "postgresql_grant_role" "grant_root" {
  role              = "root"
  grant_role        = "application"
  with_admin_option = true
}
```

~> **Note:** If you use `postgresql_grant_role` for a role that you also manage with a `postgresql_role` resource, you need to ignore the changes of the `roles` attribute in the `postgresql_role` resource or they will fight over what your role grants should be. `postgresql_role` only rewrites the memberships of the role when `roles` changes, and both resources lock the member role while changing its memberships, so they can be changed in the same apply. e.g.:
```hcl
resource "postgresql_role" "bob" {
  name = "bob"

  lifecycle {
    ignore_changes = [
      roles,
    ]
  }
}

resource "postgresql_grant_role" "bob_admin" {
  role       = "bob"
  grant_role = "admin"
}
```

## Argument Reference

* `role` - (Required) The name of the role that is granted a new membership.
* `grant_role` - (Required) The name of the role that is added to `role`.
* `with_admin_option` - (Optional) Giving ability to grant membership to others or not for `role`. (Default: false)

## Import

`postgresql_grant_role` can be imported using the resource ID,
`<role>_<grant_role>_<with_admin_option>`, where `<with_admin_option>` is
`true` or `false` (it is refreshed from the server either way). As role names
may contain `_` themselves, the provider resolves the ID against the existing
roles; the import fails if no split of the ID matches, or if more than one
does.

```hcl
import {
  to = postgresql_grant_role.grant_root
  id = "root_application_true"
}
```

or, with the CLI:

```
$ terraform import postgresql_grant_role.grant_root root_application_true
```
