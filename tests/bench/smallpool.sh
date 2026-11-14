#!/usr/bin/env bash
#
# Small-pool matrix: create, re-own and destroy databases with an owner (plus a
# schema inside each) for every combination of the provider's max_connections and
# terraform -parallelism. Each phase runs under a timeout, and a phase that does
# not finish in time counts as a hang. With a pool of one or two connections, any
# code path that holds a connection while it waits for another one deadlocks.
#
# See tests/bench/README.md. Short version:
#
#   tests/bench/smallpool.sh -p base=/path/to/old/checkout -p new=.
#
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: smallpool.sh -p label=path [-p label=path ...] [options]

  -p label=path  provider to test (repeatable), as in run.sh: a provider source checkout
                 (built into the work dir) or a directory with a terraform-provider-postgresql
                 binary.
  -a label='arg = value'
                 extra provider argument for one label (repeatable), e.g.
                 -a 'pooled=max_idle_connections = 2'
  -c "list"      provider max_connections values (default "1 2 4 8")
  -P "list"      terraform -parallelism values (default "1 2 10")
  -d n           databases per cell, each with an owner and one schema (default 4)
  -t seconds     timeout per phase (create, reown, destroy); exceeding it is a hang
                 (default 120)
  -w dir         work dir (default ${TMPDIR:-/tmp}/pg-tf-smallpool)
  -h             help

Environment: PGHOST/PGPORT/PGUSER/PGPASSWORD as for run.sh (default 127.0.0.1:25445,
postgres/postgres), TERRAFORM.
EOF
}

die() { echo "error: $*" >&2; exit 1; }
log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }

providers=()
extra_args=()
conns="1 2 4 8"
pars="1 2 10"
ndb=4
phase_timeout=120
workdir="${TMPDIR:-/tmp}"
workdir="${workdir%/}/pg-tf-smallpool"

while getopts "p:a:c:P:d:t:w:h" opt; do
	case $opt in
	p) providers+=("$OPTARG") ;;
	a) extra_args+=("$OPTARG") ;;
	c) conns=$OPTARG ;;
	P) pars=$OPTARG ;;
	d) ndb=$OPTARG ;;
	t) phase_timeout=$OPTARG ;;
	w) workdir=$OPTARG ;;
	h) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
