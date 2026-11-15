#!/usr/bin/env bash
#
# Benchmark `terraform plan -refresh=true` (one Read per resource) for one or more
# provider builds, optionally through a proxy that adds network latency.
#
# See tests/bench/README.md for the full description. Short version:
#
#   tests/bench/run.sh -p base=/path/to/old/checkout -p new=. -l "0 2 10" -n 3
#   tests/bench/run.sh -r            # Terraform core only: terraform_data, no database
#
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: run.sh -p label=path [-p label=path ...] [options]
       run.sh -r [options]

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
  -r             core reference only: plan the same config shape with terraform_data
                 in place of every provider resource. No provider, no PostgreSQL.
  -R             skip the core reference that otherwise runs before the provider plans
  -h             help

Environment:
  PGHOST/PGPORT/PGUSER/PGPASSWORD  superuser connection (default 127.0.0.1:25445,
                                   postgres/postgres)
  PROXY_PORT                       local port for the latency proxy (default PGPORT+1)
  COOLDOWN                         seconds to pause before a plan so TIME_WAIT
                                   sockets expire (default 2*MSL+5, 35 on macOS)
  COOLDOWN_MIN_CONNS               pause only if the previous plan (or the setup) opened
                                   more connections than this (default 500); 0 pauses
                                   before every plan
  GEN_ARGS                         extra flags for genfixture, e.g. "-roles 400 -tables 2000",
                                   "-fixture-dbs 4" to spread the fixture over 4 databases,
                                   "-shape count" for one block with count per resource kind,
                                   or "-preset large" (2,000 roles, 50 schemas, 10,000 tables)
  SAMPLE_INTERVAL                  how often connsampler polls pg_stat_activity during a plan
                                   (default 100ms)
  CORE_STATE                       1: apply the core reference config once and plan it
                                   against that state (default: plan against an empty state)
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
core_only=0
core_ref=1

while getopts "p:a:l:n:P:w:SrRh" opt; do
	case $opt in
	p) providers+=("$OPTARG") ;;
	a) extra_args+=("$OPTARG") ;;
	l) latencies=$OPTARG ;;
	n) runs=$OPTARG ;;
	P) parallelism=$OPTARG ;;
	w) workdir=$OPTARG ;;
	S) skip_setup=1 ;;
	r) core_only=1 ;;
	R) core_ref=0 ;;
	h) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
