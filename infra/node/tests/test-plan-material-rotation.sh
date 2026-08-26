#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
CREATE=$NODE_ROOT/scripts/create-plan-materials.sh
ROTATE=$NODE_ROOT/scripts/rotate-plan-materials.sh
PLAN_OBJECT=$NODE_ROOT/scripts/plan-material-object.sh
TEMPLATE=$NODE_ROOT/templates/node-install-plan-template-v1.json
command -v sudo >/dev/null 2>&1 || { printf 'Node plan rotation test skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'Node plan rotation test skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-node-plan-rotation-$$
mkdir "$top"
cleanup() {
  sudo find "$top" -xdev -type l -print | grep . >/dev/null && { printf 'unexpected symlink in Node plan rotation test root\n' >&2; return 1; }
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

fixture() {
  name=$1; root=$top/$name
  mkdir -p "$root/ownership"
  cp "$TEMPLATE" "$root/source.json"
  jq '.profiles["ubuntu-26.04-amd64-worker/v1"].amd64.labels["blazn.dev/rotation-test"]="prior"' "$TEMPLATE" >"$root/prior.json"
  sudo chown 0:0 "$root/source.json" "$root/prior.json" "$root/ownership"
  sudo chmod 0444 "$root/source.json" "$root/prior.json"
  sudo chmod 0700 "$root/ownership"
  sudo env BLAZN_FENCING_TOKEN=1 BLAZN_NODE_PLAN_TEST_MODE=1 \
    BLAZN_NODE_PLAN_ROOT="$root/material" BLAZN_NODE_PLAN_CREATE_JOURNAL="$root/ownership/journal.json" \
    BLAZN_NODE_PLAN_TEMPLATE_SOURCE="$root/prior.json" "$CREATE" >/dev/null
  plan=$(sudo env BLAZN_NODE_PLAN_TEST_MODE=1 BLAZN_NODE_PLAN_ROOT="$root/material" \
    BLAZN_NODE_PLAN_CREATE_JOURNAL="$root/ownership/journal.json" "$PLAN_OBJECT")
  jq -cn --arg host "$(hostname)" --argjson plan "$plan" \
    '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host,nodePlan:$plan}' >"$root/main.tmp"
  jq -cn --arg host "$(hostname)" --argjson plan "$plan" \
    '{schemaVersion:"blazn.dev/node-broker-upgrade/v2",owner:"blazn-poc",host:$host,phase:"complete",nodePlan:$plan,updatedAt:"2026-08-26T00:00:00Z"}' >"$root/upgrade.tmp"
  sudo install -o root -g root -m 0600 "$root/main.tmp" "$root/ownership/main.json"
  sudo install -o root -g root -m 0600 "$root/upgrade.tmp" "$root/ownership/upgrade.json"
  rm "$root/main.tmp" "$root/upgrade.tmp"
  printf '%s\n' "$root"
}

run_rotate() {
  rr_root=$1; rr_correlation=$2; rr_fault=${3:-}; rr_mode=${4:-forward}
  rr_rollback=
  [ "$rr_mode" = forward ] || rr_rollback=--rollback
  sudo env BLAZN_FENCING_TOKEN=2 BLAZN_CORRELATION_ID="$rr_correlation" \
    BLAZN_NODE_PLAN_ROTATION_TEST_MODE=1 BLAZN_NODE_PLAN_ROTATION_FAIL_AFTER="$rr_fault" \
    BLAZN_NODE_PLAN_ROOT="$rr_root/material" BLAZN_NODE_PLAN_CREATE_JOURNAL="$rr_root/ownership/journal.json" \
    BLAZN_RECEIPT_PATH="$rr_root/ownership/main.json" BLAZN_NODE_BROKER_UPGRADE_RECEIPT="$rr_root/ownership/upgrade.json" \
    BLAZN_NODE_PLAN_ROTATION_ROOT="$rr_root/ownership/rotations" BLAZN_NODE_PLAN_TEMPLATE_SOURCE="$rr_root/source.json" \
    BLAZN_NODE_PLAN_SOURCE_TEMPLATES="$NODE_ROOT/templates" "$ROTATE" $rr_rollback
}

for fault in initialized template-published journal-published upgrade-receipt-published main-receipt-published complete; do
  root=$(fixture "forward-$fault")
  private_before=$(sudo sha256sum "$root/material/signing-private-v1.b64url" | awk '{print $1}')
  if run_rotate "$root" rotation "$fault" >"$root/first.out" 2>"$root/first.err"; then
    printf 'Node plan rotation fault unexpectedly completed: %s\n' "$fault" >&2; exit 1
  fi
  grep -F "injected Node plan rotation fault after $fault" "$root/first.err" >/dev/null
  run_rotate "$root" rotation >"$root/retry.out"
  sudo jq -e '.phase=="complete"' "$root/ownership/rotations/rotation/receipt.json" >/dev/null
  sudo jq -e '[.profiles[][] .components[] | select(.sourceClass=="current_binary") | .version] | all(.=="v0.1.0-poc.104")' "$root/material/node-install-plan-template-v1.json" >/dev/null
  plan=$(sudo env BLAZN_NODE_PLAN_TEST_MODE=1 BLAZN_NODE_PLAN_ROOT="$root/material" BLAZN_NODE_PLAN_CREATE_JOURNAL="$root/ownership/journal.json" "$PLAN_OBJECT")
  [ "$(printf '%s' "$plan" | jq -cS .)" = "$(sudo jq -cS .nodePlan "$root/ownership/main.json")" ]
  [ "$(printf '%s' "$plan" | jq -cS .)" = "$(sudo jq -cS .nodePlan "$root/ownership/upgrade.json")" ]
  [ "$private_before" = "$(sudo sha256sum "$root/material/signing-private-v1.b64url" | awk '{print $1}')" ] || { printf 'Node plan signing key changed during rotation\n' >&2; exit 1; }
done

for fault in rollback-main-restored rollback-upgrade-restored rollback-journal-restored rolled-back; do
  root=$(fixture "rollback-$fault")
  if run_rotate "$root" rotation main-receipt-published >"$root/forward.out" 2>"$root/forward.err"; then
    printf 'forward setup unexpectedly completed\n' >&2; exit 1
  fi
  if run_rotate "$root" rotation "$fault" rollback >"$root/rollback-first.out" 2>"$root/rollback-first.err"; then
    printf 'Node plan rollback fault unexpectedly completed: %s\n' "$fault" >&2; exit 1
  fi
  grep -F "injected Node plan rotation fault after $fault" "$root/rollback-first.err" >/dev/null
  run_rotate "$root" rotation '' rollback >"$root/rollback-retry.out"
  sudo jq -e '.phase=="rolled-back"' "$root/ownership/rotations/rotation/receipt.json" >/dev/null
  sudo cmp "$root/material/node-install-plan-template-v1.json" "$root/ownership/rotations/rotation/before/template.json"
  sudo cmp "$root/ownership/journal.json" "$root/ownership/rotations/rotation/before/journal.json"
  sudo cmp "$root/ownership/main.json" "$root/ownership/rotations/rotation/before/main-receipt.json"
  sudo cmp "$root/ownership/upgrade.json" "$root/ownership/rotations/rotation/before/upgrade-receipt.json"
done

drift=$(fixture drift)
if run_rotate "$drift" rotation initialized >"$drift/first.out" 2>"$drift/first.err"; then printf 'drift setup unexpectedly completed\n' >&2; exit 1; fi
sudo sh -c 'jq '\'' .unexpected=true '\'' "$1" >"$2"' sh "$drift/ownership/main.json" "$drift/main-drift.tmp"
sudo install -o root -g root -m 0600 "$drift/main-drift.tmp" "$drift/ownership/main.json"
sudo rm "$drift/main-drift.tmp"
if run_rotate "$drift" rotation >"$drift/retry.out" 2>"$drift/retry.err"; then printf 'unreceipted drift unexpectedly passed\n' >&2; exit 1; fi
grep -F 'observed unreceipted content' "$drift/retry.err" >/dev/null

printf 'Node plan material rotation tests passed\n'
