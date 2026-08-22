#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
M2_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/../../milestone-2" && pwd)
# shellcheck disable=SC1091
. "$M2_ROOT/scripts/common.sh"

[ "$(id -u)" -eq 0 ] || die "Node infrastructure upgrade must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Node infrastructure upgrade must run through the control-plane lock"
for command_name in docker jq openssl sha256sum sync; do require_command "$command_name"; done
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"

ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
BUILD_RECEIPT=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
UPGRADE_RECEIPT=${BLAZN_NODE_BROKER_UPGRADE_RECEIPT:-/var/lib/blazn/ownership/node-broker-upgrade.json}
BACKUP_ROOT=${BLAZN_NODE_BROKER_UPGRADE_BACKUP_ROOT:-/var/lib/blazn/ownership/node-broker-upgrade-inputs}
NODE_ROOT=/etc/blazn/node-broker
CREATE_JOURNAL=/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json
if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ]; then
  NODE_ROOT=${BLAZN_NODE_INFRA_TEST_NODE_ROOT:?test Node root is required}
  CREATE_JOURNAL=${BLAZN_NODE_INFRA_TEST_CREATE_JOURNAL:?test create journal is required}
fi
NODE_SECRETS=${BLAZN_NODE_BROKER_SECRETS_ROOT:-$NODE_ROOT/secrets}
DEFER_CONFIG=${BLAZN_NODE_UPGRADE_DEFER_CONFIG:-0}
case "$DEFER_CONFIG" in 0|1) ;; *) die "BLAZN_NODE_UPGRADE_DEFER_CONFIG must be 0 or 1" ;; esac
[ "$NODE_SECRETS" = "$NODE_ROOT/secrets" ] || die "Node broker secrets root differs from the reviewed path"
export BLAZN_NODE_BROKER_SECRETS_ROOT="$NODE_SECRETS"

for path in "$ENV_FILE" "$MAIN_RECEIPT"; do assert_regular_file_owned_mode "$path" 0 600; done
for path in "$UPGRADE_RECEIPT" "$BACKUP_ROOT" "$CREATE_JOURNAL"; do require_absolute_path node-infra-path "$path"; assert_not_symlink_chain "$path"; done
jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/control-plane-ownership/v1" and .owner=="blazn-poc" and .host==$host' "$MAIN_RECEIPT" >/dev/null || die "main ownership receipt does not belong to this host"

sha() { sha256_file "$1"; }
sync_path() { sync -f "$1"; }
node_object() {
  jq -cnS \
    --arg database "sha256:$(sha "$NODE_SECRETS/database-url")" \
    --arg enrollment "sha256:$(sha "$NODE_SECRETS/enrollment-hmac-v1")" \
    --arg join "sha256:$(sha "$NODE_SECRETS/join-credential-v1")" \
    --arg journal "/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json" --arg journalDigest "sha256:$(sha "$CREATE_JOURNAL")" \
    '{schemaVersion:"blazn.dev/node-broker-infra/v1",secretsRoot:"/etc/blazn/node-broker/secrets",databaseRole:"blazn_node_broker",keyIds:{enrollment:"node-enrollment/v1",joinCredential:"node-join-credential/v1"},digests:{"database-url":$database,"enrollment-hmac-v1":$enrollment,"join-credential-v1":$join},creationJournal:{path:$journal,digest:$journalDigest}}'
}
write_phase() {
  next=$1; tmp=$UPGRADE_RECEIPT.tmp.$$
  jq --arg phase "$next" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase | .updatedAt=$updatedAt' "$UPGRADE_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"
}
test_fault() { [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ] || return 0; [ "${BLAZN_NODE_INFRA_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected test fault after $1"; }
backup_input() {
  source=$1; destination=$2
  if [ ! -e "$destination" ]; then cp --preserve=mode,timestamps -- "$source" "$destination"; sync_path "$destination"; sync_path "$BACKUP_ROOT"; fi
  assert_regular_file_owned_mode "$destination" 0 600
  [ "$(sha "$destination")" = "$(sha "$source")" ] || die "unbound upgrade input backup differs from its source"
}
ensure_env_binding() {
  expected="BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets"
  count=$(grep -c '^BLAZN_NODE_BROKER_SECRETS_ROOT=' "$ENV_FILE" || true)
  case "$count" in
    0) tmp=$ENV_FILE.tmp.$$; { sed -n '1,$p' "$ENV_FILE"; printf '%s\n' "$expected"; } >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$ENV_FILE"; sync_path "$(dirname -- "$ENV_FILE")" ;;
    1) grep -Fx "$expected" "$ENV_FILE" >/dev/null || die "existing Node broker secrets-root environment binding conflicts" ;;
    *) die "duplicate Node broker secrets-root environment bindings" ;;
  esac
}

