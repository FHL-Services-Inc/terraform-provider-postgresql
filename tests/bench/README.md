# Plan benchmark

A reproducible benchmark for large `terraform plan` runs against this provider. It
times `terraform plan -refresh=true`, the hot path: Terraform calls `Read` for every
resource in state on every plan and apply. It also counts the SQL statements and
PostgreSQL connections each plan uses, and samples how many connections the provider
holds at the same time.

Use it to compare provider builds (before/after a change). It can also add network
latency, because most of the cost of a large plan is round trips. A local PostgreSQL
has close to zero latency and hides that cost.

## What's in here

| file | purpose |
|---|---|
| `run.sh` | Runs everything: builds the providers, loads the fixture, applies once, runs the timed plans, writes the summary |
| `genfixture/` | Generates `fixture.sql` (database, schemas, tables), `main.tf` (Terraform resources) and `cleanup.sql` |
| `latencyproxy/` | TCP proxy that delays each direction by a fixed amount (RTT = 2 x delay) |
| `connsampler/` | Polls `pg_stat_activity` during each plan for the concurrent-connections columns |
| `smallpool.sh` | Small-pool matrix: databases with an owner under `max_connections` 1/2/4/8 x `-parallelism` 1/2/10, with a timeout per phase so a deadlock shows up as a hang |

### Generated workload (defaults)

Tables are not provider resources, so `fixture.sql` creates them directly. The default
is 1 database, 10 schemas and 1500 tables. Terraform then manages 1143 resources:

| resource | count | notes |
|---|---:|---|
| `postgresql_role` | 220 | 200 login users + 20 groups |
| `postgresql_grant_role` | 200 | each user is a member of one group |
| `postgresql_grant` (schema) | 200 | `USAGE` on the user's schema |
| `postgresql_grant` (table, no `objects`) | 200 | schema-wide `SELECT` (150 tables) |
| `postgresql_grant` (table, `objects`) | 200 | `INSERT, SELECT` on 10 tables of another schema |
| `postgresql_default_privileges` | 100 | every second user |
| `postgresql_schema` | 20 | empty schemas owned by the groups |
| `postgresql_database` | 3 | |

You can change the sizes through `GEN_ARGS`. For example, use
`GEN_ARGS="-roles 400 -tables 2000"` to get about 2300 resources. Run
`go run ./tests/bench/genfixture -h` to see every flag.

`GEN_ARGS="-fixture-dbs 4"` spreads the 10 schemas round-robin over 4 databases
(`bench`, `bench_1`, `bench_2`, `bench_3`), together with the grants, default
privileges and Terraform-managed schemas on them. The resource count stays the same.
The provider keeps one connection pool per database, so this is the setup where idle
connections of several pools add up.

## Running it

You need Go, Terraform 1.x, `psql` and `perl`. `perl` is used for sub-second timestamps.

1. Start a disposable PostgreSQL. `pg_stat_statements` enables the query count;
   without it, the queries column says `n/a`. With Apple's `container` CLI:

   ```sh
   container run -d --name pg-tf-bench --rm -e POSTGRES_PASSWORD=postgres \
     -p 127.0.0.1:25445:5432 postgres:16 \
     postgres -c max_connections=300 -c shared_preload_libraries=pg_stat_statements
   ```

   Check that it accepts connections with `container ls` and
   `psql -h 127.0.0.1 -p 25445 -U postgres -c 'select 1'`. Connection counts come from
   `pg_stat_database.sessions`, which needs PostgreSQL 14 or later. Since macOS 27,
   connect through the published `127.0.0.1` port, not the container's own IP: Local
   Network privacy blocks freshly built binaries from the container subnet.

2. Run the matrix. Each `-p label=path` takes either a provider source checkout, which
   is built for you, or a directory that already contains a `terraform-provider-postgresql`
   binary. To compare a branch against `main`, run this from the repository root:

   ```sh
   git worktree add --detach /tmp/provider-main fhl/main

   tests/bench/run.sh -p main=/tmp/provider-main -p branch=. -l "0 2 10" -n 5
   ```

   To compare provider settings, use `-a label='argument = value'`. It adds provider
   arguments for one label. You can measure the same build under two labels, for
   example with and without connection reuse:

   ```sh
   tests/bench/run.sh -p idle0=. -p idle10=. -a 'idle10=max_idle_connections = 10'
   ```

   `-l` lists the added round-trip times in ms. `0` connects directly; any other value
   goes through `latencyproxy`. `-n` sets the timed plans per provider and latency.
   Within each latency, runs are interleaved across providers, so slow periods on the
   machine affect all of them. Run `run.sh -h` for the other options: `-P` sets the
   parallelism, `-w` sets the work dir, and `-S` reuses an existing fixture and state.

3. `run.sh` prints a markdown table and writes it to `<workdir>/summary.md`.
   `<workdir>/results.csv` holds the per-run numbers, and `<workdir>/logs/` holds the
   apply and plan output.

`run.sh` loads Terraform from `dev_overrides` in a CLI config that it writes into the
work dir. Your `~/.terraformrc` is not touched, and `terraform init` is not needed.

