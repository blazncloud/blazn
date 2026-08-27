#!/bin/sh
set -eu

# Renders the reviewed Phase 5 boundary: the blazn-poc-system and
# blazn-poc-sandboxes namespaces, the tokenless runner ServiceAccount, the
# blazn-poc LocalQueue, the upstream controller's namespace-scoped RBAC, and
# the fail-closed Sandbox admission policy. Non-mutating.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
[ "$#" -eq 1 ] || { printf 'usage: %s OUTPUT_MANIFEST\n' "$0" >&2; exit 64; }
output=$1
[ ! -e "$output" ] || { printf 'output manifest already exists\n' >&2; exit 1; }
output_parent=$(dirname -- "$output")
[ -d "$output_parent" ] || { printf 'output directory does not exist\n' >&2; exit 1; }

: "${BLAZN_PHASE5_TRANSACTION_ID:?export one new UUID for this boundary transaction}"
: "${BLAZN_EXISTING_CLUSTER_QUEUE:?set the reviewed existing ClusterQueue name}"
case "$BLAZN_PHASE5_TRANSACTION_ID" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
  *) printf 'transaction id must be a canonical lowercase UUID\n' >&2; exit 1 ;;
esac
case "$BLAZN_EXISTING_CLUSTER_QUEUE" in
  ''|*[!a-z0-9-]*|-*|*-) printf 'cluster queue name must be a DNS label\n' >&2; exit 1 ;;
esac
[ "${#BLAZN_EXISTING_CLUSTER_QUEUE}" -le 63 ] || { printf 'cluster queue name is too long\n' >&2; exit 1; }

tmp_output=$(mktemp "$output_parent/.boundary.XXXXXX")
sed -e "s|BLAZN_PHASE5_TRANSACTION_ID|$BLAZN_PHASE5_TRANSACTION_ID|g" \
    -e "s|BLAZN_EXISTING_CLUSTER_QUEUE|$BLAZN_EXISTING_CLUSTER_QUEUE|g" \
    "$ROOT/boundary.yaml.in" >"$tmp_output"
if grep -E 'BLAZN_[A-Z0-9_]+' "$tmp_output" >/dev/null; then
  find "$tmp_output" -xdev -maxdepth 0 -delete
  printf 'unrendered placeholders remain\n' >&2
  exit 1
fi
chmod 0400 "$tmp_output"
mv "$tmp_output" "$output"
printf 'Phase 5 boundary rendered\n'