BLAZN_NODE_BROKER_SECRETS_ROOT="$NODE_SECRETS" \
BLAZN_NODE_BROKER_CREATE_JOURNAL="$CREATE_JOURNAL" \
  "$SCRIPT_DIR/create-secrets.sh" >/dev/null
test_fault secrets-published

if [ ! -e "$UPGRADE_RECEIPT" ]; then
  if [ ! -e "$BACKUP_ROOT" ]; then umask 077; mkdir -- "$BACKUP_ROOT"; chmod 0700 "$BACKUP_ROOT"; sync_path "$(dirname -- "$BACKUP_ROOT")"; fi
  assert_directory_owned_mode "$BACKUP_ROOT" 0 700
  test_fault input-root-created
  backup_input "$MAIN_RECEIPT" "$BACKUP_ROOT/control-plane.json"
  test_fault main-backed-up
  backup_input "$ENV_FILE" "$BACKUP_ROOT/control-plane.env"
  test_fault environment-backed-up
  build_present=false; build_digest=
  if [ -e "$BUILD_RECEIPT" ]; then backup_input "$BUILD_RECEIPT" "$BACKUP_ROOT/control-api-build.json"; build_present=true; build_digest=sha256:$(sha "$BUILD_RECEIPT"); fi
  test_fault build-backed-up
  node_json=$(node_object); tmp=$UPGRADE_RECEIPT.tmp.$$
  jq -cn \
    --arg host "$(hostname)" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg main "$MAIN_RECEIPT" --arg mainBackup "$BACKUP_ROOT/control-plane.json" --arg mainDigest "sha256:$(sha "$MAIN_RECEIPT")" \
    --arg env "$ENV_FILE" --arg envBackup "$BACKUP_ROOT/control-plane.env" --arg envDigest "sha256:$(sha "$ENV_FILE")" \
    --arg build "$BUILD_RECEIPT" --arg buildBackup "$BACKUP_ROOT/control-api-build.json" --arg buildDigest "$build_digest" \
    --argjson buildPresent "$build_present" --arg sourceDigest "$(jq -er '.controlApi.sourceDigest // ""' "$MAIN_RECEIPT")" \
    --arg configDigest "$(jq -er '.configDigest // ""' "$MAIN_RECEIPT")" --argjson nodeBroker "$node_json" \
    '{schemaVersion:"blazn.dev/node-broker-upgrade/v2",owner:"blazn-poc",host:$host,phase:"inputs-backed-up",createdAt:$createdAt,inputs:{mainReceipt:{path:$main,backupPath:$mainBackup,digest:$mainDigest},environment:{path:$env,backupPath:$envBackup,digest:$envDigest},buildReceipt:{path:$build,backupPath:$buildBackup,present:$buildPresent,digest:$buildDigest},sourceDigest:$sourceDigest,configDigest:$configDigest},nodeBroker:$nodeBroker}' >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"
fi

assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/node-broker-upgrade/v2" and .owner=="blazn-poc" and .host==$host' "$UPGRADE_RECEIPT" >/dev/null || die "upgrade receipt is invalid"
for pair in mainReceipt environment; do backup=$(jq -er --arg pair "$pair" '.inputs[$pair].backupPath' "$UPGRADE_RECEIPT"); expected=$(jq -er --arg pair "$pair" '.inputs[$pair].digest' "$UPGRADE_RECEIPT"); [ "$expected" = "sha256:$(sha "$backup")" ] || die "upgrade input backup changed: $pair"; done
[ "$(node_object)" = "$(jq -cS .nodeBroker "$UPGRADE_RECEIPT")" ] || die "installed Node broker secrets differ from upgrade receipt"
phase=$(jq -er .phase "$UPGRADE_RECEIPT")
case "$phase" in inputs-backed-up|role-ready|environment-bound|build-ready|complete) ;; rollback-*) die "upgrade is in rollback recovery" ;; rolled-back) die "upgrade was rolled back" ;; *) die "upgrade phase is invalid" ;; esac

if [ -f "$BUILD_RECEIPT" ]; then CONTROL_API_IMAGE=$(jq -er .image "$BUILD_RECEIPT"); else CONTROL_API_IMAGE=blazn-control-api:upgrade-placeholder; fi
export CONTROL_API_IMAGE
compose() { docker compose -f "$M2_ROOT/compose.yaml" --env-file "$ENV_FILE" "$@"; }

