#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$#" -eq 1 ] || die "usage: verify-rollback-inventory.sh BACKUP_DIRECTORY"
[ "$(id -u)" -eq 0 ] || die "rollback inventory verification must run as root"
backup=$1
require_absolute_path BACKUP_DIRECTORY "$backup"
assert_not_symlink_chain "$backup"
[ -d "$backup" ] || die "backup directory does not exist"
require_command docker
require_command jq
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
(
  cd "$backup"
  sha256sum -c SHA256SUMS >/dev/null
)

SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
RECEIPT_PATH=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
validate_workspace_invitation_secret "$SECRETS_ROOT/workspace-invitation-hmac-v1"
assert_regular_file_owned_mode "$RECEIPT_PATH" 0 600
load_control_api_image "$ROOT_DIR"
secret_digest=sha256:$(sha256_file "$SECRETS_ROOT/workspace-invitation-hmac-v1")
source_digest=sha256:$(control_api_source_digest "$ROOT_DIR")
image_id=$(docker image inspect "$CONTROL_API_IMAGE" --format '{{.Id}}')
config_digest=sha256:$(control_plane_config_digest "$ROOT_DIR")
node_plan_receipt_digest=sha256:$(jq -cS .nodePlan "$RECEIPT_PATH" | sha256sum | awk '{print $1}')

jq -e \
  --arg secretDigest "$secret_digest" \
  --arg sourceDigest "$source_digest" \
  --arg image "$CONTROL_API_IMAGE" \
  --arg imageId "$image_id" \
  --arg configDigest "$config_digest" \
  --arg nodePlanReceiptDigest "$node_plan_receipt_digest" \
  '.schemaVersion == "blazn.dev/control-plane-backup/v2" and
   .configDigest == $configDigest and
   .controlApi == {sourceDigest:$sourceDigest,image:$image,imageId:$imageId} and
   .secretDigests == {"workspace-invitation-hmac-v1":$secretDigest} and
   .nodePlanReceiptDigest == $nodePlanReceiptDigest' \
  "$backup/metadata.json" >/dev/null || die "backup inventory does not match the staged rollback release and installed invitation key"
jq -e \
  --arg secretDigest "$secret_digest" \
  --arg sourceDigest "$source_digest" \
  --arg image "$CONTROL_API_IMAGE" \
  --arg imageId "$image_id" \
  --arg configDigest "$config_digest" \
  '.configDigest == $configDigest and
   .controlApi == {sourceDigest:$sourceDigest,image:$image,imageId:$imageId} and
   .secretDigests == {"workspace-invitation-hmac-v1":$secretDigest}' \
  "$RECEIPT_PATH" >/dev/null || die "ownership receipt does not match the staged rollback release and installed invitation key"

printf 'rollback inventory matches the staged release, image, migration source, config, and invitation key\n'
