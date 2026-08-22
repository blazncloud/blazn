#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "backup must run through with-control-plane-lock.sh"
[ "$#" -eq 1 ] || die "usage: backup.sh CORRELATION_ID"
correlation=$1
case "$correlation" in
  ''|*[!a-zA-Z0-9._-]*) die "correlation ID contains unsupported characters" ;;
esac

require_command docker
require_command sha256sum
require_command cmp
require_command sort
require_command jq
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
load_control_api_image "$ROOT_DIR"
SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
RECEIPT_PATH=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
validate_workspace_invitation_secret "$SECRETS_ROOT/workspace-invitation-hmac-v1"
assert_regular_file_owned_mode "$RECEIPT_PATH" 0 600
workspace_invitation_hmac_digest=sha256:$(sha256_file "$SECRETS_ROOT/workspace-invitation-hmac-v1")
control_api_build_receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
control_api_source=$(jq -er .sourceDigest "$control_api_build_receipt")
control_api_image=$(jq -er .image "$control_api_build_receipt")
control_api_image_id=$(jq -er .imageId "$control_api_build_receipt")
config_digest=sha256:$(control_plane_config_digest "$ROOT_DIR")
jq -e \
  --arg configDigest "$config_digest" \
  --arg sourceDigest "$control_api_source" \
  --arg image "$control_api_image" \
  --arg imageId "$control_api_image_id" \
  --arg secretDigest "$workspace_invitation_hmac_digest" \
  '.configDigest == $configDigest and .controlApi == {sourceDigest:$sourceDigest,image:$image,imageId:$imageId} and .secretDigests == {"workspace-invitation-hmac-v1":$secretDigest}' \
  "$RECEIPT_PATH" >/dev/null || die "ownership receipt does not bind the current API, migration, config, and workspace invitation key"
node_plan_journal=$(jq -er .nodePlan.creationJournal.path "$RECEIPT_PATH")
node_plan_live=$(BLAZN_NODE_PLAN_CREATE_JOURNAL="$node_plan_journal" "$ROOT_DIR/../node/scripts/plan-material-object.sh")
[ "$(printf '%s' "$node_plan_live" | jq -cS .)" = "$(jq -cS .nodePlan "$RECEIPT_PATH")" ] || die "ownership receipt does not bind the active Node plan fingerprint and template"
BACKUP_ROOT=${BLAZN_BACKUP_ROOT:-}
DATA_ROOT=${BLAZN_DATA_ROOT:-/srv/frontro/blazn-poc/control-plane}
[ -n "$BACKUP_ROOT" ] || die "BLAZN_BACKUP_ROOT is required"
require_absolute_path BLAZN_BACKUP_ROOT "$BACKUP_ROOT"
assert_not_symlink_chain "$BACKUP_ROOT"
assert_approved_backup_mount "$BACKUP_ROOT"
[ "$(filesystem_device "$BACKUP_ROOT")" != "$(filesystem_device "$DATA_ROOT")" ] || die "backup destination shares the data filesystem"

timestamp=$(date -u '+%Y%m%dT%H%M%SZ')
staging=$BACKUP_ROOT/.staging-$timestamp-$correlation
final=$BACKUP_ROOT/backup-$timestamp-$correlation
if [ -e "$staging" ] || [ -e "$final" ]; then
  die "backup destination already exists"
fi
umask 077
mkdir -p -- "$staging/objects"

cleanup() {
  [ -z "${manifest_tmp:-}" ] || rm -f -- "$manifest_tmp"
  if [ -d "$staging" ]; then
    printf 'incomplete backup retained for inspection: %s\n' "$staging" >&2
  fi
}
trap cleanup EXIT HUP INT TERM

capture_object_manifest() {
  destination=$1
  docker compose -f "$ROOT_DIR/compose.yaml" --profile tools run --rm -T object-client \
    'access=$(cat /run/secrets/s3_runtime_access_key); secret=$(cat /run/secrets/s3_runtime_secret_key); mc alias set blazn http://object:9000 "$access" "$secret" >/dev/null; mc ls --recursive --json "blazn/$1"' \
    -- "${S3_BUCKET:-blazn-poc}" | LC_ALL=C sort >"$destination"
}

# M2 has no database-to-object metadata yet. This stability barrier proves that
# the bucket did not change across the database dump and object export. A future
# object metadata schema must replace it with an application-level snapshot.
capture_object_manifest "$staging/objects.before.jsonl"

docker compose -f "$ROOT_DIR/compose.yaml" exec -T postgres \
  pg_dump --format=custom --no-owner --no-privileges -U blazn_migration "${POSTGRES_DB:-blazn}" \
  >"$staging/postgres.dump"

docker compose -f "$ROOT_DIR/compose.yaml" --profile tools run --rm \
  -v "$staging/objects:/backup" object-client \
  'access=$(cat /run/secrets/s3_runtime_access_key); secret=$(cat /run/secrets/s3_runtime_secret_key); mc alias set blazn http://object:9000 "$access" "$secret" >/dev/null; mc mirror --overwrite "blazn/$1" /backup' \
  -- "${S3_BUCKET:-blazn-poc}"

capture_object_manifest "$staging/objects.after.jsonl"
cmp -s "$staging/objects.before.jsonl" "$staging/objects.after.jsonl" || \
  die "object bucket changed during backup; incomplete staging evidence was retained"

manifest_tmp=$staging.sha256.$$
(
  cd "$staging"
  find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum
) >"$manifest_tmp"
mv -- "$manifest_tmp" "$staging/SHA256SUMS"
receipt=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
assert_regular_file_owned_mode "$receipt" 0 600
node_broker_receipt_digest=sha256:$(jq -cS .nodeBroker "$receipt" | sha256sum | awk '{print $1}')
node_plan_receipt_digest=sha256:$(jq -cS .nodePlan "$receipt" | sha256sum | awk '{print $1}')
jq -cn \
  --arg correlationId "$correlation" \
  --argjson fencingToken "$BLAZN_FENCING_TOKEN" \
  --arg createdAt "$timestamp" \
  --arg database "${POSTGRES_DB:-blazn}" \
  --arg bucket "${S3_BUCKET:-blazn-poc}" \
  --arg configDigest "$config_digest" \
  --arg sourceDigest "$control_api_source" \
  --arg image "$control_api_image" \
  --arg imageId "$control_api_image_id" \
  --arg secretDigest "$workspace_invitation_hmac_digest" \
  --arg nodeBrokerReceiptDigest "$node_broker_receipt_digest" \
  --arg nodePlanReceiptDigest "$node_plan_receipt_digest" \
  '{schemaVersion:"blazn.dev/control-plane-backup/v3",correlationId:$correlationId,fencingToken:$fencingToken,createdAt:$createdAt,database:$database,bucket:$bucket,configDigest:$configDigest,controlApi:{sourceDigest:$sourceDigest,image:$image,imageId:$imageId},secretDigests:{"workspace-invitation-hmac-v1":$secretDigest},nodeBrokerReceiptDigest:$nodeBrokerReceiptDigest,nodePlanReceiptDigest:$nodePlanReceiptDigest}' \
  >"$staging/metadata.json"
(
  cd "$staging"
  sha256sum metadata.json >>SHA256SUMS
  sha256sum -c SHA256SUMS >/dev/null
)
mv -- "$staging" "$final"
trap - EXIT HUP INT TERM
printf '%s\n' "$final"
