---
layout: "postgresql"
page_title: "PostgreSQL: Migrating from cyrilgdn/postgresql"
sidebar_current: "docs-postgresql-guide-migrating-from-cyrilgdn"
description: |-
  How to switch an existing configuration and its state from the cyrilgdn/postgresql provider to FHL-Services-Inc/postgresql.
---

# Migrating from cyrilgdn/postgresql

This provider is a fork of
[cyrilgdn/terraform-provider-postgresql](https://github.com/cyrilgdn/terraform-provider-postgresql).
The resources, data sources and provider settings keep their names, so a
configuration written for `cyrilgdn/postgresql` works with this provider once
Terraform is told to use the new address. This page shows how to switch an
existing configuration and its state.

The fork is published as `FHL-Services-Inc/postgresql` from version 1.28.0 on.
Versions 1.27.0 and earlier exist only as `cyrilgdn/postgresql`.

## 1. Change the source address

Set `source` in the `required_providers` block of the root module:

```hcl
terraform {
  required_providers {
    postgresql = {
      source  = "FHL-Services-Inc/postgresql"
      version = "~> 1.29"
    }
  }
}
```

Do the same in every child module that declares the provider in its own
`required_providers` block. A module that still says `cyrilgdn/postgresql`
keeps using the old provider: Terraform installs both, and on the next apply
the state of that module's resources is moved back to `cyrilgdn/postgresql`.

## 2. Replace the provider in the state

```sh
terraform state replace-provider cyrilgdn/postgresql FHL-Services-Inc/postgresql
```

The command lists the resources it is about to change and asks for
confirmation; add `-auto-approve` to skip the prompt in scripts. It changes
only the state of the currently selected workspace, so run it once for every
workspace (`terraform workspace select <name>`) and for every configuration
that uses the provider. With the local backend, it saves a copy of the state
next to it (`terraform.tfstate.<timestamp>.backup`) before writing.

## 3. Install the new provider

```sh
terraform init -upgrade
```

This installs `FHL-Services-Inc/postgresql` and updates the dependency lock
file `.terraform.lock.hcl`. Commit the updated lock file together with the
configuration change.

Check the result with `terraform providers`. Both the configuration and the
state should list only `registry.terraform.io/fhl-services-inc/postgresql`;
if `cyrilgdn/postgresql` still appears, a module or a workspace was missed.

## 4. Plan and apply

Run `terraform plan` before applying. Versions 1.28.0 and later change some
behavior compared to upstream v1.27.0, and some of these changes show up as a
diff or an error on the first plan. Read the entries for 1.28.0, 1.29.0 and
any later release, including the upgrade notes of 1.29.0, in the
[changelog](https://github.com/FHL-Services-Inc/terraform-provider-postgresql/blob/main/CHANGELOG.md).

Since 1.29.0, the state of `postgresql_role` has a new schema version. `plan`
and `apply` upgrade it automatically, and `apply` saves it in the new format.
Until that first apply, `terraform state show` on a role may fail with
"schema version 0 ... does not match version 1 from the provider".

## Rolling back

Before the first apply with this provider, the same steps in reverse return
the configuration to `cyrilgdn/postgresql`: set `source` back to
`cyrilgdn/postgresql` with a version constraint that matches 1.27.0, run

```sh
terraform state replace-provider FHL-Services-Inc/postgresql cyrilgdn/postgresql
terraform init -upgrade
```

and check `terraform providers` again.

After an apply, the state of `postgresql_role` is saved in the new schema
version, which `cyrilgdn/postgresql` v1.27.0 can't read: its plan fails with
"Resource instance managed by newer provider version".

## Versions

The fork continues the 1.x line of the upstream provider and follows semantic
versioning. Its version numbers are independent of upstream: if upstream ever
publishes another release, its number says nothing about the contents of a
release of this provider with the same number.
