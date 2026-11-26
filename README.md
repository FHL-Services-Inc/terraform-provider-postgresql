Terraform Provider for PostgreSQL
=================================

This provider allows to manage with Terraform [Postgresql](https://www.postgresql.org/) objects like databases, extensions, roles, etc.

It's published on the [Terraform registry](https://registry.terraform.io/providers/FHL-Services-Inc/postgresql/latest)
as `FHL-Services-Inc/postgresql`.

This is a maintained fork of [cyrilgdn/terraform-provider-postgresql](https://github.com/cyrilgdn/terraform-provider-postgresql),
which no longer gets regular releases. It continues the upstream version line:
upstream's last release is v1.27.0, this fork starts at v1.28.0 and follows
semantic versioning independently of upstream. Issues and pull requests are welcome here.

- Documentation: https://registry.terraform.io/providers/FHL-Services-Inc/postgresql/latest

Migrating from cyrilgdn/postgresql
----------------------------------

1. Change the source address in the root module and in every child module:

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

2. Point the existing state at the new address and reinstall:

   ```sh
   terraform state replace-provider cyrilgdn/postgresql FHL-Services-Inc/postgresql
   terraform init -upgrade
   terraform providers   # cyrilgdn/postgresql must no longer be listed
   ```

   If a module still requires `cyrilgdn/postgresql`, Terraform installs both providers
   and the next apply moves that module's resources back to the old address.

3. Run `terraform plan` and read the [changelog](CHANGELOG.md) entries from 1.28.0 on,
   including the 1.29.0 upgrade notes, before applying.

Requirements
------------

-	[Terraform](https://developer.hashicorp.com/terraform/install) 0.13 or later (provider source addresses)
-	[Go](https://go.dev/doc/install) as pinned in [`.go-version`](.go-version) (to build the provider plugin)

Building The Provider
---------------------

Clone the repository and build the provider:

```sh
$ git clone git@github.com:FHL-Services-Inc/terraform-provider-postgresql
$ cd terraform-provider-postgresql
$ make build
```

Using the provider
----------------------

Usage examples can be found in the Terraform [provider documentation](https://registry.terraform.io/providers/FHL-Services-Inc/postgresql/latest/docs)

Developing the Provider
---------------------------

If you wish to work on the provider, you'll first need [Go](https://go.dev) installed on your machine (the version in [`.go-version`](.go-version)).

To compile the provider, run `make build`. This runs `go install`, which puts the provider binary in `$(go env GOPATH)/bin` (or `$GOBIN` if set).

```sh
$ make build
...
$ $(go env GOPATH)/bin/terraform-provider-postgresql
...
```

In order to test the provider, you can simply run `make test`.

```sh
$ make test
```

Before opening a pull request, also run `make vet` and `make lint`. `make lint` runs the same pinned golangci-lint version as CI, so lint failures show up locally instead of only on the PR.

```sh
$ make vet
$ make lint
```

In order to run the full suite of Acceptance tests, run `make testacc`.

*Note:* 
- Acceptance tests create real resources, and often cost money to run.

```sh
$ make testacc
```

In order to manually run some Acceptance test locally, run the following commands:
```sh
# spins up a local docker postgres container
make testacc_setup 

# Load the needed environment variables for the tests
source tests/switch_superuser.sh

# Run the test(s) that you're working on as often as you want
TF_LOG=INFO go test -v ./postgresql -run ^TestAccPostgresqlRole_Basic$

# cleans the env and tears down the postgres container
make testacc_cleanup 
```

### Acceptance tests with Apple `container` (or without building an image)

`make testacc_setup` needs Docker and builds a custom image (`tests/build`). `tests/container_acc.sh` runs the stock `postgres` image instead, with Apple's [`container`](https://github.com/apple/container) CLI by default or with Docker (`CONTAINER_CLI=docker`):

```sh
# start PostgreSQL 16 on 127.0.0.1:25432 and load the env for the superuser profile
eval "$(tests/container_acc.sh start)"

# run only the tests you're working on
go test -count=1 -v ./postgresql -run '^TestAccPostgresqlGrant'

# drop the databases/roles that failed tests left behind
tests/container_acc.sh clean

# the non-superuser (RDS-like) profile, same setup as tests/switch_rds.sh
eval "$(tests/container_acc.sh restart rds)"
go test -count=1 -v ./postgresql -run '^TestAccPostgresqlGrant'

tests/container_acc.sh stop
```

`make testacc_container` runs the suite in both profiles, each in a fresh container, like `make testacc`. Narrow it down with `TESTARGS`, e.g. `make testacc_container TESTARGS="-run '^TestAccPostgresqlRole'"`. `PGVERSION` selects the server version (default 16), `CONTAINER_ACC_PORT` and `CONTAINER_ACC_NAME` the port and container name; see the header of the script for the rest.

Notes:

- The stock image doesn't have the `dummy_seclabel` module, so `TestAccPostgresqlSecurityLabel_*` are skipped. Set `PGIMAGE` to an image built from `tests/build` to run them.
- Connect through `127.0.0.1`, not the container's IP. On macOS, Local Network privacy can block freshly built binaries such as `go test` executables from the container subnet (`dial tcp 192.168.x.x:5432: connect: no route to host`, while `nc` to the same address works). The script always publishes the port on `127.0.0.1`.
- Prefer running a targeted subset locally and leave the full matrix (PostgreSQL 12 to 18, both profiles) to CI. With `PGMAXIDLECONNECTIONS=0` (no idle connections, as in the "old defaults" CI shards) every query opens a new TCP connection, and each closed one stays in `TIME_WAIT` for a while; with the default `max_idle_connections = 5` the provider reuses its connections, but the test helpers and the many short-lived provider pools still open plenty. With two or three suites running at once the host can run out of ephemeral ports (`connect: can't assign requested address`). A test whose teardown fails because of that leaves objects behind, and later tests then fail with "already exists"; `tests/container_acc.sh clean` (or `restart`) gets you back to a clean server.
- If you do need to run several suites in parallel on macOS, you can widen the ephemeral port range and shorten `TIME_WAIT` (2 × MSL, 30 s by default):

  ```sh
  sudo sysctl -w net.inet.ip.portrange.first=32768   # default 49152
  sudo sysctl -w net.inet.tcp.msl=5000               # default 15000 (ms)
  ```

  Both apply to the whole machine until the next reboot. A wider range can hand out ports that a local service expects to bind, and a shorter MSL makes it more likely that a delayed packet from an old connection is taken for part of a new one on the same address and port. Set them back to the defaults when you're done.
