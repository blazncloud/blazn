#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
CREATE=$NODE_ROOT/scripts/create-plan-materials.sh
VERIFY=$NODE_ROOT/scripts/verify-plan-materials.mjs
TEMPLATE=$NODE_ROOT/templates/node-install-plan-template-v1.json
command -v sudo >/dev/null 2>&1 || { printf 'Node plan material test skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'Node plan material test skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-node-plan-$$
mkdir "$top"
cleanup() {
  sudo find "$top" -xdev -type l -print | grep . >/dev/null && { printf 'unexpected symlink in Node plan test root\n' >&2; return 1; }
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

run_create() {
  root=$1; fault=${2:-}; source=${3:-$TEMPLATE}
  sudo env BLAZN_FENCING_TOKEN=44 BLAZN_NODE_PLAN_TEST_MODE=1 BLAZN_NODE_PLAN_FAIL_AFTER="$fault" \
    BLAZN_NODE_PLAN_ROOT="$root/material" BLAZN_NODE_PLAN_CREATE_JOURNAL="$root/journal.json" \
    BLAZN_NODE_PLAN_TEMPLATE_SOURCE="$source" "$CREATE"
}

for fault in initialized tree-created key-written metadata-written template-written published; do
  root=$top/$fault; mkdir "$root"
  if run_create "$root" "$fault" >"$root/first.out" 2>"$root/first.err"; then printf 'plan material fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected plan-material fault after $fault" "$root/first.err" >/dev/null
  before=
  if sudo test -f "$root/material/signing-public-v1.json"; then before=$(sudo jq -r .publicKeyFingerprint "$root/material/signing-public-v1.json"); fi
  run_create "$root" >"$root/retry.out"
  after=$(sudo jq -r .publicKeyFingerprint "$root/material/signing-public-v1.json")
  [ -z "$before" ] || [ "$before" = "$after" ] || { printf 'plan signing identity changed across retry: %s\n' "$fault" >&2; exit 1; }
  verified=$(sudo env BLAZN_NODE_PLAN_ROOT="$root/material" BLAZN_NODE_PLAN_SOURCE_TEMPLATES="$NODE_ROOT/templates" node "$VERIFY")
  printf '%s\n' "$verified" >"$root/verify.out"
  sudo jq -e '.status=="ok" and .keyId=="control-plane-node-plan/v1" and .templateId=="frontro-poc-worker/v1"' "$root/verify.out" >/dev/null
  private=$(sudo sed -n '1p' "$root/material/signing-private-v1.b64url")
  private_standard=$(printf '%s' "$private" | tr '_-' '/+')
  private_hex=$(printf '%s' "$private" | openssl base64 -d -A 2>/dev/null | od -An -tx1 | tr -d ' \n')
  for encoding in "$private" "$private_standard" "$private_hex"; do
    if sudo grep -F -- "$encoding" "$root/journal.json" "$root"/*.out "$root"/*.err >/dev/null; then printf 'derived plan signing key encoding leaked into evidence: %s\n' "$fault" >&2; exit 1; fi
  done
done

drift=$top/template-drift; mkdir "$drift"; cp "$TEMPLATE" "$drift/source.json"
if run_create "$drift" metadata-written "$drift/source.json" >"$drift/first.out" 2>"$drift/first.err"; then printf 'template-drift setup unexpectedly completed\n' >&2; exit 1; fi
printf '\n' >>"$drift/source.json"
if run_create "$drift" '' "$drift/source.json" >"$drift/retry.out" 2>"$drift/retry.err"; then printf 'template drift unexpectedly passed recovery\n' >&2; exit 1; fi
grep -F 'source digest changed during recovery' "$drift/retry.err" >/dev/null

symlink=$top/symlink; mkdir "$symlink" "$symlink/real"; ln -s "$symlink/real" "$symlink/material"
if run_create "$symlink" >"$symlink/out" 2>"$symlink/err"; then printf 'symlinked plan material root unexpectedly passed\n' >&2; exit 1; fi
grep -F 'symbolic link' "$symlink/err" >/dev/null
unlink "$symlink/material"

trap - EXIT HUP INT TERM
cleanup
printf 'Node plan material crash-resume and secret-evidence tests passed\n'