The setup `apply` runs once, with the first `-p` provider and `-parallelism=1`. With
higher parallelism, concurrent GRANT/REVOKE on the same tables can fail with
`tuple concurrently updated`. The apply takes about 70 s at the default size. All
providers then plan against the same state. The fixture uses fixed database and role
names, so one PostgreSQL instance holds one fixture at a time. A setup run in another
work dir replaces it. Every plan must exit with code 0, which
means no changes. Exit code 2 means drift and is logged, so a provider that reads
something differently stands out.

`run.sh` pauses for `COOLDOWN` seconds before a plan. The default is 2*MSL + 5 s,
which is 35 s on macOS and 65 s elsewhere. Today a plan opens one connection per few
statements, which is about 2,800 at the default size. Each connection leaves its
ephemeral ports in TIME_WAIT. On macOS, two or three plans in a row exhaust the port
range, and the plan then fails with `connect: can't assign requested address`. The
`container` port forwarder and the latency proxy use extra ports, so this happens
sooner. The pause only happens when the previous plan (or the setup apply) opened more
than `COOLDOWN_MIN_CONNS` connections (default 500). A plan with
`max_idle_connections > 0` opens a few dozen, so the plan after it starts right away;
`COOLDOWN_MIN_CONNS=0` pauses before every plan. As a rough guide, a plan that reconnects
for every statement takes about 50 s including the cooldown. For example, 4 providers x 3
latencies x 3 runs, all without connection reuse, took 32 minutes on an M-series Mac.
Lowering `net.inet.tcp.msl` (macOS) shortens the default pause.

Several matrices can run at the same time against separate PostgreSQL containers: give
each its own `PGPORT`, `PROXY_PORT` and `-w`. They do compete for CPU, so compare
numbers within one matrix, not across matrices that ran in parallel.

To clean up, run `psql -f <workdir>/tf/cleanup.sql` and `container stop pg-tf-bench`
(the container was started with `--rm`, so stopping it also removes it).

## Small-pool matrix

`smallpool.sh` checks that the provider never deadlocks on a small pool. For every
label, `max_connections` value (`-c`, default `1 2 4 8`) and `-parallelism` value (`-P`,
default `1 2 10`) it starts from an empty cluster and runs three phases on 4 databases
(`-d`), each with an owner role and a schema inside:

1. `create`: `terraform apply`
2. `reown`: `terraform apply` with every database and schema moved to the other owner
3. `destroy`: `terraform destroy`

Each phase runs under `timeout` (`-t`, default 120 s). A phase that runs out of time is a
**hang**, a non-zero exit is a **fail**, and the rest of that cell is skipped. Between
cells the script kills leftover provider processes, terminates their backends (a hung
provider may still hold advisory locks) and drops the databases and roles. It needs GNU
`timeout` (`brew install coreutils` on macOS).

```sh
tests/bench/smallpool.sh -p main=/tmp/provider-main -p branch=.
tests/bench/smallpool.sh -p pooled=. -a 'pooled=max_idle_connections = 2' -c "1 2" -P "1 10"
```

The matrix takes a few minutes when nothing hangs; each hang adds the timeout. The table
goes to `<workdir>/summary.md` (default work dir `${TMPDIR:-/tmp}/pg-tf-smallpool`), the
per-phase rows to `results.csv`, and the Terraform output to `logs/`. It uses the same
PostgreSQL as `run.sh` and its own names (`smallpool_db_*`, `smallpool_owner_*`), so it
does not touch the plan fixture.

## Reading the numbers

- **plan median**: wall time of the whole `terraform plan` command, including
  Terraform's own startup and graph walk. That overhead is a few seconds.
- **queries**: the sum of `calls` in `pg_stat_statements` during the plan. Protocol
  round trips that are not statements, such as connection startup and authentication,
  are not included.
- **connections**: backends started during the plan. With `SetMaxIdleConns(0)`, almost
  every statement outside a transaction opens a new connection.
- **peak conns (max)**, **p95 conns**: connections the provider holds at the same time.
  `connsampler` counts client backends with `application_name = 'Terraform provider'`
  (the provider sets it as `fallback_application_name`) every 100 ms
  (`SAMPLE_INTERVAL`) during the plan. Its own connection is excluded, and it connects
  directly, not through the latency proxy. Peak is the largest sample of a run, and the
  table shows the median over runs with the highest run in parentheses. p95 is over all
  samples of a run, including the few seconds of Terraform startup with no connections.
  Sampling misses connections that live shorter than the interval, so with
  `max_idle_connections = 0` the peak is a lower bound.
- **peak idle**: the largest number of those connections in state `idle` in one
  sample: parked in the pool, or waiting for the client's next statement (at higher
  RTT a busy connection spends most of its time like that).

The raw samples are in `logs/conns-<label>-<rtt>-<run>.tsv`: time, connections, idle
connections, and connections per database, for example `bench:10,postgres:4`. With
`-fixture-dbs` this shows how much each pool holds.

The proxy also delays the first client packet of each connection by one RTT, to stand
in for the TCP handshake. It does not emulate TLS. Against a real server with
`sslmode=require`, each new connection costs further round trips, so the latency rows
understate the cost of reconnecting.
