---
layout: "postgresql"
page_title: "PostgreSQL: Large configurations"
sidebar_current: "docs-postgresql-guide-large-configurations"
description: |-
  How to keep plan and apply fast with thousands of roles, schemas and grants.
---

# Large configurations

Terraform refreshes every resource in state on every `terraform plan` and
`terraform apply`. With thousands of roles, grants and default privileges, how the
configuration is laid out matters as much as the provider itself. This page explains
where the time goes and how to structure large configurations.

## Where the time goes

A plan costs roughly:

```
plan time ≈ Terraform core per instance × instances
          + provider per instance × instances
          + statements × round-trip time / parallelism
```

* **Terraform core.** Building and walking the graph costs about 0.3 ms per
  resource instance when every instance is its own resource block: 10,000
  instances take about 3 s, 50,000 about 14 s. This holds without any provider.
* **One block with thousands of instances is much slower.** Terraform core time
  grows with the square of the instances of a single resource block. Measured with
  Terraform 1.15 and the built-in `terraform_data` resource, so without this
  provider:

  | instances | separate blocks | one block with `count` |
  |---:|---:|---:|
  | 1,000 | 0.4 s | 0.4 s |
  | 3,000 | about 1 s | 3.0 s |
  | 10,000 | 2.8 s | 36 s; the apply did not finish in 7 minutes |

  One `for_each` block with 3,000 keys built with `setproduct` took 94 s to plan.
* **The provider.** Each instance costs another 1.5–3 ms in the plugin protocol and
  the provider's Read, even with no network latency: 10,000 instances add 15–30 s.
* **Round trips.** Every Read runs one or more SQL statements. A local server hides
  this cost; against a server 10 ms away, a few thousand statements at the default
  `-parallelism=10` add tens of seconds.

The first two items are Terraform's own cost, so the layout of the configuration is
the cheapest thing to fix.

## Recommendations

### Avoid one huge `count` or `for_each` block

Split large sets of resources into several blocks or module calls, for example one
per database or per schema, and keep each `count`/`for_each` to a few hundred
instances:

```hcl
# Slow: one block with 10,000 instances
resource "postgresql_grant" "tables" {
  for_each = local.all_table_grants
  # ...
}

# Faster: one module call per database, a few hundred instances each
module "grants_app" {
  source   = "./modules/database-grants"
  database = "app"
  grants   = local.table_grants["app"]
}

module "grants_billing" {
  source   = "./modules/database-grants"
  database = "billing"
  grants   = local.table_grants["billing"]
}
```

Moving existing resources to new addresses does not need to recreate them: use
`moved` blocks or `terraform state mv`.

### Use one root module and state per database

Give each database (or group of related databases) its own root module and state.
Each run then refreshes only its own resources, and runs for different databases can
go in parallel, for example as separate CI jobs. Keep cluster-wide objects such as
roles and databases in a separate root module and pass their names to the others as
variables or through `terraform_remote_state`.

### Raise `-parallelism` together with the connection limits

`-parallelism` (default 10) is how many resources Terraform handles at the same time.
When plans are slow because of round trips to a remote server, `-parallelism=20` to
`40` shortens them considerably.

Every resource Terraform is working on needs a database connection, so the provider
opens up to about `-parallelism` connections at once. The provider keeps one connection pool per
database it connects to, and `max_connections` in the provider block (default 20)
limits each pool, not the server as a whole. A configuration that touches several
databases can therefore open up to `max_connections` connections to each of them.
Before raising `-parallelism`, check that the server's `max_connections` setting (and
any connection pooler in between) leaves room for that next to your other clients.
Raising `-parallelism` well above the provider's `max_connections` only makes
resources wait for a free connection.

### `-refresh=false` for configuration-only plans

`terraform plan -refresh=false` skips the Read of every resource and compares the
configuration with the state only. It needs no round trips to PostgreSQL and is
useful for quick feedback while editing.

The risk: changes made outside Terraform (a privilege revoked by hand, a dropped
role) do not show up, and the plan is based on what the state says rather than what
the server has. Do not apply such a plan; run a normal plan before every apply, and
keep a regular plan with refresh to detect drift.

### Use `-target` only as an exception

`-target` plans only the given resources and what they depend on. It can help to fix
one resource in a huge state, but the result is a partial view: Terraform warns that
the plan may be incomplete, other changes and drift stay hidden, and it is easy to
leave the state out of step with the configuration. Splitting the configuration into
smaller states, as above, gives the same speed without these problems.
