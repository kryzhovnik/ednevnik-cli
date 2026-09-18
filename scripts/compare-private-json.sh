#!/bin/sh
set -eu

usage() {
  printf '%s\n' 'usage: compare-private-json.sh EXPECTED.json ACTUAL.json JQ_FILTER' >&2
  exit 2
}

[ "$#" -eq 3 ] || usage
expected=$1
actual=$2
filter=$3

[ -f "$expected" ] || usage
[ -f "$actual" ] || usage
command -v jq >/dev/null 2>&1 || {
  printf '%s\n' 'jq is required' >&2
  exit 2
}

umask 077
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/ednevnik-private-compare.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

jq -S "$filter" "$expected" >"$work_dir/expected.json"
jq -S "$filter" "$actual" >"$work_dir/actual.json"

if cmp -s "$work_dir/expected.json" "$work_dir/actual.json"; then
  printf '%s\n' 'match'
  exit 0
fi

printf '%s\n' 'different'
exit 1
