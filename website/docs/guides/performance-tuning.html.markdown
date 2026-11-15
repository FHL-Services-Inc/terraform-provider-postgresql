---
layout: "postgresql"
page_title: "PostgreSQL: Performance tuning"
sidebar_current: "docs-postgresql-guide-performance-tuning"
description: |-
  Provider settings that make plan and apply faster with many resources: connection reuse, single round-trip statements, a shared catalog snapshot, and how to size and measure them.
---

# Performance tuning

Terraform refreshes every resource in state on every `terraform plan` and
`terraform apply`. With hundreds or thousands of roles, grants and default
privileges, most of that time is spent waiting for the network, not in
PostgreSQL. This page explains where the time goes, which provider settings
reduce it, what they cost, and how to measure the effect on your own
configuration.

All settings on this page are opt-in. With the defaults, the provider behaves
as it always has.

## Why large plans are slow

For every resource in state, Terraform calls the provider's Read, and the
provider runs one or more SQL statements. Two things multiply the cost of each
statement:

* **A new connection for almost every statement.** By default
  (`max_idle_connections = 0`) the provider closes each connection as soon as
  it is returned to its pool, so the next statement outside a transaction
  opens a new one. Every new connection costs a TCP handshake, the TLS
  handshake with `sslmode = require` or stricter, and the PostgreSQL startup
  and authentication exchange: several round-trips before the statement is
  even sent. In the benchmark below, a plan of 1,143 resources ran about
  1,160 statements and opened about 1,180 connections with the defaults
  (before the read optimizations of this release: about 7,000 statements and
  2,600 connections).
