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
BACKUP_ROOT=${BLAZN_BACKUP_ROOT:-}
DATA_ROOT=${BLAZN_DATA_ROOT:-/srv/frontro/blazn-poc/control-plane}
[ -n "$BACKUP_ROOT" ] || die "BLAZN_BACKUP_ROOT is required"
[ "$(filesystem_device "$BACKUP_ROOT")" != "$(filesystem_device "$DATA_ROOT")" ] || die "backup destination shares the data filesystem"

timestamp=$(date -u '+%Y%m%dT%H%M%SZ')
staging=$BACKUP_ROOT/.staging-$timestamp-$correlation
final=$BACKUP_ROOT/backup-$timestamp-$correlation
[ ! -e "$staging" ] && [ ! -e "$final" ] || die "backup destination already exists"
umask 077
mkdir -p -- "$staging/objects"

cleanup() {
  [ -z "${manifest_tmp:-}" ] || rm -f -- "$manifest_tmp"
  if [ -d "$staging" ]; then
    printf 'incomplete backup retained for inspection: %s\n' "$staging" >&2
  fi
}
trap cleanup EXIT HUP INT TERM

docker compose -f "$ROOT_DIR/compose.yaml" exec -T postgres \
  pg_dump --format=custom --no-owner --no-privileges -U "${POSTGRES_USER:-blazn}" "${POSTGRES_DB:-blazn}" \
  >"$staging/postgres.dump"

docker compose -f "$ROOT_DIR/compose.yaml" --profile tools run --rm \
  -v "$staging/objects:/backup" object-client \
  'access=$(cat /run/secrets/s3_access_key); secret=$(cat /run/secrets/s3_secret_key); mc alias set blazn http://object:9000 "$access" "$secret" >/dev/null; mc mirror --overwrite "blazn/$1" /backup' \
  -- "${S3_BUCKET:-blazn-poc}"

manifest_tmp=$staging.sha256.$$
(
  cd "$staging"
  find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum
) >"$manifest_tmp"
mv -- "$manifest_tmp" "$staging/SHA256SUMS"
printf '{"schemaVersion":"blazn.dev/control-plane-backup/v1","correlationId":"%s","fencingToken":%s,"createdAt":"%s","database":"%s","bucket":"%s"}\n' \
  "$correlation" "$BLAZN_FENCING_TOKEN" "$timestamp" "${POSTGRES_DB:-blazn}" "${S3_BUCKET:-blazn-poc}" >"$staging/metadata.json"
(
  cd "$staging"
  sha256sum metadata.json >>SHA256SUMS
  sha256sum -c SHA256SUMS >/dev/null
)
mv -- "$staging" "$final"
trap - EXIT HUP INT TERM
printf '%s\n' "$final"
