Terraform Provider for PostgreSQL
=================================

This provider allows to manage with Terraform [Postgresql](https://www.postgresql.org/) objects like databases, extensions, roles, etc.

It's published on the [Terraform registry](https://registry.terraform.io/providers/cyrilgdn/postgresql/latest/docs).
It replaces https://github.com/hashicorp/terraform-provider-postgresql since Hashicorp stopped hosting community providers in favor of the Terraform registry.

- Documentation: https://registry.terraform.io/providers/cyrilgdn/postgresql/latest/docs

Requirements
------------

-	[Terraform](https://www.terraform.io/downloads.html) 0.12.x
-	[Go](https://golang.org/doc/install) 1.16 (to build the provider plugin)

Building The Provider
---------------------

Clone repository to: `$GOPATH/src/github.com/cyrilgdn/terraform-provider-postgresql`

```sh
$ mkdir -p $GOPATH/src/github.com/cyrilgdn; cd $GOPATH/src/github.com/cyrilgdn
$ git clone git@github.com:cyrilgdn/terraform-provider-postgresql
```

Enter the provider directory and build the provider

```sh
$ cd $GOPATH/src/github.com/cyrilgdn/terraform-provider-postgresql
$ make build
```

Using the provider
----------------------

Usage examples can be found in the Terraform [provider documentation](https://www.terraform.io/docs/providers/postgresql/index.html)

Developing the Provider
---------------------------

If you wish to work on the provider, you'll first need [Go](http://www.golang.org) installed on your machine (version 1.11+ is *required*). You'll also need to correctly setup a [GOPATH](http://golang.org/doc/code.html#GOPATH), as well as adding `$GOPATH/bin` to your `$PATH`.

To compile the provider, run `make build`. This will build the provider and put the provider binary in the `$GOPATH/bin` directory.

```sh
$ make build
...
$ $GOPATH/bin/terraform-provider-postgresql
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
- Prefer running a targeted subset locally and leave the full matrix (PostgreSQL 12 to 18, both profiles) to CI. By default the provider doesn't keep idle connections, so every query opens a new TCP connection, and each closed one stays in `TIME_WAIT` for a while. With two or three suites running at once the host can run out of ephemeral ports (`connect: can't assign requested address`). A test whose teardown fails because of that leaves objects behind, and later tests then fail with "already exists"; `tests/container_acc.sh clean` (or `restart`) gets you back to a clean server.
- If you do need to run several suites in parallel on macOS, you can widen the ephemeral port range and shorten `TIME_WAIT` (2 × MSL, 30 s by default):

  ```sh
  sudo sysctl -w net.inet.ip.portrange.first=32768   # default 49152
  sudo sysctl -w net.inet.tcp.msl=5000               # default 15000 (ms)
  ```

  Both apply to the whole machine until the next reboot. A wider range can hand out ports that a local service expects to bind, and a shorter MSL makes it more likely that a delayed packet from an old connection is taken for part of a new one on the same address and port. Set them back to the defaults when you're done.
