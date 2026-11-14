#!/usr/bin/env bash
#
# Benchmark `terraform plan -refresh=true` (one Read per resource) for one or more
# provider builds, optionally through a proxy that adds network latency.
#
# See tests/bench/README.md for the full description. Short version:
#
#   tests/bench/run.sh -p base=/path/to/old/checkout -p new=. -l "0 2 10" -n 3
#
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: run.sh -p label=path [-p label=path ...] [options]

  -p label=path  provider to measure (repeatable). path is either a directory that
                 contains a terraform-provider-postgresql binary, or a provider source
                 checkout (a directory with go.mod), which is built into the work dir.
  -a label='arg = value'
                 extra provider argument for one label (repeatable), written into a
                 Terraform override file for that label's plans only, e.g.
                 -a 'pooled=max_idle_connections = 10'. The same path can be given
                 under several labels to compare settings of one build.
  -l "RTTs"      added round-trip latencies in ms, space separated (default "0 2 10").
                 0 connects directly; anything else goes through latencyproxy.
  -n runs        timed plans per provider and latency (default 3)
  -P n           terraform -parallelism (default 10, the terraform default)
  -w dir         work dir (default ${TMPDIR:-/tmp}/pg-tf-bench)
  -S             skip setup: reuse the fixture and state from a previous run in -w
  -h             help

Environment:
  PGHOST/PGPORT/PGUSER/PGPASSWORD  superuser connection (default 127.0.0.1:25445,
                                   postgres/postgres)
  PROXY_PORT                       local port for the latency proxy (default PGPORT+1)
  COOLDOWN                         seconds to pause before each plan so TIME_WAIT
                                   sockets expire (default 2*MSL+5, 35 on macOS)
  GEN_ARGS                         extra flags for genfixture, e.g. "-roles 400 -tables 2000"
  TERRAFORM                        terraform binary (default: terraform from PATH)
EOF
}

die() { echo "error: $*" >&2; exit 1; }
log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }

BENCH_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(cd "$BENCH_DIR/../.." && pwd)

providers=()
extra_args=()
latencies="0 2 10"
runs=3
parallelism=10
workdir="${TMPDIR:-/tmp}"
workdir="${workdir%/}/pg-tf-bench"
skip_setup=0

while getopts "p:a:l:n:P:w:Sh" opt; do
	case $opt in
	p) providers+=("$OPTARG") ;;
	a) extra_args+=("$OPTARG") ;;
	l) latencies=$OPTARG ;;
	n) runs=$OPTARG ;;
	P) parallelism=$OPTARG ;;
	w) workdir=$OPTARG ;;
	S) skip_setup=1 ;;
	h) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
