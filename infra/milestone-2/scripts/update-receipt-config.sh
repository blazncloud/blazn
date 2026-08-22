#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "receipt reconciliation must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "receipt reconciliation must run through with-control-plane-lock.sh"
require_command jq

receipt=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
if [ ! -f "$receipt" ] || [ -L "$receipt" ]; then
  die "control-plane ownership receipt is unavailable"
fi
if [ "$(stat -c '%u' "$receipt")" -ne 0 ] || [ "$(stat -c '%a' "$receipt")" != 600 ]; then
  die "control-plane ownership receipt has unsafe ownership or mode"
fi

jq -e \
  --arg host "$(hostname)" \
  --arg data "${BLAZN_DATA_ROOT:-/srv/frontro/blazn-poc/control-plane}" \
  --arg backup "${BLAZN_BACKUP_ROOT:-}" \
  --arg secrets "${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}" \
  '.schemaVersion == "blazn.dev/control-plane-ownership/v1" and .owner == "blazn-poc" and .host == $host and .paths == {data:$data,backup:$backup,secrets:$secrets}' \
  "$receipt" >/dev/null || die "control-plane ownership receipt does not match this deployment"

digest=sha256:$(control_plane_config_digest "$ROOT_DIR")
tmp=$receipt.tmp.$$
umask 077
jq --arg digest "$digest" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  '.configDigest=$digest | .configUpdatedAt=$updatedAt' "$receipt" >"$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$receipt"
printf 'updated control-plane receipt config digest to %s\n' "$digest"
