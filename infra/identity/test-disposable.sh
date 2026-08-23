#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ] || [ "$#" -ne 2 ]; then
  printf 'usage: sudo %s REVIEWED_ENV_FILE QUALIFICATION_DRIVER\n' "$0" >&2
  exit 64
fi
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
env_file=$1; driver=$2
"$script_dir/validate-environment.sh" "$env_file"
[ -x "$driver" ] || { printf 'an executable browser/email/MFA qualification driver is required\n' >&2; exit 69; }
# The reviewed disposable environment is explicit input.
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
[ "${BLAZN_IDENTITY_DISPOSABLE:-}" = true ] || { printf 'BLAZN_IDENTITY_DISPOSABLE=true is required\n' >&2; exit 77; }
case "$BLAZN_IDENTITY_DATA_ROOT:$BLAZN_IDENTITY_SECRETS_ROOT" in /tmp/blazn-identity-disposable.*:/tmp/blazn-identity-disposable.*) ;; *) printf 'disposable roots must use distinct /tmp/blazn-identity-disposable.* paths\n' >&2; exit 77 ;; esac
[ "$BLAZN_IDENTITY_DATA_ROOT" != "$BLAZN_IDENTITY_SECRETS_ROOT" ] || { printf 'disposable roots must differ\n' >&2; exit 77; }
receipt_output=${BLAZN_IDENTITY_QUALIFICATION_RECEIPT:?set a new absolute redacted receipt path}
case "$receipt_output" in /*) ;; *) printf 'qualification receipt path must be absolute\n' >&2; exit 77 ;; esac
[ ! -e "$receipt_output" ] && [ ! -L "$receipt_output" ] || { printf 'qualification receipt target must not exist\n' >&2; exit 73; }
case "$env_file:$receipt_output" in
  "$BLAZN_IDENTITY_DATA_ROOT"/*:*|"$BLAZN_IDENTITY_SECRETS_ROOT"/*:*|*:"$BLAZN_IDENTITY_DATA_ROOT"/*|*:"$BLAZN_IDENTITY_SECRETS_ROOT"/*)
    printf 'environment and receipt paths must remain outside disposable roots\n' >&2; exit 77 ;;
esac
receipt_dir=$(mktemp -d /tmp/blazn-identity-receipt.XXXXXX)
backup_dir=${receipt_dir}/backup
cleanup() {
  docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" down -v >/dev/null 2>&1 || true
  rm -rf -- "$BLAZN_IDENTITY_DATA_ROOT" "$BLAZN_IDENTITY_SECRETS_ROOT" "$receipt_dir"
}
trap cleanup EXIT HUP INT TERM

"$script_dir/generate-secrets.sh" "$BLAZN_IDENTITY_SECRETS_ROOT" "${ZITADEL_QUALIFICATION_ADMIN_EMAIL:?set qualification admin email}"
docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" up -d --wait
curl --fail --silent --show-error --proto '=https' --tlsv1.2 "https://${ZITADEL_DOMAIN}/.well-known/openid-configuration" >/dev/null
pat_before=$(docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/source,readonly "$ZITADEL_BACKUP_IMAGE" sh -ceu 'sha256sum /source/login-client.pat' | awk '{print $1}')
master_before=$(sha256sum "$BLAZN_IDENTITY_SECRETS_ROOT/zitadel-masterkey" | awk '{print $1}')
"$script_dir/backup.sh" "$env_file" "$backup_dir"
"$script_dir/restore.sh" "$backup_dir" "$env_file"
pat_after=$(docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/source,readonly "$ZITADEL_BACKUP_IMAGE" sh -ceu 'sha256sum /source/login-client.pat' | awk '{print $1}')
master_after=$(sha256sum "$BLAZN_IDENTITY_SECRETS_ROOT/zitadel-masterkey" | awk '{print $1}')
[ "$pat_before" = "$pat_after" ] && [ "$master_before" = "$master_after" ] || { printf 'PAT volume or master-key restore mismatch\n' >&2; exit 1; }
curl --fail --silent --show-error "${BLAZN_QUALIFICATION_API_URL:?set control API qualification URL}/healthz" | grep -F '"identityProvider":"ok"' >/dev/null
"$driver" --issuer "https://${ZITADEL_DOMAIN}" --api-url "$BLAZN_QUALIFICATION_API_URL" --backup-dir "$backup_dir" --receipt "$receipt_dir/driver.json"
node "$script_dir/verify-qualification.mjs" "$receipt_dir/driver.json"
install -o root -g root -m 600 "$receipt_dir/driver.json" "$receipt_output"
printf 'disposable identity qualification: ok (%s)\n' "$receipt_output"