[ ${#providers[@]} -gt 0 ] || { usage >&2; exit 2; }

export PGHOST=${PGHOST:-127.0.0.1} PGPORT=${PGPORT:-25445} PGUSER=${PGUSER:-postgres} PGPASSWORD=${PGPASSWORD:-postgres}
export PGDATABASE=postgres PGSSLMODE=${PGSSLMODE:-disable}
PROXY_PORT=${PROXY_PORT:-$((PGPORT + 1))}
TERRAFORM=${TERRAFORM:-terraform}
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1

command -v psql >/dev/null || die "psql not found"
command -v "$TERRAFORM" >/dev/null || die "terraform not found"
psql -qAtc 'select 1' >/dev/null || die "cannot connect to PostgreSQL at $PGHOST:$PGPORT"

mkdir -p "$workdir"/{bin,tf,logs}
workdir=$(cd "$workdir" && pwd)

sql() { psql -qAtX -v ON_ERROR_STOP=1 -c "$1"; }

# --- tools -------------------------------------------------------------------
log "building genfixture and latencyproxy"
(cd "$REPO_DIR" && go build -o "$workdir/bin/" ./tests/bench/genfixture ./tests/bench/latencyproxy)

# --- providers ---------------------------------------------------------------
labels=()
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
	labels+=("$label")
done

# Plans run from a per-label directory that links the shared config and state, plus
# an override file with that label's extra provider arguments, if any.
for label in "${labels[@]}"; do
	pdir="$workdir/plan/$label"
	rm -rf "$pdir"
	mkdir -p "$pdir"
	ln -s ../../tf/main.tf "$pdir/main.tf"
	ln -s ../../tf/terraform.tfstate "$pdir/terraform.tfstate"
	body=""
	for a in ${extra_args[@]+"${extra_args[@]}"}; do
		if [ "${a%%=*}" = "$label" ]; then body="$body  ${a#*=}"$'\n'; fi
	done
	if [ -n "$body" ]; then
		printf 'provider "postgresql" {\n%s}\n' "$body" >"$pdir/provider_override.tf"
		log "provider '$label' plans with extra arguments:"$'\n'"$body"
	fi
done
for a in ${extra_args[@]+"${extra_args[@]}"}; do
	[ -d "$workdir/plan/${a%%=*}" ] || die "-a '$a' does not match any -p label"
done

tf() { # tf <label> <args...>
	local label=$1
	shift
	TF_CLI_CONFIG_FILE="$workdir/cli-$label.tfrc" "$TERRAFORM" -chdir="$workdir/tf" "$@"
}

tf_plan() { # tf_plan <label> <args...>: plan from the label's directory
	local label=$1
	shift
	TF_CLI_CONFIG_FILE="$workdir/cli-$label.tfrc" "$TERRAFORM" -chdir="$workdir/plan/$label" "$@"
}

# --- statistics --------------------------------------------------------------
have_pgss=0
if [ "$(sql "select count(*) from pg_settings where name = 'shared_preload_libraries' and setting like '%pg_stat_statements%'")" = 1 ]; then
	sql "create extension if not exists pg_stat_statements" >/dev/null
	have_pgss=1
else
	log "pg_stat_statements is not preloaded; the queries column will be empty"
fi

stats_reset() {
	[ $have_pgss = 1 ] && sql "select pg_stat_statements_reset()" >/dev/null
	# sessions (PG 14+) counts every backend started; it is flushed when backends
	# exit, which is why the snapshot is taken before and after.
	sql "select sum(sessions) from pg_stat_database"
}

stats_read() { # stats_read <sessions before> -> "queries connections"
	local before=$1 q="'n/a'" row
	sleep 1 # let the provider's backends exit and flush their stats
	[ $have_pgss = 1 ] && q="(select coalesce(sum(calls), 0) from pg_stat_statements where query not like '%pg_stat%')"
	# One session for both reads, so only the "before" session needs subtracting.
	row=$(sql "select $q, (select sum(sessions) from pg_stat_database)")
	echo "${row%%|*} $((${row##*|} - before - 1))"
}

# --- setup -------------------------------------------------------------------
if [ $skip_setup = 0 ]; then
	log "generating fixture (GEN_ARGS=${GEN_ARGS:-})"
	# shellcheck disable=SC2086
	"$workdir/bin/genfixture" -out "$workdir/tf" ${GEN_ARGS:-}
	log "dropping leftovers and loading fixture.sql"
	psql -qX -f "$workdir/tf/cleanup.sql" >/dev/null 2>&1 || true
	rm -f "$workdir"/tf/terraform.tfstate*
	psql -qX -v ON_ERROR_STOP=1 -f "$workdir/tf/fixture.sql" >/dev/null

	# Serial apply: concurrent GRANT/REVOKE on the same tables can fail with "tuple
	# concurrently updated". Setup time is not what we measure.
	log "terraform apply with '${labels[0]}' (one time, -parallelism=1, creates everything)"
	t0=$(now)
	tf "${labels[0]}" apply -auto-approve -input=false -no-color -parallelism=1 \
		-var "pg_port=$PGPORT" -var "pg_host=$PGHOST" -var "pg_password=$PGPASSWORD" >"$workdir/logs/apply.log" 2>&1 ||
		die "apply failed, see $workdir/logs/apply.log"
	log "apply took $(echo "$(now) - $t0" | bc)s"
else
	[ -f "$workdir/tf/terraform.tfstate" ] || die "-S given but there is no state in $workdir/tf"
fi
resources=$(tf "${labels[0]}" state list 2>/dev/null | wc -l | tr -d ' ')
log "$resources resources in state"

# --- measure -----------------------------------------------------------------
proxy_pid=""
stop_proxy() {
	if [ -n "$proxy_pid" ]; then
		kill "$proxy_pid" 2>/dev/null || true
		wait "$proxy_pid" 2>/dev/null || true # 143: terminated by our SIGTERM
	fi
	proxy_pid=""
}
trap stop_proxy EXIT

# Every plan opens thousands of short-lived connections (each one takes an
# ephemeral port in the client, and another in the latency proxy or a container
# port forwarder). Their TIME_WAIT entries pin those ports for 2*MSL, and
# back-to-back plans then fail with "can't assign requested address". A pause
# before each plan lets them expire so every run starts from the same state.
# Socket listings are not reliable across platforms, hence a fixed pause:
# 2*MSL + 5s on macOS (35s by default), 65s elsewhere (Linux TIME_WAIT is 60s).
if [ -z "${COOLDOWN:-}" ]; then
	msl=$(sysctl -n net.inet.tcp.msl 2>/dev/null || true)
	if [ -n "$msl" ]; then COOLDOWN=$((2 * msl / 1000 + 5)); else COOLDOWN=65; fi
fi

results="$workdir/results.csv"
echo "provider,rtt_ms,run,seconds,exit_code,queries,connections" >"$results"

for rtt in $latencies; do
	stop_proxy
	port=$PGPORT
	if [ "$rtt" != 0 ]; then
		port=$PROXY_PORT
		half=$(echo "scale=3; $rtt / 2" | bc)
		"$workdir/bin/latencyproxy" -listen "127.0.0.1:$port" -upstream "$PGHOST:$PGPORT" -delay "${half}ms" \
			2>"$workdir/logs/proxy-$rtt.log" &
		proxy_pid=$!
		sleep 0.5
		kill -0 "$proxy_pid" 2>/dev/null || die "latencyproxy did not start, see $workdir/logs/proxy-$rtt.log"
	fi
	host=$PGHOST
	[ "$rtt" != 0 ] && host=127.0.0.1

	# Runs are interleaved across providers so drift on the machine hits all of them.
	for run in $(seq 1 "$runs"); do
		for label in "${labels[@]}"; do
			sleep "$COOLDOWN"
			before=$(stats_reset)
			t0=$(now)
			rc=0
			tf_plan "$label" plan -refresh=true -input=false -no-color -lock=false -detailed-exitcode \
				-parallelism="$parallelism" -var "pg_port=$port" -var "pg_host=$host" -var "pg_password=$PGPASSWORD" \
				>"$workdir/logs/plan-$label-$rtt-$run.log" 2>&1 || rc=$?
			secs=$(echo "$(now) - $t0" | bc)
			read -r queries conns <<<"$(stats_read "$before")"
			echo "$label,$rtt,$run,$secs,$rc,$queries,$conns" >>"$results"
			log "$label rtt=${rtt}ms run $run/$runs: ${secs}s exit=$rc queries=$queries connections=$conns"
			[ $rc = 0 ] || log "  plan exit code $rc (2 = drift, 1 = error), see $workdir/logs/plan-$label-$rtt-$run.log"
		done
	done
done
stop_proxy

# --- summary -----------------------------------------------------------------
summary="$workdir/summary.md"
{
	echo "$resources resources, $runs runs each, -parallelism=$parallelism"
	echo
	echo "Only runs that exited 0 (no changes, no errors) count towards the medians."
	echo
	echo "| provider | added RTT | plan median (s) | min-max (s) | queries | connections | ok runs |"
	echo "|---|---:|---:|---:|---:|---:|---:|"
	for rtt in $latencies; do
		for label in "${labels[@]}"; do
			awk -F, -v l="$label" -v r="$rtt" '
				BEGIN { n = 0; all = 0 }
				NR > 1 && $1 == l && $2 == r { all++ }
				NR > 1 && $1 == l && $2 == r && $5 == 0 { t[n] = $4; q[n] = $6; c[n] = $7; n++ }
				function med(a, n,   i, j, x) {
					for (i = 1; i < n; i++) { x = a[i]; for (j = i - 1; j >= 0 && a[j] > x; j--) a[j + 1] = a[j]; a[j + 1] = x }
					return n % 2 ? a[int(n / 2)] : (a[n / 2 - 1] + a[n / 2]) / 2
				}
				END {
					if (!n) { if (all) printf "| %s | %s ms | - | - | - | - | 0/%d |\n", l, r, all; exit }
					lo = hi = t[0]; for (i = 1; i < n; i++) { if (t[i] < lo) lo = t[i]; if (t[i] > hi) hi = t[i] }
					qs = (q[0] == "n/a") ? "n/a" : sprintf("%d", med(q, n))
					printf "| %s | %s ms | %.1f | %.1f-%.1f | %s | %d | %d/%d |\n", l, r, med(t, n), lo, hi, qs, med(c, n), n, all
				}' "$results"
		done
	done
} >"$summary"
cat "$summary"
log "raw results: $results, logs: $workdir/logs"
