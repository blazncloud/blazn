#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
CREATE=$TEST_DIR/../scripts/create-secrets.sh
command -v sudo >/dev/null 2>&1 || { printf 'Node secret-create resume test skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'Node secret-create resume test skipped: passwordless sudo unavailable\n'; exit 0; }
top=${TMPDIR:-/tmp}/blazn-node-secret-create-$$
mkdir "$top"
cleanup() { sudo find "$top" -xdev -type l -print | grep . >/dev/null && return 1; sudo find "$top" -xdev -type f -delete; sudo find "$top" -xdev -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM

run_create() {
  root=$1; fault=${2:-}
  sudo env BLAZN_FENCING_TOKEN=3 BLAZN_NODE_INFRA_TEST_MODE=1 BLAZN_NODE_SECRET_TEST_FAIL_AFTER="$fault" \
    BLAZN_NODE_BROKER_SECRETS_ROOT="$root/etc/node-broker/secrets" \
    BLAZN_NODE_BROKER_CREATE_JOURNAL="$root/ownership/secret-create.json" "$CREATE"
}

for fault in initialized tree-created database-written hmac-written join-written published; do
  root=$top/$fault
  mkdir -p "$root/etc" "$root/ownership"
  sudo chown -R 0:0 "$root"
  sudo chmod 0700 "$root" "$root/etc" "$root/ownership"
  if run_create "$root" "$fault" >"$top/$fault-first.out" 2>"$top/$fault-first.err"; then printf 'secret fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected secret-create fault after $fault" "$top/$fault-first.err" >/dev/null
  run_create "$root" >"$top/$fault-retry.out"
  sudo jq -e '.phase=="published"' "$root/ownership/secret-create.json" >/dev/null
  before=$(sudo sha256sum "$root/etc/node-broker/secrets/database-url" "$root/etc/node-broker/secrets/enrollment-hmac-v1" "$root/etc/node-broker/secrets/join-credential-v1")
  run_create "$root" >"$top/$fault-idempotent.out"
  after=$(sudo sha256sum "$root/etc/node-broker/secrets/database-url" "$root/etc/node-broker/secrets/enrollment-hmac-v1" "$root/etc/node-broker/secrets/join-credential-v1")
  [ "$before" = "$after" ] || { printf 'secret retry changed generation: %s\n' "$fault" >&2; exit 1; }
  [ "$(sudo find "$root/etc" -maxdepth 1 -name '.node-broker-create-*' -print | wc -l)" -eq 0 ] || { printf 'secret staging residue remained\n' >&2; exit 1; }
done
trap - EXIT HUP INT TERM
cleanup
printf 'fresh and upgrade secret creation intra-generation fault matrix passed\n'
