#!/usr/bin/env bash
#
# Run the acceptance tests against a throwaway PostgreSQL container.
#
# Works with Apple's `container` CLI (default, macOS) and with Docker
# (CONTAINER_CLI=docker). Unlike `make testacc_setup` it uses the stock
# `postgres` image, so no image build is needed. The only thing missing from
# the stock image is the `dummy_seclabel` module, so the security label tests
# are skipped (set PGIMAGE to an image built from tests/build to run them).
#
# The port is always published on 127.0.0.1: on macOS, Local Network privacy
# can block freshly built test binaries from reaching the container's own IP.
#
# Usage:
#   tests/container_acc.sh start [superuser|rds]   start PG, print the env to eval
#   tests/container_acc.sh env [superuser|rds]     print the env for a profile
#   tests/container_acc.sh clean                   drop leftover test databases/roles
#   tests/container_acc.sh restart [superuser|rds] stop, then start
#   tests/container_acc.sh stop                    stop and remove the container
#   tests/container_acc.sh run [go test args...]   run the suite once per profile
#                                                  in a fresh container each time
#
#   eval "$(tests/container_acc.sh start rds)"
#   go test -count=1 ./postgresql -run '^TestAccPostgresqlGrant'
#   tests/container_acc.sh stop
#
# Settings (environment):
#   CONTAINER_CLI           container (default) or docker
#   PGVERSION               PostgreSQL major version (default 16)
#   PGIMAGE                 image to run (default postgres:$PGVERSION)
#   CONTAINER_ACC_NAME      container name (default pg-tf-acc)
#   CONTAINER_ACC_PORT      host port on 127.0.0.1 (default 25432)
#   CONTAINER_ACC_PROFILES  profiles used by `run` (default "superuser rds")
#   CONTAINER_ACC_RUN_ARGS  extra arguments for `<cli> run`, e.g. "--memory 2g"

set -euo pipefail

CLI=${CONTAINER_CLI:-container}
PGVERSION=${PGVERSION:-16}
IMAGE=${PGIMAGE:-postgres:${PGVERSION}}
NAME=${CONTAINER_ACC_NAME:-pg-tf-acc}
PORT=${CONTAINER_ACC_PORT:-25432}
PROFILES=${CONTAINER_ACC_PROFILES:-superuser rds}
SUPERUSER_PASSWORD=postgres

log() {
    echo "container_acc: $*" >&2
}

die() {
    log "$*"
    exit 1
}

exists() {
    "$CLI" inspect "$NAME" >/dev/null 2>&1
}

# psql inside the container as the bootstrap superuser, so nothing has to be
# installed on the host.
psql_exec() {
    local db=$1
    shift
    "$CLI" exec -i "$NAME" psql -X -q -v ON_ERROR_STOP=1 -U postgres -d "$db" "$@"
}

check_profile() {
    case "$1" in
        superuser | rds) ;;
        *) die "unknown profile '$1' (expected superuser or rds)" ;;
    esac
}

print_env() {
    local profile=$1 user password superuser
    check_profile "$profile"
    if [ "$profile" = rds ]; then
        user=rds password=rds superuser=false
    else
        user=postgres password=$SUPERUSER_PASSWORD superuser=true
    fi
    cat <<EOF
export TF_ACC=true
export PGHOST=127.0.0.1
export PGPORT=$PORT
export PGUSER=$user
export PGPASSWORD=$password
export PGSSLMODE=disable
export PGSUPERUSER=$superuser
EOF
}

wait_ready() {
    local i
    # Inside the container first: the image's init step runs a temporary
    # server that only listens on the Unix socket, so a TCP check on
    # 127.0.0.1 only passes once the real server is up.
    for i in $(seq 1 60); do
        if "$CLI" exec "$NAME" pg_isready -q -h 127.0.0.1 -p 5432 -U postgres 2>/dev/null; then
            break
        fi
        [ "$i" -eq 60 ] && die "PostgreSQL did not become ready, see: $CLI logs $NAME"
        sleep 1
    done
    # Then through the published port, from the host.
    for i in $(seq 1 30); do
        if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    die "127.0.0.1:$PORT is not reachable from the host"
}

# Same setup as tests/switch_rds.sh: a non-superuser role with CREATEDB and
# CREATEROLE that owns the postgres database and the public schemas, like the
# master user on RDS.
setup_rds() {
    log "setting up the rds profile"
    psql_exec postgres <<'EOS'
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'rds') THEN
        CREATE ROLE rds LOGIN CREATEDB CREATEROLE PASSWORD 'rds';
    END IF;
