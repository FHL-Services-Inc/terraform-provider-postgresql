---
layout: "postgresql"
page_title: "PostgreSQL: Performance tuning"
sidebar_current: "docs-postgresql-guide-performance-tuning"
description: |-
  Provider settings that make plan and apply faster with many resources: connection reuse, single round-trip statements, and how to size and measure them.
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
  7,100 statements and opened about 2,600 connections.
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
benchmark below this took a plan from about 2,600 new connections to 38, and
the plan time at 10 ms round-trip time from 30.2 s to 17.9 s.

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

  | databases (plus the provider's `database`) | default | `max_idle_connections = 2` | `= 5` | `= 10` |
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
reused: at 10 ms round-trip time, about 11% on its own, but 36% on top of
`max_idle_connections = 10` (see the numbers below). Enable both together.

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

Before you enable it, check the connection budget: the server's
`max_connections` (and any connection limit on the role, the database or a
pooler in front) must leave room for about
`(number of databases + 1) × max_idle_connections` sessions per provider block,
next to your applications.

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
`terraform plan` with refresh, `-parallelism=10`, PostgreSQL 16 in a local
container on an Apple-silicon laptop, `sslmode = disable`, median of 3 runs. The
"RTT" is the round-trip time added by the benchmark's latency proxy. Real
servers with TLS pay more round-trips per new connection, so the gain from
connection reuse there is larger, not smaller. Absolute times vary with the
load on the machine, so compare within a table, not across tables.

**`max_idle_connections`**, same build, before the other read
optimizations of this release:

| RTT | `max_idle_connections = 0` | `= 10` |
|---:|---:|---:|
| 0 ms | 10.6 s | 8.7 s |
| 2 ms | 13.3 s | 8.7 s |
| 10 ms | 30.2 s | **17.9 s** |
| statements per plan | 7,103 | 7,103 |
| connections per plan | 2,583 | **38** |

**`binary_parameters`** on its own, same build, `max_idle_connections = 0`
(this run was on a busy machine, hence the higher absolute times):

| RTT | default | `binary_parameters = true` |
|---:|---:|---:|
| 0 ms | 25.2 s | 25.0 s |
| 2 ms | 25.3 s | 24.2 s |
| 10 ms | 36.4 s | **32.4 s** |

**Both together**, on a branch with the read optimizations of this release
and a few changes still in review, single database:

| RTT | v1.28.0 | branch, defaults | `max_idle_connections = 10` | `+ binary_parameters` |
|---:|---:|---:|---:|---:|
| 0 ms | 8.9 s | 4.7 s | 2.4 s | **2.3 s** |
| 10 ms | 30.9 s | 31.5 s | 18.2 s | **11.6 s** |
| connections per plan | 2,587 | 2,583 | 38 | 38 |
| peak connections | 10 | 10 | 20 | 20 |

**Pool size trade-off** at 10 ms RTT, same branch, single database (run
in parallel with another benchmark, so the times are higher than above):

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
