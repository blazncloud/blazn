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
NODE_SECRETS_ROOT=${BLAZN_NODE_BROKER_SECRETS_ROOT:-/etc/blazn/node-broker/secrets}

"$SCRIPT_DIR/preflight.sh" --plan >/dev/null
[ ! -e "$RECEIPT_PATH" ] || die "ownership receipt already exists: $RECEIPT_PATH"
[ ! -e "$DATA_ROOT" ] || die "data root already exists without an ownership receipt: $DATA_ROOT"
[ ! -e "$SECRETS_ROOT" ] || die "secrets root already exists without an ownership receipt: $SECRETS_ROOT"
[ ! -e /etc/blazn/node-broker ] || die "node broker secrets already exist without an ownership receipt"

umask 077
mkdir -p -- "$DATA_ROOT/postgres" "$DATA_ROOT/objects" "$SECRETS_ROOT" "$(dirname -- "$RECEIPT_PATH")"
chmod 0700 -- "$DATA_ROOT" "$DATA_ROOT/postgres" "$DATA_ROOT/objects" "$SECRETS_ROOT"
chown 999:999 -- "$DATA_ROOT/postgres"
chown 1000:1000 -- "$DATA_ROOT/objects"

postgres_password=$(openssl rand -hex 32)
migration_password=$(openssl rand -hex 32)
bootstrap_password=$(openssl rand -hex 32)
runtime_password=$(openssl rand -hex 32)
s3_root_access_key=blaznroot$(openssl rand -hex 8)
s3_root_secret_key=$(openssl rand -hex 32)
s3_runtime_access_key=blaznruntime$(openssl rand -hex 8)
s3_runtime_secret_key=$(openssl rand -hex 32)
proxy_auth_secret=$(openssl rand -hex 32)
initial_password=$(openssl rand -hex 24)
printf '%s\n' "$postgres_password" >"$SECRETS_ROOT/postgres-password"
printf 'postgresql://%s:%s@postgres:5432/%s\n' \
  blazn_migration "$migration_password" "${POSTGRES_DB:-blazn}" >"$SECRETS_ROOT/migration-database-url"
printf 'postgresql://%s:%s@postgres:5432/%s\n' \
  blazn_bootstrap "$bootstrap_password" "${POSTGRES_DB:-blazn}" >"$SECRETS_ROOT/bootstrap-database-url"
printf 'postgresql://%s:%s@postgres:5432/%s\n' \
  blazn_runtime "$runtime_password" "${POSTGRES_DB:-blazn}" >"$SECRETS_ROOT/runtime-database-url"
printf '%s\n' "$s3_root_access_key" >"$SECRETS_ROOT/s3-root-access-key"
printf '%s\n' "$s3_root_secret_key" >"$SECRETS_ROOT/s3-root-secret-key"
printf '%s\n' "$s3_runtime_access_key" >"$SECRETS_ROOT/s3-runtime-access-key"
printf '%s\n' "$s3_runtime_secret_key" >"$SECRETS_ROOT/s3-runtime-secret-key"
printf '%s\n' "$proxy_auth_secret" >"$SECRETS_ROOT/proxy-auth-secret"
printf '%s\n' "$initial_password" >"$SECRETS_ROOT/initial-password"
# The parent directory is root-only. Compose bind-mounts only the named files;
# mode 0444 lets explicitly configured non-root container users read them.
chmod 0444 -- "$SECRETS_ROOT"/*

BLAZN_NODE_BROKER_SECRETS_ROOT="$NODE_SECRETS_ROOT" \
  "$ROOT_DIR/../node/scripts/create-secrets.sh" >/dev/null

"$SCRIPT_DIR/build-control-api.sh" >/dev/null
CONTROL_API_BUILD_RECEIPT=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
control_api_source=$(jq -er .sourceDigest "$CONTROL_API_BUILD_RECEIPT")
control_api_image=$(jq -er .image "$CONTROL_API_BUILD_RECEIPT")
control_api_image_id=$(jq -er .imageId "$CONTROL_API_BUILD_RECEIPT")
node_database_digest=sha256:$(sha256_file "$NODE_SECRETS_ROOT/database-url")
node_enrollment_digest=sha256:$(sha256_file "$NODE_SECRETS_ROOT/enrollment-hmac-v1")
node_join_digest=sha256:$(sha256_file "$NODE_SECRETS_ROOT/join-credential-v1")

config_digest=$(control_plane_config_digest "$ROOT_DIR")
host=$(hostname)
created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
receipt_tmp=$RECEIPT_PATH.tmp.$$
jq -cn \
  --arg host "$host" \
  --arg createdAt "$created_at" \
  --arg data "$DATA_ROOT" \
  --arg backup "$BACKUP_ROOT" \
  --arg secrets "$SECRETS_ROOT" \
  --arg backupMount "$BLAZN_BACKUP_MOUNT" \
  --arg backupSource "$BLAZN_BACKUP_SOURCE" \
  --arg backupFstype "$BLAZN_BACKUP_FSTYPE" \
  --arg postgresImage "$POSTGRES_IMAGE" \
  --arg minioImage "$MINIO_IMAGE" \
  --arg minioMcImage "$MINIO_MC_IMAGE" \
  --arg configDigest "sha256:$config_digest" \
  --arg controlApiSource "$control_api_source" \
  --arg controlApiImage "$control_api_image" \
  --arg controlApiImageId "$control_api_image_id" \
  --arg nodeSecrets "$NODE_SECRETS_ROOT" \
  --arg nodeDatabaseDigest "$node_database_digest" \
  --arg nodeEnrollmentDigest "$node_enrollment_digest" \
  --arg nodeJoinDigest "$node_join_digest" \
  --argjson postgresPort "${POSTGRES_PORT:-55432}" \
  --argjson s3Port "${S3_PORT:-59000}" \
  --argjson s3ConsolePort "${S3_CONSOLE_PORT:-59001}" \
  --argjson apiPort "${API_PORT:-58080}" \
  '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host,createdAt:$createdAt,paths:{data:$data,backup:$backup,secrets:$secrets},backupMount:{target:$backupMount,source:$backupSource,fstype:$backupFstype},controlApi:{sourceDigest:$controlApiSource,image:$controlApiImage,imageId:$controlApiImageId},nodeBroker:{schemaVersion:"blazn.dev/node-broker-infra/v1",secretsRoot:$nodeSecrets,databaseRole:"blazn_node_broker",keyIds:{enrollment:"node-enrollment/v1",joinCredential:"node-join-credential/v1"},digests:{"database-url":$nodeDatabaseDigest,"enrollment-hmac-v1":$nodeEnrollmentDigest,"join-credential-v1":$nodeJoinDigest}},ports:[$postgresPort,$s3Port,$s3ConsolePort,$apiPort],units:["blazn-control-plane.service"],images:[$postgresImage,$minioImage,$minioMcImage],configDigest:$configDigest}' \
  >"$receipt_tmp"
chmod 0600 "$receipt_tmp"
mv -- "$receipt_tmp" "$RECEIPT_PATH"
printf 'prepared Blazn control-plane paths; ownership receipt: %s\n' "$RECEIPT_PATH"
