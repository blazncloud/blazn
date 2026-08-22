#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "host preparation must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "host preparation must run through with-control-plane-lock.sh"
require_command openssl
require_command sha256sum
require_command jq

DATA_ROOT=${BLAZN_DATA_ROOT:-/srv/frontro/blazn-poc/control-plane}
BACKUP_ROOT=${BLAZN_BACKUP_ROOT:-}
SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
RECEIPT_PATH=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}

"$SCRIPT_DIR/preflight.sh" --plan >/dev/null
[ ! -e "$RECEIPT_PATH" ] || die "ownership receipt already exists: $RECEIPT_PATH"
[ ! -e "$DATA_ROOT" ] || die "data root already exists without an ownership receipt: $DATA_ROOT"
[ ! -e "$SECRETS_ROOT" ] || die "secrets root already exists without an ownership receipt: $SECRETS_ROOT"

umask 077
mkdir -p -- "$DATA_ROOT/postgres" "$DATA_ROOT/objects" "$SECRETS_ROOT" "$(dirname -- "$RECEIPT_PATH")"
chmod 0700 -- "$DATA_ROOT" "$DATA_ROOT/postgres" "$DATA_ROOT/objects" "$SECRETS_ROOT"
chown 999:999 -- "$DATA_ROOT/postgres"
chown 1000:1000 -- "$DATA_ROOT/objects"

postgres_password=$(openssl rand -hex 32)
s3_access_key=blazn$(openssl rand -hex 8)
s3_secret_key=$(openssl rand -hex 32)
printf '%s\n' "$postgres_password" >"$SECRETS_ROOT/postgres-password"
printf 'postgresql://%s:%s@postgres:5432/%s\n' \
  "${POSTGRES_USER:-blazn}" "$postgres_password" "${POSTGRES_DB:-blazn}" >"$SECRETS_ROOT/postgres-url"
printf '%s\n' "$s3_access_key" >"$SECRETS_ROOT/s3-access-key"
printf '%s\n' "$s3_secret_key" >"$SECRETS_ROOT/s3-secret-key"
chmod 0600 -- "$SECRETS_ROOT"/*

config_digest=$(sha256_file "$ROOT_DIR/compose.yaml")
host=$(hostname)
created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
receipt_tmp=$RECEIPT_PATH.tmp.$$
jq -cn \
  --arg host "$host" \
  --arg createdAt "$created_at" \
  --arg data "$DATA_ROOT" \
  --arg backup "$BACKUP_ROOT" \
  --arg secrets "$SECRETS_ROOT" \
  --arg postgresImage "$POSTGRES_IMAGE" \
  --arg minioImage "$MINIO_IMAGE" \
  --arg minioMcImage "$MINIO_MC_IMAGE" \
  --arg configDigest "sha256:$config_digest" \
  --argjson postgresPort "${POSTGRES_PORT:-55432}" \
  --argjson s3Port "${S3_PORT:-59000}" \
  --argjson s3ConsolePort "${S3_CONSOLE_PORT:-59001}" \
  --argjson apiPort "${API_PORT:-58080}" \
  '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host,createdAt:$createdAt,paths:{data:$data,backup:$backup,secrets:$secrets},ports:[$postgresPort,$s3Port,$s3ConsolePort,$apiPort],units:["blazn-control-plane.service"],images:[$postgresImage,$minioImage,$minioMcImage],configDigest:$configDigest}' \
  >"$receipt_tmp"
chmod 0600 "$receipt_tmp"
mv -- "$receipt_tmp" "$RECEIPT_PATH"
printf 'prepared Blazn control-plane paths; ownership receipt: %s\n' "$RECEIPT_PATH"