END
$$;
ALTER DATABASE postgres OWNER TO rds;
ALTER SCHEMA public OWNER TO rds;
EOS
    psql_exec template1 <<'EOS'
ALTER SCHEMA public OWNER TO rds;
EOS
}

cmd_start() {
    local profile=${1:-superuser}
    check_profile "$profile"
    exists && die "container $NAME already exists (use restart, or stop it first)"

    log "starting $IMAGE as $NAME on 127.0.0.1:$PORT"
    # shellcheck disable=SC2086 # CONTAINER_ACC_RUN_ARGS is meant to be split
    "$CLI" run -d --rm --name "$NAME" \
        -e POSTGRES_PASSWORD="$SUPERUSER_PASSWORD" \
        -p "127.0.0.1:$PORT:5432" \
        ${CONTAINER_ACC_RUN_ARGS:-} \
        "$IMAGE" \
        postgres -c wal_level=logical -c max_replication_slots=10 >/dev/null
    wait_ready

    if [ "$profile" = rds ]; then
        setup_rds
    fi
    log "ready ($profile profile)"
    print_env "$profile"
}

cmd_stop() {
    local i
    if ! exists; then
        log "container $NAME is not running"
        return 0
    fi
    log "stopping $NAME"
    "$CLI" stop "$NAME" >/dev/null 2>&1 || true
    # Started with --rm, but the removal is asynchronous.
    for i in $(seq 1 15); do
        exists || return 0
        sleep 1
    done
    "$CLI" rm "$NAME" >/dev/null 2>&1 || true
}

# Drop what earlier runs left behind: every database other than postgres and
# the templates, every role other than postgres, rds and the predefined pg_*
# roles, and inactive replication slots. A test whose teardown was skipped
# (e.g. after a dropped connection) otherwise makes later tests fail with
# "already exists". Objects created by the superuser in the postgres database
# itself are not touched; use restart for a pristine server.
cmd_clean() {
    exists || die "container $NAME is not running"
    log "dropping leftover databases, roles and replication slots"

    local version force=""
    version=$(psql_exec postgres -At -c "SELECT current_setting('server_version_num')::int")
    [ "$version" -ge 130000 ] && force=" WITH (FORCE)"

    psql_exec postgres <<EOS
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname NOT IN ('postgres', 'template0', 'template1') AND pid <> pg_backend_pid() \\g /dev/null
SELECT format('DROP DATABASE IF EXISTS %I$force', datname) FROM pg_database
 WHERE datname NOT IN ('postgres', 'template0', 'template1') \\gexec
SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE NOT active \\g /dev/null
EOS

    local db
    for db in postgres template1; do
        psql_exec "$db" <<'EOS'
SELECT format('DROP OWNED BY %s CASCADE', string_agg(quote_ident(rolname), ', '))
  FROM pg_roles
 WHERE rolname NOT LIKE 'pg\_%' AND rolname NOT IN ('postgres', 'rds')
HAVING count(*) > 0 \gexec
EOS
    done
    psql_exec postgres <<'EOS'
SELECT format('DROP ROLE %I', rolname) FROM pg_roles
 WHERE rolname NOT LIKE 'pg\_%' AND rolname NOT IN ('postgres', 'rds') \gexec
EOS
}

cmd_run() {
    local profile env rc=0
    cd "$(dirname "$0")/.."
    trap cmd_stop EXIT
    for profile in $PROFILES; do
        check_profile "$profile"
        cmd_stop
        env=$(cmd_start "$profile") || exit 1
        eval "$env"
        log "running acceptance tests ($profile profile)"
        if ! go test -count=1 ./postgresql -v -timeout 120m "$@"; then
            log "acceptance tests failed ($profile profile)"
            rc=1
        fi
    done
    return $rc
}

usage() {
    sed -n '/^# Usage:/,/^# Settings/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2
    exit 2
}

case "${1:-}" in
    start) shift; cmd_start "$@" ;;
    stop) cmd_stop ;;
    restart) shift; cmd_stop; cmd_start "$@" ;;
    env) shift; print_env "${1:-superuser}" ;;
    clean) cmd_clean ;;
    run) shift; cmd_run "$@" ;;
    *) usage ;;
esac
