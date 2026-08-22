#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
UPGRADE=$ROOT_DIR/scripts/upgrade-live-v2-to-workspace.sh
command -v sudo >/dev/null 2>&1 || { printf 'workspace-secret upgrade tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'workspace-secret upgrade tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-workspace-secret-test-$$
mkdir "$top"
cleanup() {
  if sudo find "$top" -xdev -type l -print | grep . >/dev/null; then
    printf 'unexpected symlink in workspace-secret test root\n' >&2
    return 1
  fi
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

fixture() {
  name=$1
  root=$top/$name
  secrets=$root/secrets
  ownership=$root/ownership
  mkdir -p "$secrets" "$ownership"
  chmod 0700 "$secrets" "$ownership"
  jq -cn --arg host "$(hostname)" --arg secrets "$secrets" \
    '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host,paths:{secrets:$secrets}}' >"$ownership/control-plane.json"
  chmod 0600 "$ownership/control-plane.json"
  sudo chown -R 0:0 "$secrets" "$ownership"
  printf '%s\n' "$root"
}

run_upgrade() {
  root=$1
  sudo env \
    BLAZN_FENCING_TOKEN=9 \
    BLAZN_SECRETS_ROOT="$root/secrets" \
    BLAZN_RECEIPT_PATH="$root/ownership/control-plane.json" \
    BLAZN_WORKSPACE_SECRET_UPGRADE_RECEIPT_PATH="$root/ownership/workspace-secret-upgrade.json" \
    "$UPGRADE"
}

normal=$(fixture normal)
run_upgrade "$normal" >"$normal/run-1.out"
secret=$normal/secrets/workspace-invitation-hmac-v1
[ "$(sudo wc -c "$secret" | awk '{print $1}')" = 65 ]
sudo grep -Eq '^[a-f0-9]{64}$' "$secret"
[ "$(sudo stat -c '%u:%a' "$secret")" = 0:444 ]
sudo jq -e '.schemaVersion == "blazn.dev/workspace-secret-upgrade/v1" and (.secretDigest | test("^sha256:[a-f0-9]{64}$"))' \
  "$normal/ownership/workspace-secret-upgrade.json" >/dev/null
before=$(sudo sha256sum "$secret")
run_upgrade "$normal" >"$normal/run-2.out"
after=$(sudo sha256sum "$secret")
[ "$before" = "$after" ] || { printf 'idempotent retry changed the workspace invitation key\n' >&2; exit 1; }
if grep -E '[a-f0-9]{64}' "$normal"/*.out >/dev/null; then
  printf 'workspace secret upgrade emitted secret or digest material\n' >&2
  exit 1
fi

partial=$(fixture partial)
sudo mkdir "$partial/secrets/.workspace-secret-upgrade-staging"
sudo chmod 0700 "$partial/secrets/.workspace-secret-upgrade-staging"
sudo sh -c 'printf "%064d\n" 7 >"$1/.workspace-secret-upgrade-staging/workspace-invitation-hmac-v1"' sh "$partial/secrets"
sudo chmod 0444 "$partial/secrets/.workspace-secret-upgrade-staging/workspace-invitation-hmac-v1"
run_upgrade "$partial" >"$partial/resumed.out"
[ ! -e "$partial/secrets/.workspace-secret-upgrade-staging" ]
sudo grep -Eq '^[a-f0-9]{64}$' "$partial/secrets/workspace-invitation-hmac-v1"

ambiguous=$(fixture ambiguous)
sudo sh -c 'printf "%064d\n" 8 >"$1/workspace-invitation-hmac-v1"' sh "$ambiguous/secrets"
sudo chmod 0444 "$ambiguous/secrets/workspace-invitation-hmac-v1"
if run_upgrade "$ambiguous" >"$ambiguous/out" 2>"$ambiguous/err"; then
  printf 'unreceipted pre-existing workspace secret unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'exists without an upgrade receipt' "$ambiguous/err" >/dev/null

corrupt=$(fixture corrupt)
run_upgrade "$corrupt" >"$corrupt/first.out"
sudo chmod 0644 "$corrupt/secrets/workspace-invitation-hmac-v1"
if run_upgrade "$corrupt" >"$corrupt/out" 2>"$corrupt/err"; then
  printf 'unsafe workspace secret mode unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'unexpected mode' "$corrupt/err" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'workspace-secret clean, retry, partial-resume, and fail-closed tests passed\n'