if [ $core_only = 1 ]; then
	[ ${#providers[@]} = 0 ] || die "-r measures Terraform core alone and takes no -p"
	[ $core_ref = 1 ] || die "-r and -R exclude each other"
else
	[ ${#providers[@]} -gt 0 ] || { usage >&2; exit 2; }
fi

export PGHOST=${PGHOST:-127.0.0.1} PGPORT=${PGPORT:-25445} PGUSER=${PGUSER:-postgres} PGPASSWORD=${PGPASSWORD:-postgres}
export PGDATABASE=postgres PGSSLMODE=${PGSSLMODE:-disable}
PROXY_PORT=${PROXY_PORT:-$((PGPORT + 1))}
TERRAFORM=${TERRAFORM:-terraform}
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1

command -v "$TERRAFORM" >/dev/null || die "terraform not found"

mkdir -p "$workdir"/{bin,tf,logs}
workdir=$(cd "$workdir" && pwd)

# --- tools -------------------------------------------------------------------
log "building genfixture, latencyproxy and connsampler"
(cd "$REPO_DIR" && go build -o "$workdir/bin/" ./tests/bench/genfixture ./tests/bench/latencyproxy ./tests/bench/connsampler)

results="$workdir/results.csv"
echo "provider,rtt_ms,run,seconds,exit_code,queries,connections,peak_conns,p95_conns,peak_idle,peak_by_db" >"$results"

# --- core reference ----------------------------------------------------------
# The same config (same GEN_ARGS: shape, sizes) with terraform_data in place of every
# provider resource: what Terraform core alone costs for this shape. No provider and
# no database; terraform_data is built in, so no init either. Rows go into
# results.csv as provider "core" with RTT "-".
#
# By default the reference plans against an empty state (a plan that creates
# everything), which needs no apply. CORE_STATE=1 applies once and then plans
# against that state like the provider runs do, but the apply itself grows much
# faster than the instance count (minutes at a few thousand instances).
core_apply_secs=""
core_resources=""
core_ok=2
[ "${CORE_STATE:-0}" = 1 ] && core_ok=0
core_reference() {
	local cdir="$workdir/core" run t0 rc secs out blocks
	local -a tfc=(env TF_CLI_CONFIG_FILE=/dev/null "$TERRAFORM" -chdir="$cdir")
	if [ $skip_setup = 0 ] || [ ! -f "$cdir/main.tf" ] || { [ $core_ok = 0 ] && [ ! -f "$cdir/terraform.tfstate" ]; }; then
		rm -rf "$cdir"
		mkdir -p "$cdir"
		# shellcheck disable=SC2086
		out=$("$workdir/bin/genfixture" -core-only -out "$cdir" ${GEN_ARGS:-})
		log "$out"
		if [ $core_ok = 0 ]; then
			log "core reference: terraform apply (-parallelism=$parallelism)"
			t0=$(now)
			"${tfc[@]}" apply -auto-approve -input=false -no-color -parallelism="$parallelism" \
				>"$workdir/logs/core-apply.log" 2>&1 || die "core apply failed, see $workdir/logs/core-apply.log"
			core_apply_secs=$(echo "$(now) - $t0" | bc)
			log "core apply took ${core_apply_secs}s"
		fi
	fi
	if [ $core_ok = 2 ]; then rm -f "$cdir"/terraform.tfstate*; fi
	# Instances: blocks without count count once, blocks with count count N times.
	blocks=$(grep -c '^resource ' "$cdir/main.tf" || true)
	core_resources="$blocks blocks, $(sed -n 's/^  count = //p' "$cdir/main.tf" |
		awk -v b="$blocks" '{ c += $1; n++ } END { print c + b - n }') instances"
	for run in $(seq 1 "$runs"); do
		t0=$(now)
		rc=0
		"${tfc[@]}" plan -refresh=true -input=false -no-color -lock=false -detailed-exitcode \
			-parallelism="$parallelism" >"$workdir/logs/plan-core-$run.log" 2>&1 || rc=$?
		secs=$(echo "$(now) - $t0" | bc)
		echo "core,-,$run,$secs,$rc,n/a,n/a,n/a,n/a,n/a,-" >>"$results"
		log "core run $run/$runs: ${secs}s exit=$rc"
		[ "$rc" = $core_ok ] || log "  plan exit code $rc, want $core_ok, see $workdir/logs/plan-core-$run.log"
	done
}

# core_row prints the summary table row for the core reference.
core_row() {
	awk -F, -v ok="$core_ok" '
		BEGIN { n = 0; all = 0 }
		NR > 1 && $1 == "core" { all++ }
		NR > 1 && $1 == "core" && $5 == ok { t[n++] = $4 }
		END {
			if (!all) exit
			if (!n) { printf "| core (terraform_data) | - | - | - | - | - | - | - | - | 0/%d |\n", all; exit }
			for (i = 1; i < n; i++) { x = t[i]; for (j = i - 1; j >= 0 && t[j] > x; j--) t[j + 1] = t[j]; t[j + 1] = x }
			m = n % 2 ? t[int(n / 2)] : (t[n / 2 - 1] + t[n / 2]) / 2
			printf "| core (terraform_data) | - | %.2f | %.2f-%.2f | - | - | - | - | - | %d/%d |\n", m, t[0], t[n - 1], n, all
		}' "$results"
}

core_note() {
	local against="an empty state (the plan creates everything)"
	[ $core_ok = 2 ] || against="its applied state${core_apply_secs:+ (the apply took ${core_apply_secs}s)}"
	echo "core (terraform_data): Terraform core alone for the same config shape ($core_resources),"
	echo "no provider, no database, planned against $against."
}

if [ $core_only = 1 ]; then
	core_reference
	summary="$workdir/summary.md"
	{
		echo "core reference only, GEN_ARGS=${GEN_ARGS:-}, $runs runs, -parallelism=$parallelism"
		echo
		core_note
		echo
		echo "| provider | added RTT | plan median (s) | min-max (s) | queries | connections | peak conns (max) | p95 conns | peak idle | ok runs |"
		echo "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|"
		core_row
	} >"$summary"
	cat "$summary"
	log "raw results: $results, logs: $workdir/logs"
	exit 0
fi

command -v psql >/dev/null || die "psql not found"
psql -qAtc 'select 1' >/dev/null || die "cannot connect to PostgreSQL at $PGHOST:$PGPORT"

sql() { psql -qAtX -v ON_ERROR_STOP=1 -c "$1"; }

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

# connsampler polls pg_stat_activity (directly, not through the latency proxy) and
# writes one line per sample: time, provider connections, idle ones, per database.
# It is started before stats_reset and stopped after stats_read, so its own session
# never shows up in the connections column; its rows are cut to the plan's window.
sampler_pid=""
sampler_start() { # sampler_start <samples file>
	"$workdir/bin/connsampler" -interval "${SAMPLE_INTERVAL:-100ms}" >"$1" 2>"$1.err" &
	sampler_pid=$!
	local _
	for _ in $(seq 1 100); do
		# PostgreSQL 15+ flushes the session counter up to 1 s after connect;
		# wait so it lands before the stats_reset snapshot.
		[ -s "$1" ] && { sleep 1.5; return 0; }
		kill -0 "$sampler_pid" 2>/dev/null || break
		sleep 0.1
	done
	die "connsampler did not start, see $1.err"
}
sampler_stop() {
	if [ -n "$sampler_pid" ]; then
		kill "$sampler_pid" 2>/dev/null || true
		wait "$sampler_pid" 2>/dev/null || true
	fi
	sampler_pid=""
}

# peak_read <samples file> <t0> <t1> -> "peak p95 peak_idle by_db", where by_db is
# the per-database split of the (first) peak sample, e.g. "bench:10;postgres:4".
peak_read() {
	awk -F'\t' -v a="$2" -v b="$3" '$1 >= a && $1 <= b {
			print $2, $3, ($4 == "" ? "-" : $4)
			if ($2 + 0 > pk) { pk = $2 + 0; db = $4 }
		}
		END { gsub(",", ";", db); print "@", (db == "" ? "-" : db) }' "$1" |
		awk '$1 == "@" { db = $2; next } { c[NR] = $1; if ($2 > mi) mi = $2; n = NR }
			END {
				if (!n) { print "n/a n/a n/a -"; exit }
				asort_n(c, n)
				i = int(n * 0.95 + 0.999999); if (i > n) i = n # nearest rank
				printf "%d %d %d %s\n", c[n], c[i], mi, db
			}
			function asort_n(a, n,   i, j, x) {
				for (i = 2; i <= n; i++) { x = a[i]; for (j = i - 1; j >= 1 && a[j] > x; j--) a[j + 1] = a[j]; a[j + 1] = x }
			}'
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
trap 'stop_proxy; sampler_stop' EXIT

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

# Terraform core alone first: CPU only, no connections, so no cooldown needed.
[ $core_ref = 0 ] || core_reference

# The setup apply opened thousands of connections, so the first plan always waits.
last_conns=999999

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
			# Only plans that open many connections leave enough TIME_WAIT sockets
			# behind to matter; after a plan on a pool that reuses connections
			# (a few dozen) the next one can start right away.
			if [ "$last_conns" -gt "${COOLDOWN_MIN_CONNS:-500}" ] || [ "${COOLDOWN_MIN_CONNS:-500}" = 0 ]; then
				sleep "$COOLDOWN"
			fi
			samples="$workdir/logs/conns-$label-$rtt-$run.tsv"
			sampler_start "$samples"
			before=$(stats_reset)
			t0=$(now)
			rc=0
			tf_plan "$label" plan -refresh=true -input=false -no-color -lock=false -detailed-exitcode \
				-parallelism="$parallelism" -var "pg_port=$port" -var "pg_host=$host" -var "pg_password=$PGPASSWORD" \
				>"$workdir/logs/plan-$label-$rtt-$run.log" 2>&1 || rc=$?
			t1=$(now)
			secs=$(echo "$t1 - $t0" | bc)
			read -r queries conns <<<"$(stats_read "$before")"
			sampler_stop
			read -r peak p95 peak_idle by_db <<<"$(peak_read "$samples" "$t0" "$t1")"
			echo "$label,$rtt,$run,$secs,$rc,$queries,$conns,$peak,$p95,$peak_idle,$by_db" >>"$results"
			log "$label rtt=${rtt}ms run $run/$runs: ${secs}s exit=$rc queries=$queries connections=$conns peak=$peak p95=$p95 idle=$peak_idle by_db=$by_db"
			case $conns in '' | *[!0-9]*) last_conns=999999 ;; *) last_conns=$conns ;; esac
			[ $rc = 0 ] || log "  plan exit code $rc (2 = drift, 1 = error), see $workdir/logs/plan-$label-$rtt-$run.log"
		done
	done
done
stop_proxy

# --- summary -----------------------------------------------------------------
summary="$workdir/summary.md"
{
	echo "$resources resources, $runs runs each, -parallelism=$parallelism, GEN_ARGS=${GEN_ARGS:-}"
	echo
	[ $core_ref = 0 ] || { core_note; echo; }
	echo "Only runs that exited 0 (no changes, no errors) count towards the medians."
	echo "peak / p95 / idle: concurrent provider connections sampled every ${SAMPLE_INTERVAL:-100ms}"
	echo "during the plan (median of the runs; peak also shows the highest run)."
	echo
	echo "| provider | added RTT | plan median (s) | min-max (s) | queries | connections | peak conns (max) | p95 conns | peak idle | ok runs |"
	echo "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|"
	for rtt in $latencies; do
		for label in "${labels[@]}"; do
			awk -F, -v l="$label" -v r="$rtt" '
				BEGIN { n = 0; all = 0 }
				NR > 1 && $1 == l && $2 == r { all++ }
				NR > 1 && $1 == l && $2 == r && $5 == 0 { t[n] = $4; q[n] = $6; c[n] = $7; pk[n] = $8; p95[n] = $9; id[n] = $10; n++ }
				function med(a, n,   i, j, x) {
					for (i = 1; i < n; i++) { x = a[i]; for (j = i - 1; j >= 0 && a[j] > x; j--) a[j + 1] = a[j]; a[j + 1] = x }
					return n % 2 ? a[int(n / 2)] : (a[n / 2 - 1] + a[n / 2]) / 2
				}
				END {
					if (!n) { if (all) printf "| %s | %s ms | - | - | - | - | - | - | - | 0/%d |\n", l, r, all; exit }
					lo = hi = t[0]; for (i = 1; i < n; i++) { if (t[i] < lo) lo = t[i]; if (t[i] > hi) hi = t[i] }
					qs = (q[0] == "n/a") ? "n/a" : sprintf("%d", med(q, n))
					if (pk[0] == "n/a") { ps = "n/a"; p95s = "n/a"; ids = "n/a" } else {
						mx = pk[0]; for (i = 1; i < n; i++) if (pk[i] + 0 > mx + 0) mx = pk[i]
						ps = sprintf("%d (%d)", med(pk, n), mx); p95s = sprintf("%d", med(p95, n)); ids = sprintf("%d", med(id, n))
					}
					printf "| %s | %s ms | %.1f | %.1f-%.1f | %s | %d | %s | %s | %s | %d/%d |\n", l, r, med(t, n), lo, hi, qs, med(c, n), ps, p95s, ids, n, all
				}' "$results"
		done
	done
	[ $core_ref = 0 ] || core_row
	echo
	echo "Connections per database at the peak sample, one entry per ok run:"
	echo
	echo "| provider | added RTT | per database at peak |"
	echo "|---|---:|---|"
	for rtt in $latencies; do
		for label in "${labels[@]}"; do
			awk -F, -v l="$label" -v r="$rtt" '
				NR > 1 && $1 == l && $2 == r && $5 == 0 { gsub(";", ", ", $11); s = s (s == "" ? "" : " / ") $11 }
				END { if (s != "") printf "| %s | %s ms | %s |\n", l, r, s }' "$results"
		done
	done
} >"$summary"
cat "$summary"
log "raw results: $results, logs: $workdir/logs"
