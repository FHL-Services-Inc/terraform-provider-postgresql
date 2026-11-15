#!/usr/bin/env bash
#
# Split the tests of the postgresql package into deterministic shards for CI.
#
#   scripts/test-shard.sh list                  all tests, sorted (one per line)
#   scripts/test-shard.sh tests  <index> <total> tests of one shard (0-based index)
#   scripts/test-shard.sh regex  <index> <total> a `go test -run` regex for one shard
#   scripts/test-shard.sh verify <total>         check that the shards cover every test
#
# Tests are sorted (LC_ALL=C) and dealt round-robin, so shard membership only
# depends on the set of test names. `verify` fails when a test would be run by
# no shard or by more than one, and when a shard's regex does not select
# exactly the tests assigned to it (checked with `go test -list`, which matches
# names the same way as `-run`).
set -euo pipefail

PKG=${PKG:-./postgresql/}
export LC_ALL=C

list_matching() {
	# Only top-level Test/Fuzz/Example functions are selected by -run.
	go test -list "$1" "$PKG" | grep -E '^(Test|Fuzz|Example)' | sort
}

all_tests() {
	if [[ -z "${_ALL_TESTS:-}" ]]; then
		_ALL_TESTS=$(list_matching '.*')
		if [[ -z "$_ALL_TESTS" ]]; then
			echo "no tests found in $PKG" >&2
			exit 1
		fi
	fi
	printf '%s\n' "$_ALL_TESTS"
}

check_shard_args() {
	local index=$1 total=$2
	if ! [[ "$index" =~ ^[0-9]+$ && "$total" =~ ^[1-9][0-9]*$ ]] || ((index >= total)); then
		echo "invalid shard $index/$total" >&2
		exit 2
	fi
}

shard_tests() {
	local index=$1 total=$2
	all_tests | awk -v i="$index" -v n="$total" '(NR - 1) % n == i'
}

shard_regex() {
	local tests
	tests=$(shard_tests "$1" "$2")
	if [[ -z "$tests" ]]; then
		echo "shard $1/$2 is empty" >&2
		exit 1
	fi
	printf '^(%s)$\n' "$(printf '%s\n' "$tests" | paste -sd '|' -)"
}

verify() {
	local total=$1 i assigned selected union
	check_shard_args 0 "$total"
	union=""
	for ((i = 0; i < total; i++)); do
		assigned=$(shard_tests "$i" "$total")
		selected=$(list_matching "$(shard_regex "$i" "$total")")
		if [[ "$assigned" != "$selected" ]]; then
			echo "shard $i/$total: regex selects a different set of tests than assigned" >&2
			diff <(printf '%s\n' "$assigned") <(printf '%s\n' "$selected") >&2 || true
			exit 1
		fi
		echo "shard $i/$total: $(printf '%s\n' "$assigned" | wc -l | tr -d ' ') tests"
		union+="$assigned"$'\n'
	done
	union=$(printf '%s' "$union" | sort)
	if [[ "$union" != "$(all_tests)" ]]; then
		echo "the union of the shards differs from the full test list:" >&2
		diff <(all_tests) <(printf '%s\n' "$union") >&2 || true
		exit 1
	fi
	echo "OK: $(all_tests | wc -l | tr -d ' ') tests, each in exactly one of $total shards"
}

cmd=${1:-}
case "$cmd" in
list | tests | regex | verify) _ALL_TESTS=$(all_tests) ;;
esac
case "$cmd" in
list) all_tests ;;
tests)
	check_shard_args "${2:-}" "${3:-}"
	shard_tests "$2" "$3"
	;;
regex)
	check_shard_args "${2:-}" "${3:-}"
	shard_regex "$2" "$3"
	;;
verify) verify "${2:-}" ;;
*)
	sed -n '3,8p' "$0" >&2
	exit 2
	;;
esac
