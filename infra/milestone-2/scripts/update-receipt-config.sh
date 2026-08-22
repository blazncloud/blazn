#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "receipt reconciliation must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "receipt reconciliation must run through with-control-plane-lock.sh"
require_command jq
require_command docker
backup_root=${BLAZN_BACKUP_ROOT:-}
[ -n "$backup_root" ] || die "BLAZN_BACKUP_ROOT is required"
assert_approved_backup_mount "$backup_root"
load_control_api_image "$ROOT_DIR"
control_api_build_receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
control_api_source=$(jq -er .sourceDigest "$control_api_build_receipt")
control_api_image=$(jq -er .image "$control_api_build_receipt")
control_api_image_id=$(jq -er .imageId "$control_api_build_receipt")
secrets_root=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
validate_workspace_invitation_secret "$secrets_root/workspace-invitation-hmac-v1"
workspace_invitation_hmac_digest=sha256:$(sha256_file "$secrets_root/workspace-invitation-hmac-v1")

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
  --arg backupMount "${BLAZN_BACKUP_MOUNT:-}" \
  --arg backupSource "${BLAZN_BACKUP_SOURCE:-}" \
  --arg backupFstype "${BLAZN_BACKUP_FSTYPE:-}" \
  '.schemaVersion == "blazn.dev/control-plane-ownership/v1" and .owner == "blazn-poc" and .host == $host and .paths == {data:$data,backup:$backup,secrets:$secrets} and (.backupMount == null or .backupMount == {target:$backupMount,source:$backupSource,fstype:$backupFstype})' \
  "$receipt" >/dev/null || die "control-plane ownership receipt does not match this deployment"

digest=sha256:$(control_plane_config_digest "$ROOT_DIR")
tmp=$receipt.tmp.$$
umask 077
jq --arg digest "$digest" \
  --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  --arg backupMount "$BLAZN_BACKUP_MOUNT" \
  --arg backupSource "$BLAZN_BACKUP_SOURCE" \
  --arg backupFstype "$BLAZN_BACKUP_FSTYPE" \
  --arg controlApiSource "$control_api_source" \
  --arg controlApiImage "$control_api_image" \
  --arg controlApiImageId "$control_api_image_id" \
  --arg workspaceInvitationHmacDigest "$workspace_invitation_hmac_digest" \
  '.configDigest=$digest | .configUpdatedAt=$updatedAt | .backupMount={target:$backupMount,source:$backupSource,fstype:$backupFstype} | .controlApi={sourceDigest:$controlApiSource,image:$controlApiImage,imageId:$controlApiImageId} | .secretDigests={"workspace-invitation-hmac-v1":$workspaceInvitationHmacDigest}' "$receipt" >"$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$receipt"
printf 'updated control-plane receipt config digest to %s\n' "$digest"
