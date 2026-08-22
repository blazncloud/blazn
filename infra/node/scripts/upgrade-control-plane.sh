#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
M2_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/../../milestone-2" && pwd)
# shellcheck disable=SC1091
. "$M2_ROOT/scripts/common.sh"

[ "$(id -u)" -eq 0 ] || die "Node infrastructure upgrade must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Node infrastructure upgrade must run through the control-plane lock"
require_command docker
require_command jq
require_command openssl
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"

ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
UPGRADE_RECEIPT=${BLAZN_NODE_BROKER_UPGRADE_RECEIPT:-/var/lib/blazn/ownership/node-broker-upgrade.json}
MAIN_BACKUP=${BLAZN_NODE_BROKER_MAIN_RECEIPT_BACKUP:-/var/lib/blazn/ownership/node-broker-main-receipt.before.json}
NODE_ROOT=/etc/blazn/node-broker
STAGE=/etc/blazn/node-broker-upgrade-staging
if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ]; then
  NODE_ROOT=${BLAZN_NODE_INFRA_TEST_NODE_ROOT:?test Node root is required}
  STAGE=${BLAZN_NODE_INFRA_TEST_STAGE:?test staging path is required}
fi
NODE_SECRETS=${BLAZN_NODE_BROKER_SECRETS_ROOT:-$NODE_ROOT/secrets}
STAGED_SECRETS=$STAGE/secrets

[ "$NODE_SECRETS" = "$NODE_ROOT/secrets" ] || die "Node broker secrets root differs from the reviewed path"
assert_regular_file_owned_mode "$MAIN_RECEIPT" 0 600
assert_regular_file_owned_mode "$ENV_FILE" 0 600
require_absolute_path BLAZN_NODE_BROKER_UPGRADE_RECEIPT "$UPGRADE_RECEIPT"
require_absolute_path BLAZN_NODE_BROKER_MAIN_RECEIPT_BACKUP "$MAIN_BACKUP"
assert_not_symlink_chain "$UPGRADE_RECEIPT"
assert_not_symlink_chain "$MAIN_BACKUP"
assert_not_symlink_chain "$STAGE"
jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/control-plane-ownership/v1" and .owner=="blazn-poc" and .host==$host' "$MAIN_RECEIPT" >/dev/null || die "main ownership receipt does not belong to this host"

sha() { sha256_file "$1"; }
node_object() {
  root=$1
  jq -cnS \
    --arg database "sha256:$(sha "$root/database-url")" \
    --arg enrollment "sha256:$(sha "$root/enrollment-hmac-v1")" \
    --arg join "sha256:$(sha "$root/join-credential-v1")" \
    '{schemaVersion:"blazn.dev/node-broker-infra/v1",secretsRoot:"/etc/blazn/node-broker/secrets",databaseRole:"blazn_node_broker",keyIds:{enrollment:"node-enrollment/v1",joinCredential:"node-join-credential/v1"},digests:{"database-url":$database,"enrollment-hmac-v1":$enrollment,"join-credential-v1":$join}}'
}
validate_secret_tree() {
  root=$1
  assert_directory_owned_mode "$root" 0 700
  assert_regular_file_owned_mode "$root/database-url" 0 444
  assert_regular_file_owned_mode "$root/enrollment-hmac-v1" 0 400
  assert_regular_file_owned_mode "$root/join-credential-v1" 0 400
  [ "$(wc -c <"$root/enrollment-hmac-v1" | tr -d ' ')" = 32 ] || die "enrollment HMAC key must be exactly 32 bytes"
  [ "$(wc -c <"$root/join-credential-v1" | tr -d ' ')" = 32 ] || die "join credential key must be exactly 32 bytes"
  value=$(sed -n '1p' "$root/database-url")
  case "$value" in postgresql://blazn_node_broker:????????????????????????????????????????????????????????????????@postgres:5432/blazn) ;; *) die "Node broker database URL is invalid" ;; esac
  password=${value#*://*:}; password=${password%%@*}
  case "$password" in *[!a-f0-9]*) die "Node broker password is invalid" ;; esac
}
write_phase() {
  phase=$1
  tmp=$UPGRADE_RECEIPT.tmp.$$
  jq --arg phase "$phase" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase | .updatedAt=$updatedAt' "$UPGRADE_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"
  mv -- "$tmp" "$UPGRADE_RECEIPT"
}
test_fault() {
  [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ] || return 0
  [ "${BLAZN_NODE_INFRA_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected test fault after $1"
}

if [ ! -e "$UPGRADE_RECEIPT" ]; then
  [ ! -e "$NODE_ROOT" ] || die "Node broker secrets exist without an upgrade receipt"
  if [ ! -e "$STAGE" ]; then
    umask 077
    mkdir -p -- "$STAGED_SECRETS"
    chmod 0700 "$STAGE" "$STAGED_SECRETS"
    password=$(openssl rand -hex 32)
    printf 'postgresql://blazn_node_broker:%s@postgres:5432/blazn\n' "$password" >"$STAGED_SECRETS/database-url"
    openssl rand 32 >"$STAGED_SECRETS/enrollment-hmac-v1"
    openssl rand 32 >"$STAGED_SECRETS/join-credential-v1"
    chmod 0444 "$STAGED_SECRETS/database-url"
    chmod 0400 "$STAGED_SECRETS/enrollment-hmac-v1" "$STAGED_SECRETS/join-credential-v1"
  fi
  assert_directory_owned_mode "$STAGE" 0 700
  validate_secret_tree "$STAGED_SECRETS"
  if [ ! -e "$MAIN_BACKUP" ]; then
    cp --preserve=mode,timestamps -- "$MAIN_RECEIPT" "$MAIN_BACKUP"
  fi
  assert_regular_file_owned_mode "$MAIN_BACKUP" 0 600
  [ "$(sha "$MAIN_BACKUP")" = "$(sha "$MAIN_RECEIPT")" ] || die "unbound main receipt backup differs from the current receipt"
  node_json=$(node_object "$STAGED_SECRETS")
  tmp=$UPGRADE_RECEIPT.tmp.$$
  jq -cn \
    --arg host "$(hostname)" \
    --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg main "$MAIN_RECEIPT" \
    --arg backup "$MAIN_BACKUP" \
    --arg backupDigest "sha256:$(sha "$MAIN_BACKUP")" \
    --argjson nodeBroker "$node_json" \
    '{schemaVersion:"blazn.dev/node-broker-upgrade/v1",owner:"blazn-poc",host:$host,phase:"staged",createdAt:$createdAt,mainReceipt:{path:$main,backupPath:$backup,backupDigest:$backupDigest},nodeBroker:$nodeBroker}' >"$tmp"
  chmod 0600 "$tmp"
  mv -- "$tmp" "$UPGRADE_RECEIPT"
fi

assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
phase=$(jq -er '.phase' "$UPGRADE_RECEIPT")
case "$phase" in staged|secrets-installed|role-ready|receipt-bound) ;; rolled-back) die "Node infrastructure upgrade was rolled back" ;; *) die "Node infrastructure upgrade receipt phase is invalid" ;; esac
[ "$(jq -er '.mainReceipt.backupDigest' "$UPGRADE_RECEIPT")" = "sha256:$(sha "$MAIN_BACKUP")" ] || die "main receipt rollback backup digest changed"