[ ${#providers[@]} -gt 0 ] || { usage >&2; exit 2; }

export PGHOST=${PGHOST:-127.0.0.1} PGPORT=${PGPORT:-25445} PGUSER=${PGUSER:-postgres} PGPASSWORD=${PGPASSWORD:-postgres}
export PGDATABASE=postgres PGSSLMODE=${PGSSLMODE:-disable}
TERRAFORM=${TERRAFORM:-terraform}
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1

TIMEOUT=$(command -v timeout || command -v gtimeout || true)
[ -n "$TIMEOUT" ] || die "timeout (GNU coreutils) not found; on macOS: brew install coreutils"
command -v psql >/dev/null || die "psql not found"
command -v "$TERRAFORM" >/dev/null || die "terraform not found"
psql -qAtc 'select 1' >/dev/null || die "cannot connect to PostgreSQL at $PGHOST:$PGPORT"

mkdir -p "$workdir"/{logs,cells}
workdir=$(cd "$workdir" && pwd)

# --- providers ---------------------------------------------------------------
labels=()
bins=() # parallel to labels (no associative arrays: macOS ships bash 3.2)
for spec in "${providers[@]}"; do
	label=${spec%%=*}
	path=${spec#*=}
	[ "$label" != "$spec" ] && [ -n "$label" ] || die "bad -p '$spec', want label=path"
	path=$(cd "$path" && pwd) || die "no such directory: $spec"
	dir="$workdir/providers/$label"
	if [ -f "$path/go.mod" ]; then
		log "building provider '$label' from $path ($(git -C "$path" rev-parse --short HEAD 2>/dev/null || echo '?'))"
		mkdir -p "$dir"
		(cd "$path" && go build -o "$dir/terraform-provider-postgresql" .)
	elif [ -x "$path/terraform-provider-postgresql" ]; then
		dir=$path
	else
		die "$path is neither a provider checkout nor a directory with terraform-provider-postgresql"
	fi
	cat >"$workdir/cli-$label.tfrc" <<EOF
provider_installation {
  dev_overrides {
    "cyrilgdn/postgresql" = "$dir"
  }
  direct {}
}
EOF
	bins+=("$dir/terraform-provider-postgresql")
	labels+=("$label")
done
bin_of() { # bin_of <label>
	local i
	for i in "${!labels[@]}"; do
		[ "${labels[$i]}" = "$1" ] && { echo "${bins[$i]}"; return 0; }
	done
	return 1
}
for a in ${extra_args[@]+"${extra_args[@]}"}; do
	bin_of "${a%%=*}" >/dev/null || die "-a '$a' does not match any -p label"
done

# --- helpers -----------------------------------------------------------------
# Leftovers of a hung or failed cell: provider processes that outlived terraform,
# their backends (which may still hold advisory locks), databases and roles.
cleanup() { # cleanup <label>
	pkill -KILL -f "$(bin_of "$1")" 2>/dev/null || true
	psql -qAtX >/dev/null <<'EOF'
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
WHERE application_name = 'Terraform provider' AND pid <> pg_backend_pid();
SELECT format('DROP DATABASE IF EXISTS %I WITH (FORCE)', datname) FROM pg_database
WHERE datname ~ '^smallpool_db_[0-9]+$'
\gexec
SELECT format('DROP ROLE IF EXISTS %I', rolname) FROM pg_roles
WHERE rolname ~ '^smallpool_owner_[0-9]+$'
\gexec
EOF
}

write_config() { # write_config <dir> <label> <max_connections>
	local d
	{
		cat <<EOF
terraform {
  required_providers {
    postgresql = {
      source = "cyrilgdn/postgresql"
    }
  }
}

variable "owner_shift" {
  type    = number
  default = 0
}

provider "postgresql" {
  host            = "$PGHOST"
  port            = $PGPORT
  username        = "$PGUSER"
  password        = "$PGPASSWORD"
  sslmode         = "disable"
  superuser       = true
  connect_timeout = 15
  max_connections = $3
EOF
		for a in ${extra_args[@]+"${extra_args[@]}"}; do
			if [ "${a%%=*}" = "$2" ]; then echo "  ${a#*=}"; fi
		done
		cat <<'EOF'
}

locals {
  owners = [postgresql_role.owner_0.name, postgresql_role.owner_1.name]
}

resource "postgresql_role" "owner_0" {
  name = "smallpool_owner_0"
}

resource "postgresql_role" "owner_1" {
  name = "smallpool_owner_1"
}
EOF
		for d in $(seq 0 $((ndb - 1))); do
			cat <<EOF

resource "postgresql_database" "db_$d" {
  name  = "smallpool_db_$d"
  owner = local.owners[($d + var.owner_shift) % 2]
}

resource "postgresql_schema" "db_$d" {
  name     = "app"
  database = postgresql_database.db_$d.name
  owner    = local.owners[($d + var.owner_shift) % 2]
}
EOF
		done
	} >"$1/main.tf"
}

# --- matrix ------------------------------------------------------------------
results="$workdir/results.csv"
echo "provider,max_connections,parallelism,phase,result,seconds" >"$results"

for label in "${labels[@]}"; do
	for mc in $conns; do
		for par in $pars; do
			cell="$label-c$mc-p$par"
			cdir="$workdir/cells/$cell"
			rm -rf "$cdir"
			mkdir -p "$cdir"
			cleanup "$label"
			write_config "$cdir" "$label" "$mc"
			failed=""
			for phase in create reown destroy; do
				if [ -n "$failed" ]; then
					echo "$label,$mc,$par,$phase,skipped," >>"$results"
					continue
				fi
				case $phase in
				create) args=(apply -auto-approve) ;;
				reown) args=(apply -auto-approve -var owner_shift=1) ;;
				destroy) args=(destroy -auto-approve -var owner_shift=1) ;;
				esac
				t0=$(now)
				rc=0
				TF_CLI_CONFIG_FILE="$workdir/cli-$label.tfrc" "$TIMEOUT" -k 10 "$phase_timeout" \
					"$TERRAFORM" -chdir="$cdir" "${args[@]}" -input=false -no-color -parallelism="$par" \
					>"$workdir/logs/$cell-$phase.log" 2>&1 || rc=$?
				secs=$(echo "$(now) - $t0" | bc)
				case $rc in
				0) result=ok ;;
				124 | 137) result=hang failed=1 ;;
				*) result="fail" failed=1 ;;
				esac
				echo "$label,$mc,$par,$phase,$result,$secs" >>"$results"
				log "$cell $phase: $result (exit $rc, ${secs}s)"
			done
		done
	done
	cleanup "$label"
done

# --- summary -----------------------------------------------------------------
summary="$workdir/summary.md"
{
	echo "$ndb databases with an owner (and a schema in each): create, re-own, destroy;"
	echo "${phase_timeout}s timeout per phase. Cells show the total time, or the first phase"
	echo "that hung or failed."
	echo
	printf '| provider | max_connections |'
	for par in $pars; do printf ' -parallelism=%s |' "$par"; done
	echo
	printf '|---|---:|'
	for _ in $pars; do printf -- '---|'; done
	echo
	for label in "${labels[@]}"; do
		for mc in $conns; do
			printf '| %s | %s |' "$label" "$mc"
			for par in $pars; do
				awk -F, -v l="$label" -v c="$mc" -v p="$par" '
					$1 == l && $2 == c && $3 == p { if ($5 == "ok") t += $6; else if ($5 != "skipped" && !bad) bad = toupper($5) " (" $4 ")" }
					END { if (bad) printf " %s |", bad; else printf " ok (%.0f s) |", t }' "$results"
			done
			echo
		done
	done
} >"$summary"
cat "$summary"
log "raw results: $results, logs: $workdir/logs"
