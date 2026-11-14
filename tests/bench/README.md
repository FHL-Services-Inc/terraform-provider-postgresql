# Plan benchmark

A reproducible benchmark for large `terraform plan` runs against this provider. It
times `terraform plan -refresh=true`, the hot path: Terraform calls `Read` for every
resource in state on every plan and apply. It also counts the SQL statements and
PostgreSQL connections each plan uses.

Use it to compare provider builds (before/after a change). It can also add network
latency, because most of the cost of a large plan is round trips. A local PostgreSQL
has close to zero latency and hides that cost.

## What's in here

| file | purpose |
|---|---|
| `run.sh` | Runs everything: builds the providers, loads the fixture, applies once, runs the timed plans, writes the summary |
| `genfixture/` | Generates `fixture.sql` (database, schemas, tables), `main.tf` (Terraform resources) and `cleanup.sql` |
| `latencyproxy/` | TCP proxy that delays each direction by a fixed amount (RTT = 2 x delay) |

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

## Running it

You need Go, Terraform 1.x, `psql` and `perl`. `perl` is used for sub-second timestamps.

1. Start a disposable PostgreSQL. `pg_stat_statements` enables the query count;
   without it, the queries column says `n/a`. With Apple's `container` CLI:

   ```sh
   container run -d --name pg-tf-bench --rm -e POSTGRES_PASSWORD=postgres \
     -p 127.0.0.1:25445:5432 postgres:16 \
     postgres -c max_connections=300 -c shared_preload_libraries=pg_stat_statements
   ```

   The same arguments work with `docker run` if you use Docker. Connection counts come
   from `pg_stat_database.sessions`, which needs PostgreSQL 14 or later.

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

`run.sh` pauses for `COOLDOWN` seconds before each plan. The default is 2*MSL + 5 s,
which is 35 s on macOS and 65 s elsewhere. Today a plan opens one connection per few
statements, which is about 2,800 at the default size. Each connection leaves its
ephemeral ports in TIME_WAIT. On macOS, two or three plans in a row exhaust the port
range, and the plan then fails with `connect: can't assign requested address`. The
`container` port forwarder and the latency proxy use extra ports, so this happens
sooner. As a rough guide, each plan takes about 50 s including the cooldown. For example,
4 providers x 3 latencies x 3 runs took 32 minutes on an M-series Mac.

To clean up, run `psql -f <workdir>/tf/cleanup.sql` and `container stop pg-tf-bench`.

## Reading the numbers

- **plan median**: wall time of the whole `terraform plan` command, including
  Terraform's own startup and graph walk. That overhead is a few seconds.
- **queries**: the sum of `calls` in `pg_stat_statements` during the plan. Protocol
  round trips that are not statements, such as connection startup and authentication,
  are not included.
- **connections**: backends started during the plan. With `SetMaxIdleConns(0)`, almost
  every statement outside a transaction opens a new connection.

The proxy also delays the first client packet of each connection by one RTT, to stand
in for the TCP handshake. It does not emulate TLS. Against a real server with
`sslmode=require`, each new connection costs further round trips, so the latency rows
understate the cost of reconnecting.
