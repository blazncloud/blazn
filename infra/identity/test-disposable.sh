#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ] || [ "$#" -ne 2 ]; then
  printf 'usage: sudo %s REVIEWED_ENV_FILE QUALIFICATION_DRIVER\n' "$0" >&2
  exit 64
fi
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$script_dir/lib.sh"
env_file=$1; driver=$2
"$script_dir/validate-environment.sh" "$env_file"
# The reviewed disposable environment is explicit input.
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
identity_validate_path "$driver" driver
case "$(stat -c '%u:%a:%h' -- "$driver")" in 0:500:1|0:700:1) ;; *) identity_fail 'qualification driver must be root-owned, singly linked, and mode 0500 or 0700' ;; esac
driver_digest=sha256:$(sha256sum "$driver" | awk '{print $1}')
expected_driver_digest=${ZITADEL_QUALIFICATION_DRIVER_SHA256:?set the independently reviewed driver digest}
printf '%s' "$expected_driver_digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || identity_fail 'reviewed driver digest is invalid'
[ "$driver_digest" = "$expected_driver_digest" ] || identity_fail 'qualification driver digest does not match independent review'
[ "${BLAZN_IDENTITY_DISPOSABLE:-}" = true ] || { printf 'BLAZN_IDENTITY_DISPOSABLE=true is required\n' >&2; exit 77; }
case "$BLAZN_IDENTITY_DATA_ROOT:$BLAZN_IDENTITY_SECRETS_ROOT" in /tmp/blazn-identity-disposable.*:/tmp/blazn-identity-disposable.*) ;; *) printf 'disposable roots must use distinct /tmp/blazn-identity-disposable.* paths\n' >&2; exit 77 ;; esac
[ "$BLAZN_IDENTITY_DATA_ROOT" != "$BLAZN_IDENTITY_SECRETS_ROOT" ] || { printf 'disposable roots must differ\n' >&2; exit 77; }
receipt_output=${BLAZN_IDENTITY_QUALIFICATION_RECEIPT:?set a new absolute redacted receipt path}
identity_validate_path "$receipt_output" receipt
[ ! -e "$receipt_output" ] && [ ! -L "$receipt_output" ] || { printf 'qualification receipt target must not exist\n' >&2; exit 73; }
case "$env_file:$receipt_output" in
  "$BLAZN_IDENTITY_DATA_ROOT"/*:*|"$BLAZN_IDENTITY_SECRETS_ROOT"/*:*|*:"$BLAZN_IDENTITY_DATA_ROOT"/*|*:"$BLAZN_IDENTITY_SECRETS_ROOT"/*)
    printf 'environment and receipt paths must remain outside disposable roots\n' >&2; exit 77 ;;
esac
identity_reject_overlap "$receipt_output" "$BLAZN_IDENTITY_DATA_ROOT"
identity_reject_overlap "$receipt_output" "$BLAZN_IDENTITY_SECRETS_ROOT"
receipt_dir=$(mktemp -d /tmp/blazn-identity-receipt.XXXXXX)
qualification_started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
backup_dir=${BLAZN_IDENTITY_DATA_ROOT%/data}/backup
recovery_dir=${BLAZN_IDENTITY_DATA_ROOT%/data}/recovery
identity_validate_path "$backup_dir" backup
identity_reject_overlap "$backup_dir" "$BLAZN_IDENTITY_DATA_ROOT"
identity_reject_overlap "$backup_dir" "$BLAZN_IDENTITY_SECRETS_ROOT"
cleanup() {
  docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" down -v >/dev/null 2>&1 || true
  rm -rf -- "$BLAZN_IDENTITY_DATA_ROOT" "$BLAZN_IDENTITY_SECRETS_ROOT" "$backup_dir" "$recovery_dir" "$receipt_dir"
}
trap cleanup EXIT HUP INT TERM

"$script_dir/generate-secrets.sh" "$BLAZN_IDENTITY_SECRETS_ROOT" "${ZITADEL_QUALIFICATION_ADMIN_EMAIL:?set qualification admin email}"
docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" up -d --wait
configured_images=$(docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" config --images | LC_ALL=C sort -u)
running_image_digests=$(
  for container_id in $(docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" ps -q); do docker inspect --format '{{.Image}}' "$container_id"; done | LC_ALL=C sort -u
)
[ "$(printf '%s\n' "$configured_images" | grep -c .)" -eq 4 ] && [ "$(printf '%s\n' "$running_image_digests" | grep -c .)" -eq 4 ] || identity_fail 'configured and running image evidence must contain exactly four unique identities'
curl --fail --silent --show-error --proto '=https' --tlsv1.2 "https://${ZITADEL_DOMAIN}/.well-known/openid-configuration" >/dev/null
pat_before=$(docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/source,readonly "$ZITADEL_BACKUP_IMAGE" sh -ceu 'sha256sum /source/login-client.pat' | awk '{print $1}')
master_before=$(sha256sum "$BLAZN_IDENTITY_SECRETS_ROOT/zitadel-masterkey" | awk '{print $1}')
"$script_dir/backup.sh" "$env_file" "$backup_dir"
backup_manifest_digest=sha256:$(sha256sum "$backup_dir/SHA256SUMS" | awk '{print $1}')
database_digest=sha256:$(sha256sum "$backup_dir/postgres.sql" | awk '{print $1}')
"$script_dir/restore.sh" "$backup_dir" "$env_file"
pre_restore_pat_snapshot_digest=$(cat "$recovery_dir/pre-restore-pat.sha256")
pat_after=$(docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/source,readonly "$ZITADEL_BACKUP_IMAGE" sh -ceu 'sha256sum /source/login-client.pat' | awk '{print $1}')
master_after=$(sha256sum "$BLAZN_IDENTITY_SECRETS_ROOT/zitadel-masterkey" | awk '{print $1}')
[ "$pat_before" = "$pat_after" ] && [ "$master_before" = "$master_after" ] || { printf 'PAT volume or master-key restore mismatch\n' >&2; exit 1; }
curl --fail --silent --show-error "${BLAZN_QUALIFICATION_API_URL:?set control API qualification URL}/healthz" | grep -F '"identityProvider":"ok"' >/dev/null
"$driver" --issuer "https://${ZITADEL_DOMAIN}" --api-url "$BLAZN_QUALIFICATION_API_URL" --backup-dir "$backup_dir" --evidence "$receipt_dir/driver.json"
identity_require_root_file "$receipt_dir/driver.json"
QUALIFICATION_ISSUER="https://${ZITADEL_DOMAIN}" \
QUALIFICATION_STARTED_AT="$qualification_started_at" \
QUALIFICATION_DRIVER_DIGEST="$driver_digest" \
QUALIFICATION_ENVIRONMENT_DIGEST="sha256:$(sha256sum "$env_file" | awk '{print $1}')" \
QUALIFICATION_CONFIGURED_IMAGES="$configured_images" \
QUALIFICATION_RUNNING_IMAGE_DIGESTS="$running_image_digests" \
QUALIFICATION_BACKUP_MANIFEST_DIGEST="$backup_manifest_digest" \
QUALIFICATION_DATABASE_DIGEST="$database_digest" \
QUALIFICATION_MASTER_BEFORE="sha256:$master_before" QUALIFICATION_MASTER_AFTER="sha256:$master_after" \
QUALIFICATION_PAT_BEFORE="sha256:$pat_before" QUALIFICATION_PAT_AFTER="sha256:$pat_after" \
QUALIFICATION_PRE_RESTORE_PAT_SNAPSHOT_DIGEST="$pre_restore_pat_snapshot_digest" \
node "$script_dir/compose-qualification.mjs" "$receipt_dir/driver.json" "$receipt_dir/final.json"
node "$script_dir/verify-qualification.mjs" "$receipt_dir/final.json"
install -d -o root -g root -m 700 "$(dirname -- "$receipt_output")"
install -o root -g root -m 600 "$receipt_dir/final.json" "$receipt_output"
printf 'disposable identity qualification: ok (%s)\n' "$receipt_output"
