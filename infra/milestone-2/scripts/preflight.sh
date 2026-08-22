#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

MODE=plan
case ${1:-} in
  '') ;;
  --plan) MODE=plan ;;
  --deploy) MODE=deploy ;;
  *) die "usage: preflight.sh [--plan|--deploy]" ;;
esac

DATA_ROOT=${BLAZN_DATA_ROOT:-/srv/frontro/blazn-poc/control-plane}
BACKUP_ROOT=${BLAZN_BACKUP_ROOT:-}
SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
RECEIPT_PATH=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
BIND_ADDRESS=${BLAZN_BIND_ADDRESS:-127.0.0.1}
MIN_DATA_BYTES=${BLAZN_MIN_DATA_BYTES:-42949672960}
MIN_BACKUP_BYTES=${BLAZN_MIN_BACKUP_BYTES:-21474836480}
MIN_FREE_INODES=${BLAZN_MIN_FREE_INODES:-100000}

[ -n "$BACKUP_ROOT" ] || die "BLAZN_BACKUP_ROOT must identify a separate backup destination"
[ "$BIND_ADDRESS" = 127.0.0.1 ] || die "BLAZN_BIND_ADDRESS must remain 127.0.0.1 for the POC"

for named_path in \
  "BLAZN_DATA_ROOT:$DATA_ROOT" \
  "BLAZN_BACKUP_ROOT:$BACKUP_ROOT" \
  "BLAZN_SECRETS_ROOT:$SECRETS_ROOT" \
  "BLAZN_RECEIPT_PATH:$RECEIPT_PATH"; do
  name=${named_path%%:*}
  value=${named_path#*:}
  require_absolute_path "$name" "$value"
  assert_not_symlink_chain "$value"
done

case "$DATA_ROOT" in
  /srv/frontro/blazn-poc/control-plane|/srv/frontro/blazn-poc/control-plane/*) ;;
  *) die "BLAZN_DATA_ROOT is outside the reviewed /srv/frontro/blazn-poc/control-plane boundary" ;;
esac

[ "$DATA_ROOT" != "$BACKUP_ROOT" ] || die "data and backup roots must differ"
case "$BACKUP_ROOT/" in
  "$DATA_ROOT/"*) die "backup root must not be inside the data root" ;;
esac
case "$DATA_ROOT/" in
  "$BACKUP_ROOT/"*) die "data root must not be inside the backup root" ;;
esac

for number in "$MIN_DATA_BYTES" "$MIN_BACKUP_BYTES" "$MIN_FREE_INODES"; do
  is_uint "$number" || die "capacity thresholds must be unsigned integers"
done

data_device=$(filesystem_device "$DATA_ROOT")
backup_device=$(filesystem_device "$BACKUP_ROOT")
[ "$data_device" != "$backup_device" ] || die "backup and data roots resolve to the same filesystem; this cannot satisfy the isolated-backup gate"

data_bytes=$(available_bytes "$DATA_ROOT")
backup_bytes=$(available_bytes "$BACKUP_ROOT")
data_inodes=$(available_inodes "$DATA_ROOT")
backup_inodes=$(available_inodes "$BACKUP_ROOT")
[ "$data_bytes" -ge "$MIN_DATA_BYTES" ] || die "data filesystem has $data_bytes bytes free; require $MIN_DATA_BYTES"
[ "$backup_bytes" -ge "$MIN_BACKUP_BYTES" ] || die "backup filesystem has $backup_bytes bytes free; require $MIN_BACKUP_BYTES"
[ "$data_inodes" -ge "$MIN_FREE_INODES" ] || die "data filesystem has $data_inodes inodes free; require $MIN_FREE_INODES"
[ "$backup_inodes" -ge "$MIN_FREE_INODES" ] || die "backup filesystem has $backup_inodes inodes free; require $MIN_FREE_INODES"

ports="${POSTGRES_PORT:-55432} ${S3_PORT:-59000} ${S3_CONSOLE_PORT:-59001} ${API_PORT:-58080}"
seen=' '
for port in $ports; do
  is_uint "$port" || die "invalid port: $port"
  [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || die "port is out of range: $port"
  case "$seen" in
    *" $port "*) die "duplicate port in control-plane plan: $port" ;;
  esac
  seen="$seen$port "
done

if command -v ss >/dev/null 2>&1; then
  listeners=$(ss -H -ltn 2>/dev/null || true)
  for port in $ports; do
    if printf '%s\n' "$listeners" | awk -v port="$port" '$4 ~ (":" port "$") { found=1 } END { exit !found }'; then
      die "TCP port is already in use: $port"
    fi
  done
elif [ "$MODE" = deploy ]; then
  die "required command is unavailable: ss"
fi

if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files --no-legend 2>/dev/null | awk '{print $1}' | grep -Fx 'blazn-control-plane.service' >/dev/null 2>&1; then
  [ -f "$RECEIPT_PATH" ] || die "blazn-control-plane.service exists without the ownership receipt"
fi

for image in "${POSTGRES_IMAGE:-}" "${MINIO_IMAGE:-}" "${MINIO_MC_IMAGE:-}"; do
  case "$image" in
    *@sha256:????????????????????????????????????????????????????????????????) ;;
    *) die "all infrastructure images must use an immutable sha256 digest" ;;
  esac
done

if [ "$MODE" = deploy ]; then
  [ "$(id -u)" -eq 0 ] || die "deploy preflight must run as root"
  require_command docker
  require_command jq
  require_command sha256sum
  [ "${PUBLIC_URL:-}" = https://blazn.benpelo.com ] || die "PUBLIC_URL must be https://blazn.benpelo.com for the live POC deployment"
  [ -n "${BLAZN_INITIAL_LOGIN:-}" ] || die "BLAZN_INITIAL_LOGIN is required"
  [ "${BLAZN_INITIAL_LOGIN:-}" != admin@example.invalid ] || die "the placeholder BLAZN_INITIAL_LOGIN is forbidden for deployment"
  [ -n "${BLAZN_INITIAL_DISPLAY_NAME:-}" ] || die "BLAZN_INITIAL_DISPLAY_NAME is required"
  DOCKER_CONFIG=${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli} docker compose version >/dev/null 2>&1 || die "Blazn-owned Docker Compose v2 is unavailable"
  require_command flock
  [ -d "$DATA_ROOT/postgres" ] || die "prepared PostgreSQL directory is missing"
  [ -d "$DATA_ROOT/objects" ] || die "prepared object directory is missing"
  [ -d "$SECRETS_ROOT" ] || die "prepared secrets directory is missing"
  [ -f "$RECEIPT_PATH" ] || die "ownership receipt is missing"
  [ "$(stat -c '%u' "$RECEIPT_PATH")" -eq 0 ] || die "ownership receipt must be owned by root"
  [ "$(stat -c '%a' "$RECEIPT_PATH")" = 600 ] || die "ownership receipt must have mode 0600"
  config_digest=sha256:$(control_plane_config_digest "$ROOT_DIR")
  jq -e \
    --arg host "$(hostname)" \
    --arg data "$DATA_ROOT" \
    --arg backup "$BACKUP_ROOT" \
    --arg secrets "$SECRETS_ROOT" \
    --arg configDigest "$config_digest" \
    --arg postgresImage "$POSTGRES_IMAGE" \
    --arg minioImage "$MINIO_IMAGE" \
    --arg minioMcImage "$MINIO_MC_IMAGE" \
    --argjson postgresPort "${POSTGRES_PORT:-55432}" \
    --argjson s3Port "${S3_PORT:-59000}" \
    --argjson s3ConsolePort "${S3_CONSOLE_PORT:-59001}" \
    --argjson apiPort "${API_PORT:-58080}" \
    '.schemaVersion == "blazn.dev/control-plane-ownership/v1" and
     .owner == "blazn-poc" and .host == $host and
     .paths == {data:$data,backup:$backup,secrets:$secrets} and
     .ports == [$postgresPort,$s3Port,$s3ConsolePort,$apiPort] and
     .images == [$postgresImage,$minioImage,$minioMcImage] and
     .configDigest == $configDigest' \
    "$RECEIPT_PATH" >/dev/null || die "ownership receipt does not match the requested deployment"
  for secret in postgres-password migration-database-url runtime-database-url initial-password s3-access-key s3-secret-key; do
    [ -f "$SECRETS_ROOT/$secret" ] || die "required secret file is missing: $secret"
    [ "$(stat -c '%u' "$SECRETS_ROOT/$secret")" -eq 0 ] || die "secret file must be owned by root: $secret"
    mode=$(stat -c '%a' "$SECRETS_ROOT/$secret")
    [ "$mode" = 444 ] || die "secret file must have mode 0444 inside the root-only secrets directory: $secret"
  done
fi

printf '{"status":"ok","mode":"%s","bindAddress":"%s","ports":[%s,%s,%s,%s],"dataBytesFree":%s,"backupBytesFree":%s,"dataInodesFree":%s,"backupInodesFree":%s,"separateFilesystem":true}\n' \
  "$MODE" "$BIND_ADDRESS" \
  "${POSTGRES_PORT:-55432}" "${S3_PORT:-59000}" "${S3_CONSOLE_PORT:-59001}" "${API_PORT:-58080}" \
  "$data_bytes" "$backup_bytes" "$data_inodes" "$backup_inodes"