if [ "$phase" = staged ]; then
  if [ -d "$STAGE" ] && [ ! -e "$NODE_ROOT" ]; then
    assert_directory_owned_mode "$STAGE" 0 700
    validate_secret_tree "$STAGED_SECRETS"
    [ "$(node_object "$STAGED_SECRETS")" = "$(jq -cS '.nodeBroker' "$UPGRADE_RECEIPT")" ] || die "staged Node broker secrets do not match receipt"
    mv -- "$STAGE" "$NODE_ROOT"
  elif [ -d "$NODE_ROOT" ] && [ ! -e "$STAGE" ]; then
    validate_secret_tree "$NODE_SECRETS"
    [ "$(node_object "$NODE_SECRETS")" = "$(jq -cS '.nodeBroker' "$UPGRADE_RECEIPT")" ] || die "installed Node broker secrets do not match receipt"
  else
    die "Node broker staged-to-installed transition is ambiguous"
  fi
  write_phase secrets-installed
  phase=secrets-installed
  test_fault secrets-installed
fi

validate_secret_tree "$NODE_SECRETS"
[ "$(node_object "$NODE_SECRETS")" = "$(jq -cS '.nodeBroker' "$UPGRADE_RECEIPT")" ] || die "installed Node broker secrets do not match receipt"

if [ -f "${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}" ]; then
  load_control_api_image "$M2_ROOT"
else
  CONTROL_API_IMAGE=blazn-control-api:upgrade-placeholder
  export CONTROL_API_IMAGE
fi
compose() { docker compose -f "$M2_ROOT/compose.yaml" --env-file "$ENV_FILE" "$@"; }
postgres_container=$(compose ps -q postgres)
[ -n "$postgres_container" ] || die "the exact Blazn PostgreSQL container is not running"
[ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.State.Status}}' "$postgres_container")" = blazn-m2/postgres/running ] || die "the running PostgreSQL container is not the expected Compose service"

if [ "$phase" = secrets-installed ]; then
  url=$(sed -n '1p' "$NODE_SECRETS/database-url")
  password=${url#*://*:}; password=${password%%@*}
  role_count=$(compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from pg_roles where rolname='blazn_node_broker'")
  case "$role_count" in
    0) printf "CREATE ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '%s';\n" "$password" | compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null ;;
    1) [ "$(compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from pg_auth_members where member=(select oid from pg_roles where rolname='blazn_node_broker')")" = 0 ] || die "existing Node broker role has unreviewed memberships" ;;
    *) die "could not determine Node broker role state" ;;
  esac
  printf "ALTER ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '%s'; GRANT CONNECT ON DATABASE \"%s\" TO blazn_node_broker; GRANT USAGE ON SCHEMA public TO blazn_node_broker; REVOKE CREATE ON SCHEMA public FROM blazn_node_broker;\n" "$password" "${POSTGRES_DB:-blazn}" | compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null
  compose run --rm -T node-migration-preflight >/dev/null
  write_phase role-ready
  phase=role-ready
  test_fault role-ready
fi

if [ "$phase" = role-ready ]; then
  node_json=$(jq -cS '.nodeBroker' "$UPGRADE_RECEIPT")
  tmp=$MAIN_RECEIPT.tmp.$$
  jq --argjson nodeBroker "$node_json" '.nodeBroker=$nodeBroker' "$MAIN_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"
  mv -- "$tmp" "$MAIN_RECEIPT"
  write_phase receipt-bound
  phase=receipt-bound
  test_fault receipt-bound
fi

[ "$(jq -cS '.nodeBroker' "$MAIN_RECEIPT")" = "$(jq -cS '.nodeBroker' "$UPGRADE_RECEIPT")" ] || die "main receipt is not bound to Node broker prerequisites"
printf 'Node broker infrastructure upgrade is receipt-bound; reconcile the control-plane config digest before startup\n'