* **Two round-trips per parameterized statement.** By default the driver
  ([`lib/pq`](https://pkg.go.dev/github.com/lib/pq)) prepares each statement
  that has bind parameters (almost every query the provider runs) and executes
  it in a second exchange.

Against a server on the same machine a round-trip costs almost nothing and
none of this shows. Against a server 10 ms away (another region, a VPN, a
bastion), it dominates the plan.

## `max_idle_connections`: reuse connections

```hcl
provider "postgresql" {
  # ...
  max_idle_connections = 10
}
```

or, without changing the configuration, `export PGMAXIDLECONNECTIONS=10`.

With a value above zero, each connection pool keeps up to that many idle
connections open and the provider reuses them instead of reconnecting. In the
benchmark below this took a plan from about 1,180 new connections to 38, and
the plan time at 10 ms round-trip time from 12.9 s to 5.2 s.

### Recommended values

* Set it to the `-parallelism` you run Terraform with (10 by default). That
  gives the most reuse; the benchmark below found 5 almost as fast.
* If the provider manages objects in many databases, or the server is short on
  connection slots, use a lower value such as 2 to 5. Even 2 kept most of the
  gain in the measurements below.
* Keep `max_connections` (default `20`) at or above `-parallelism`. A smaller
  pool makes resources queue for a connection: in the measurements below,
  `max_connections = 5` lost almost all of the gain at 10 ms.

### What it costs

* **Idle sessions on the server.** The provider keeps one pool per database it
  connects to: the provider's `database` (used for roles, databases and other
  cluster-wide objects) plus every database named in a resource. Each pool
  keeps up to `max_idle_connections` connections, so the total adds up over
  the databases: plan for roughly
  `(number of databases + 1) × max_idle_connections` sessions while Terraform
  runs, next to your other clients. Every `provider` block (alias) runs in its
  own plugin process with its own pools, so add them up across blocks too.
  With the default `max_idle_connections = 0` the provider only holds
  connections for the resources in progress. Measured peaks of concurrent
  provider connections (`-parallelism=10`):

  | databases with resources (pools: one more) | default | `max_idle_connections = 2` | `= 5` | `= 10` |
  |---|---:|---:|---:|---:|
  | 1 | 10 | 12 | 15 | 20 |
  | 4 | 10 | 16–18 | 26–29 | 34–39 |

* **Out-of-band `DROP DATABASE`.** PostgreSQL refuses `DROP DATABASE`,
  `ALTER DATABASE ... RENAME` and `CREATE DATABASE ... TEMPLATE` while any
  other session is connected to that database. `postgresql_database` closes
  the provider's own idle connections before it drops, renames or copies a
  database, but a script or another tool doing the same while Terraform runs
  can fail with "database is being accessed by other users". Idle connections
  are closed 30 seconds after their last use and when Terraform exits. If you
  need to drop a database in the middle of a run, terminate the sessions first
  (`pg_terminate_backend`) or use `DROP DATABASE ... WITH (FORCE)` on
  PostgreSQL 13 or later.
* **Several provider blocks on one database.** `postgresql_database` only
  closes the pools of its own provider block. If another provider block has
  idle connections to the same database, a rename or a template copy of that
  database can fail. A drop still works, because the provider terminates the
  remaining sessions before `DROP DATABASE`.
* **A managed database used as a `template`.** When a `postgresql_database`
  uses another managed database as its `template`, the provider closes its
  pool to the template database before the copy. Resources inside the template
  database (schemas, extensions, grants) that are still running at that moment
  fail with `sql: database is closed`. Make the copy depend on them with
  `depends_on` (PostgreSQL refuses the copy while they are connected anyway);
  see [`postgresql_database`](../r/postgresql_database.html).

## `max_total_connections`: one limit for all databases

```hcl
provider "postgresql" {
  # ...
  max_idle_connections  = 10
  max_total_connections = 15 # >= -parallelism + number of databases
}
```

or `export PGMAXTOTALCONNECTIONS=15`.

`max_connections` and `max_idle_connections` apply to each pool, and the
provider keeps one pool per database, so with many databases the sessions add
up to about `(number of databases + 1) × max_idle_connections`.
`max_total_connections` caps the connections the provider holds open at the
same time across all its pools, so the number of sessions on the server no
longer grows with the number of databases. When all slots are taken, a new
connection replaces an idle connection of another pool; if none is idle, it
waits up to `connect_timeout` and then fails with an error that names
`max_total_connections`.

### Sizing rule

Set

```
max_total_connections >= -parallelism + number of databases
```

where the number of databases counts the provider's `database` plus every
database named in a resource. With the default `-parallelism=10` and 4
databases with resources, that is `10 + 5 = 15`.

The reason: Terraform works on up to `-parallelism` resources at a time, each
using at most one connection, but a connection belongs to one database. With
the rule, every pool can keep one idle connection while all workers hold one,
so a worker that moves to another database usually finds a free slot or an
idle connection of its own database. With a smaller budget, connections are
closed in one pool and opened in another all the time, and each of those
reconnects costs a full connection setup:

BENCH_TABLE

At or below `-parallelism` the plan still works, and peak connections stay at
the limit, but it is slower. The budget never makes the provider hang: a
resource that cannot get a connection fails after `connect_timeout`. Keep the
server's `max_connections`, minus your applications' connections, above
`max_total_connections` times the number of provider blocks that run at the
same time.

## `binary_parameters`: one round-trip per statement

```hcl
provider "postgresql" {
  # ...
  binary_parameters = true
}
```

or `export PGBINARYPARAMETERS=true`.

This adds `binary_parameters=yes` to the connection string. `lib/pq` then sends
the parse, bind and execute steps of a parameterized statement in one go and
reads all the answers at once, so each statement costs one round-trip instead
of two. The number of statements and connections stays the same.

The name is misleading: only `[]byte` parameters are sent in binary format,
and the provider passes none. Every other parameter, and every result column,
is still exchanged as text, and the server still infers the parameter types
the same way. The one encoding difference in `lib/pq` is for string
parameters bound to a `bytea` column, which the provider does not have. The
provider's acceptance tests pass with it enabled.

The gain is small while every statement still opens a new connection (the
handshake round-trips are not affected), and large once connections are
reused: at 10 ms round-trip time, about 12% on its own (12.9 s to 11.3 s),
but 29% on top of `max_idle_connections = 10` (5.2 s to 3.7 s; see the numbers
below). Enable both together.

### Connection poolers

`binary_parameters` does not add a requirement on poolers. In the default
mode, a parameterized statement is prepared and executed in two exchanges,
each ending with a Sync message; with `binary_parameters` it is a single
exchange.

Independently of this setting, the provider takes session-level advisory locks
(`pg_advisory_lock`) on a dedicated connection for some database and role
operations, which a pooler in transaction mode (for example PgBouncer with
`pool_mode = transaction`) does not keep on one server connection. Connect the
provider to PostgreSQL directly or through a pooler in session mode.

## `catalog_snapshot`: one catalog read per database

```hcl
provider "postgresql" {
  # ...
  catalog_snapshot = true
}
```

or `export PGCATALOGSNAPSHOT=true`. Off by default.

Without it, every `postgresql_grant` and `postgresql_default_privileges`
resource reads its privileges with its own catalog query: one statement (and,
without `max_idle_connections`, one new connection) per resource and per plan.
A grant on all tables of a schema also scans `pg_class` each time, which gets
slower as the database holds more relations.

With `catalog_snapshot = true`, the first read of a database takes a snapshot
of the catalogs those reads need (roles, schemas, relations, default ACLs and
the database ACL) in one statement and keeps it in memory; every other grant
and default privileges read of that database is answered from it. The first
function, procedure or routine grant read adds `pg_proc` with a second
statement. The statements for these resources go from one per resource to one
or two per database.

It covers `postgresql_default_privileges` and `postgresql_grant` with every
object type except `column`, `foreign_data_wrapper` and `foreign_server`.
Function, procedure and routine grants whose `objects` carry an argument list
(`f(integer)`) are read directly too. Other resources (roles, memberships,
schemas, databases) are not affected.

### How it stays correct

* **One snapshot per database**, kept per connection settings (like the
  connection pools) in the memory of the provider process, so each `provider`
  block has its own.
* **Every Terraform command starts without snapshots.** A plan sees each
  database as it was when its first grant or default privileges read ran.
* **Writes drop it.** Any create, update or delete of a grant, default
  privileges, schema, function or extension drops the snapshot of the database
  it wrote to, whether the write succeeded or not; roles, role memberships and
  databases drop every snapshot. The reads that follow a create or update
  always query the database.
* **It expires** after `catalog_snapshot_ttl_seconds` (default `30`,
  `PGCATALOGSNAPSHOTTTL`) and is taken again by the next read.
* **It falls back** to the direct read, with a `[WARN]` in the provider log,
  when the snapshot cannot be taken (for the next
  `catalog_snapshot_ttl_seconds`), when the catalog text of the database
  (names and ACLs) exceeds 64 MiB, or when one resource's ACL cannot be parsed
  (that resource only).

### Caveats

* **Out-of-band changes during a run.** A `GRANT` or `REVOKE` made by another
  tool while Terraform runs may not be seen until the snapshot expires or the
  next command. The same holds for writes made by another `provider` block
  (alias) to the same database: they run in another process and do not drop
  this block's snapshot.
* **Memory.** The provider holds the catalog text of each database it reads
  privileges in: a few MB for a database with about 20,000 relations, at most
  64 MiB per database.
* **Few grants, little gain.** The snapshot reads every relation of the
  database once. With only a handful of grant resources on a large database,
  the direct reads can be cheaper.

### When to enable it

Enable it when a configuration has many `postgresql_grant` or
`postgresql_default_privileges` resources per database, especially schema-wide
grants on databases with many relations, or when the server is far away. Leave
it off if other tools change privileges in the same databases while Terraform
runs and the run must see those changes.

It was not part of the benchmark in [Measured numbers](#measured-numbers). In
the measurements of the pull request that added it
([#98](https://github.com/FHL-Services-Inc/terraform-provider-postgresql/pull/98)),
on an earlier build without connection reuse, a plan of the same 1,143-resource
fixture at 10 ms round-trip time went from 12.8 s to 6.4 s and from 1,203 to
504 statements (median of 2, PostgreSQL 16).

## Recommended settings for large configurations

For a configuration with more than a few hundred resources, or a server that is
more than a couple of milliseconds away:

```hcl
provider "postgresql" {
  host     = var.host
  username = var.username
  password = var.password
  sslmode  = "require"

  max_connections      = 20   # default; per database, keep >= -parallelism
  max_idle_connections = 10   # = -parallelism; lower it for many databases
  binary_parameters    = true
}
```

Add `catalog_snapshot = true` if the configuration has many grant or default
privileges resources and nothing else changes privileges while Terraform runs
(see above).

Before you enable it, check the connection budget: the server's
`max_connections` (and any connection limit on the role, the database or a
pooler in front) must leave room for about
`(number of databases + 1) × max_idle_connections` sessions per provider block,
next to your applications. With many databases, cap that with
`max_total_connections` (at least `-parallelism` + number of databases, see
above).

`-parallelism` (default 10) is how many resources Terraform works on at the
same time. Raising it can shorten plans that wait on round-trips, but every
resource in progress needs a connection: raise `max_connections` and the
server budget with it, and measure. Concurrent `GRANT`/`REVOKE` on the same
tables can fail with `tuple concurrently updated`
([#53](https://github.com/FHL-Services-Inc/terraform-provider-postgresql/issues/53));
if you hit it, add `depends_on` between those grants or apply with a lower
`-parallelism`.

### Configuration layout

Past a certain size, Terraform itself, not the provider, is the bottleneck.
Measured with Terraform 1.17.2 and the built-in `terraform_data` resource, so
without this provider: a plan with 10,000 instances in state took about 21 s
with one `count` block and about 2 s with 10,000 separate blocks, but the apply
took 354 s with `count` and 704 s with separate blocks
([details](https://github.com/FHL-Services-Inc/terraform-provider-postgresql/issues/71#issuecomment-6876098429)).
Splitting a huge `count` or `for_each` makes plans faster, not applies.

What helps both is a smaller state per run: give each database (or group of
databases) its own root module and state, keep cluster-wide objects such as
roles and databases in a separate one, and run them in parallel. Move existing
resources with `moved` blocks or `terraform state mv`; they are not recreated.

`terraform plan -refresh=false` skips every Read and needs no round-trips, which
is useful for quick feedback while editing. It does not see changes made
outside Terraform, so do not apply such a plan. `-target` plans a subset but
hides other changes and drift; use it only as an exception.

## Measuring before and after

Measure your own configuration: the gain depends on the round-trip time to the
server, TLS, the number of databases and the resource mix.

* **Wall time.** Run `time terraform plan` a few times with each setting. The
  environment variables are handy for this, since they need no configuration
  change:

  ```shell
  time terraform plan
  time PGMAXIDLECONNECTIONS=10 PGBINARYPARAMETERS=true terraform plan
  ```

  The variables only apply to provider blocks that do not set the argument.

* **Connections opened.** On PostgreSQL 14 or later,
  `pg_stat_database.sessions` counts the sessions started per database; read
  it before and after a plan:

  ```sql
  SELECT datname, sessions FROM pg_stat_database WHERE datname IS NOT NULL;
  ```

  With `log_connections = on`, the server log has one "connection authorized"
  line per new connection.

* **Connections held.** While Terraform runs, list the provider's sessions.
  The provider connects with the application name `Terraform provider` unless
  `PGAPPNAME` is set:

  ```sql
  SELECT datname, state, count(*)
  FROM pg_stat_activity
  WHERE application_name = 'Terraform provider'
  GROUP BY datname, state;
  ```

* **Provider timings.** `TF_LOG_PROVIDER=TRACE` with `TF_LOG_PATH=plan.log`
  writes a timestamped log of every call Terraform makes to the provider,
  together with the provider's own debug messages (for example when it closes
  a pool before dropping a database). It shows where a slow plan spends its
  time.

* **Benchmark.** The repository has a reproducible plan benchmark in
  [`tests/bench`](https://github.com/FHL-Services-Inc/terraform-provider-postgresql/tree/main/tests/bench).
  It generates a fixture (1,143 resources by default), adds a fixed network
  latency through a local proxy, and reports the plan time, statements and
  connections per plan for several provider builds or settings:

  ```shell
  tests/bench/run.sh -p idle0=. -p idle10=. \
    -a 'idle10=max_idle_connections = 10' -l "0 10" -n 3
  ```

  See its README for the setup.

## Measured numbers

All numbers below come from that benchmark: 1,143 resources (220 roles, 200
role memberships, 600 grants, 100 default privileges, schemas and databases),
`-parallelism=10`, PostgreSQL 17 in a local container on an Apple-silicon
laptop, `sslmode = disable`. The "RTT" is the round-trip time added by the
benchmark's latency proxy. Real servers with TLS pay more round-trips per new
connection, so the gain from connection reuse there is larger, not smaller.
Absolute times vary with the load on the machine, so compare within a table,
not across tables.

"base" is `main` at `9ac7399`, before the performance changes of this release.
"main" is `main` at `e313310`, with the read optimizations and connection
reuse of this release. It predates later merges, among them `catalog_snapshot`
([#98](https://github.com/FHL-Services-Inc/terraform-provider-postgresql/pull/98)),
which was not benchmarked here (see the `catalog_snapshot` section above for
the numbers measured in that pull request).

**`terraform plan`** with refresh, 10 ms RTT, median of 3 runs:

| build | plan | statements per plan | connections per plan |
|---|---:|---:|---:|
| base | 35.0 s | 7,005 | 2,587 |
| main, defaults | 12.9 s | 1,163 | 1,183 |
| main, `max_idle_connections = 10` | 5.2 s | 1,163 | **38** |
| main, `binary_parameters = true` | 11.3 s | 1,163 | 1,183 |
| main, both | **3.7 s** | 1,163 | **38** |

Without added latency (0 ms RTT, one run) the same plans took 6.0 s, 4.0 s,
2.2 s, 3.5 s and 2.2 s.

**`terraform apply`** of the whole fixture into an empty server, 10 ms RTT,
`-parallelism=1`, one run:

| build | apply |
|---|---:|
| base | 417 s |
| main, defaults | 367 s |
| main, `max_idle_connections = 10` | 226 s |
| main, `binary_parameters = true` | 316 s |
| main, both | **174 s** |

**Pool size trade-off** at 10 ms RTT, single database, on an earlier branch
(PostgreSQL 16, run in parallel with another benchmark, so the times are
higher than they would be on an idle machine):

| setting | plan | connections per plan | peak connections |
|---|---:|---:|---:|
| v1.28.0 | 30.3 s | 2,587 | 10 |
| `max_idle_connections = 2` | 19.7 s | 504 | 12 |
| `max_idle_connections = 5` | 18.2 s | 74 | 15 |
| `max_idle_connections = 10` | 17.8 s | 38 | 20 |
| `max_idle_connections = 10`, `max_connections = 5` | 28.3 s | 27 | 10 |

With 4 databases (5 pools), `max_idle_connections = 10` reached a peak of
34–39 connections and `= 2` a peak of 16–18, at 16.8 s and 20.5 s for the
10 ms plan (v1.28.0: 29.1 s, peak 10).