if [ "$phase" = inputs-backed-up ]; then
  postgres_container=$(compose ps -q postgres)
  [ -n "$postgres_container" ] || die "the exact Blazn PostgreSQL container is not running"
  [ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.State.Status}}' "$postgres_container")" = blazn-m2/postgres/running ] || die "the running PostgreSQL container is not the expected Compose service"
  url=$(sed -n '1p' "$NODE_SECRETS/database-url"); password=${url#*://*:}; password=${password%%@*}
  {
    printf 'BEGIN;\n'
    printf "DO \$block\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='blazn_node_broker') THEN EXECUTE 'CREATE ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS'; END IF; END \$block\$;\n"
    printf "DO \$preserve\$ DECLARE database_row record; role_row record; BEGIN FOR database_row IN SELECT oid,datname FROM pg_database WHERE datallowconn LOOP FOR role_row IN SELECT oid,rolname FROM pg_roles WHERE rolcanlogin AND rolname <> 'blazn_node_broker' AND has_database_privilege(oid,database_row.oid,'CONNECT') LOOP EXECUTE format('GRANT CONNECT ON DATABASE %%I TO %%I',database_row.datname,role_row.rolname); IF has_database_privilege(role_row.oid,database_row.oid,'TEMP') THEN EXECUTE format('GRANT TEMPORARY ON DATABASE %%I TO %%I',database_row.datname,role_row.rolname); END IF; END LOOP; EXECUTE format('REVOKE CONNECT, TEMPORARY ON DATABASE %%I FROM PUBLIC',database_row.datname); END LOOP; END \$preserve\$;\n"
    printf 'REVOKE ALL PRIVILEGES ON DATABASE "%s" FROM blazn_node_broker;\n' "${POSTGRES_DB:-blazn}"
    printf 'REVOKE ALL PRIVILEGES ON SCHEMA public FROM blazn_node_broker;\n'
    printf 'REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM blazn_node_broker;\n'
    printf 'REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM blazn_node_broker;\n'
    printf 'REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM blazn_node_broker;\n'
    printf 'ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON TABLES FROM blazn_node_broker;\n'
    printf 'ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON SEQUENCES FROM blazn_node_broker;\n'
    printf 'ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM blazn_node_broker;\n'
    printf 'ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;\n'
    printf 'REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC;\n'
    printf 'GRANT EXECUTE ON FUNCTION workspace_json_contains_secret_key(jsonb) TO blazn_runtime;\n'
    printf "ALTER ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '%s';\n" "$password"
    printf 'GRANT CONNECT ON DATABASE "%s" TO blazn_node_broker; GRANT USAGE ON SCHEMA public TO blazn_node_broker; COMMIT;\n' "${POSTGRES_DB:-blazn}"
  } | compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null
  compose run --rm -T node-migration-preflight >/dev/null
  write_phase role-ready; phase=role-ready; test_fault role-ready
fi

if [ "$phase" = role-ready ]; then ensure_env_binding; write_phase environment-bound; phase=environment-bound; test_fault environment-bound; fi
if [ "$phase" = environment-bound ] && [ "$DEFER_CONFIG" = 1 ]; then
  grep -Fx 'BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets' "$ENV_FILE" >/dev/null || die "environment binding is absent"
  printf 'Node broker prerequisites are environment-bound; config publication is deferred until after release promotion\n'
  exit 0
fi
if [ "$phase" = environment-bound ]; then
  if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" != 1 ]; then "$M2_ROOT/scripts/build-control-api.sh" >/dev/null; load_control_api_image "$M2_ROOT"; fi
  write_phase build-ready; phase=build-ready; test_fault build-ready
fi
if [ "$phase" = build-ready ]; then
  node_json=$(jq -cS .nodeBroker "$UPGRADE_RECEIPT"); tmp=$MAIN_RECEIPT.tmp.$$
  if [ -f "$BUILD_RECEIPT" ]; then
    jq --argjson nodeBroker "$node_json" --arg digest "sha256:$(control_plane_config_digest "$M2_ROOT")" \
      --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg source "$(jq -er .sourceDigest "$BUILD_RECEIPT")" \
      --arg image "$(jq -er .image "$BUILD_RECEIPT")" --arg imageId "$(jq -er .imageId "$BUILD_RECEIPT")" \
      '.nodeBroker=$nodeBroker | .configDigest=$digest | .configUpdatedAt=$updatedAt | .controlApi={sourceDigest:$source,image:$image,imageId:$imageId}' "$MAIN_RECEIPT" >"$tmp"
  else
    jq --argjson nodeBroker "$node_json" '.nodeBroker=$nodeBroker' "$MAIN_RECEIPT" >"$tmp"
  fi
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$MAIN_RECEIPT"; sync_path "$(dirname -- "$MAIN_RECEIPT")"
  write_phase complete; phase=complete; test_fault complete
fi

[ "$(jq -cS .nodeBroker "$MAIN_RECEIPT")" = "$(jq -cS .nodeBroker "$UPGRADE_RECEIPT")" ] || die "main receipt is not bound to Node broker prerequisites"
grep -Fx 'BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets' "$ENV_FILE" >/dev/null || die "environment binding is absent"
printf 'Node broker infrastructure upgrade is complete and config-reconciled\n'
