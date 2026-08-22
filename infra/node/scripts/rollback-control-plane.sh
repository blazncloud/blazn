#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
M2_ROOT=${BLAZN_NODE_ROLLBACK_M2_ROOT:-$(CDPATH='' cd -- "$SCRIPT_DIR/../../milestone-2" && pwd)}
# shellcheck disable=SC1091
. "$M2_ROOT/scripts/common.sh"

[ "$(id -u)" -eq 0 ] || die "Node infrastructure rollback must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Node infrastructure rollback must run through the control-plane lock"
for command_name in docker jq sha256sum sync; do require_command "$command_name"; done
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"

ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
BUILD_RECEIPT=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
UPGRADE_RECEIPT=${BLAZN_NODE_BROKER_UPGRADE_RECEIPT:-/var/lib/blazn/ownership/node-broker-upgrade.json}
CREATE_JOURNAL=${BLAZN_NODE_BROKER_CREATE_JOURNAL:-/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json}
NODE_ROOT=/etc/blazn/node-broker
PLAN_ROOT=/etc/blazn/node-plan
PLAN_CREATE_JOURNAL=/var/lib/blazn/ownership/node-plan-material-upgrade-create.json
RETAIN_PARENT=/var/lib/blazn/ownership
if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ]; then
  NODE_ROOT=${BLAZN_NODE_INFRA_TEST_NODE_ROOT:?test Node root is required}
  PLAN_ROOT=${BLAZN_NODE_INFRA_TEST_PLAN_ROOT:?test plan root is required}
  PLAN_CREATE_JOURNAL=${BLAZN_NODE_INFRA_TEST_PLAN_CREATE_JOURNAL:?test plan create journal is required}
  CREATE_JOURNAL=${BLAZN_NODE_INFRA_TEST_CREATE_JOURNAL:?test create journal is required}
  RETAIN_PARENT=${BLAZN_NODE_INFRA_TEST_RETAIN_PARENT:?test retention parent is required}
fi

assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
jq -e '.schemaVersion=="blazn.dev/node-broker-upgrade/v2" and .owner=="blazn-poc"' "$UPGRADE_RECEIPT" >/dev/null || die "Node upgrade receipt is invalid"
phase=$(jq -er .phase "$UPGRADE_RECEIPT")
case "$phase" in inputs-backed-up|role-ready|environment-bound|complete|rollback-started|role-removed|secrets-retained|environment-restored|build-restored|main-restored|source-restore-required) ;; rolled-back) printf 'Node broker prerequisite rollback is already complete\n'; exit 0 ;; *) die "rollback requires a receipted prerequisite phase or recovering Node upgrade" ;; esac

sync_path() { sync -f "$1"; }
write_phase() {
  next=$1; retained=$2; tmp=$UPGRADE_RECEIPT.tmp.$$
  jq --arg phase "$next" --arg retained "$retained" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase | .rollback.retainedPath=$retained | .updatedAt=$updatedAt' "$UPGRADE_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"
}
test_fault() { [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ] || return 0; [ "${BLAZN_NODE_ROLLBACK_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected rollback fault after $1"; }
restore_file() {
  backup=$1; expected=$2; target=$3
  [ "$expected" = "sha256:$(sha256_file "$backup")" ] || die "rollback backup digest changed: $backup"
  tmp=$target.tmp.$$; cp --preserve=mode,timestamps -- "$backup" "$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$target"; sync_path "$(dirname -- "$target")"
}

if [ "$phase" = complete ] || [ "$phase" = inputs-backed-up ] || [ "$phase" = role-ready ] || [ "$phase" = environment-bound ]; then
  correlation=${BLAZN_CORRELATION_ID:-manual}
  case "$correlation" in ''|*[!a-zA-Z0-9._-]*) die "rollback correlation ID is invalid" ;; esac
  retained=$RETAIN_PARENT/node-broker-rollback-$correlation
  [ ! -e "$retained" ] || die "rollback retention target already exists"
  tmp=$UPGRADE_RECEIPT.tmp.$$
  jq --arg retained "$retained" --arg startedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase="rollback-started" | .rollback={retainedPath:$retained,startedAt:$startedAt}' "$UPGRADE_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"; phase=rollback-started; test_fault rollback-started
else retained=$(jq -er .rollback.retainedPath "$UPGRADE_RECEIPT"); fi
case "$retained" in "$RETAIN_PARENT"/node-broker-rollback-*) ;; *) die "rollback retention target escaped its reviewed parent" ;; esac

complete_source_restore() {
  if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ]; then
    observed_source=${BLAZN_NODE_INFRA_TEST_OBSERVED_SOURCE_DIGEST:?test observed source digest is required}
    observed_config=${BLAZN_NODE_INFRA_TEST_OBSERVED_CONFIG_DIGEST:?test observed config digest is required}
  else
    observed_source=sha256:$(control_api_source_digest "$M2_ROOT")
    observed_config=sha256:$(control_plane_config_digest "$M2_ROOT")
  fi
  expected_source=$(jq -er .inputs.sourceDigest "$UPGRADE_RECEIPT")
  expected_config=$(jq -er .inputs.configDigest "$UPGRADE_RECEIPT")
  [ "$observed_source" = "$expected_source" ] || die "receipt-bound prior source restore is required before rollback can complete"
  [ "$observed_config" = "$expected_config" ] || die "receipt-bound prior config restore is required before rollback can complete"
  tmp=$UPGRADE_RECEIPT.tmp.$$
  jq --arg rolledBackAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase="rolled-back" | .rollback.rolledBackAt=$rolledBackAt' "$UPGRADE_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"; phase=rolled-back; test_fault rolled-back
}
if [ "$phase" = source-restore-required ]; then complete_source_restore; printf 'Node broker prerequisite rollback is complete; recovery evidence retained at %s\n' "$retained"; exit 0; fi

if [ -f "$BUILD_RECEIPT" ]; then CONTROL_API_IMAGE=$(jq -er .image "$BUILD_RECEIPT"); else CONTROL_API_IMAGE=blazn-control-api:rollback-placeholder; fi
export CONTROL_API_IMAGE BLAZN_NODE_BROKER_SECRETS_ROOT="$NODE_ROOT/secrets"
compose() { docker compose -f "$M2_ROOT/compose.yaml" --env-file "$ENV_FILE" "$@"; }
applied=$(compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from schema_migrations where version in ('004_nodes.sql','005_node_broker_security.sql','006_node_plan_signing_trust.sql','007_node_broker_connect.sql','008_node_broker_intents.sql')")
[ "$applied" = 0 ] || die "Node migrations are applied; automatic prerequisite rollback is forbidden"

if [ "$phase" = rollback-started ]; then
  role_count=$(compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from pg_roles where rolname='blazn_node_broker'")
  if [ "$role_count" = 1 ]; then
    cat <<'SQL' | compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null
BEGIN;
REASSIGN OWNED BY blazn_node_broker TO blazn_migration;
DROP OWNED BY blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON DATABASE blazn FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON TABLES FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON SEQUENCES FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM blazn_node_broker;
DO $revoke$ DECLARE database_row record; BEGIN FOR database_row IN SELECT datname FROM pg_database LOOP EXECUTE format('REVOKE ALL PRIVILEGES ON DATABASE %I FROM blazn_node_broker',database_row.datname); END LOOP; END $revoke$;
DROP ROLE blazn_node_broker;
COMMIT;
SQL
  elif [ "$role_count" != 0 ]; then die "could not determine broker role state"; fi
  write_phase role-removed "$retained"; phase=role-removed; test_fault role-removed
fi

if [ "$phase" = role-removed ]; then
  if [ -d "$NODE_ROOT" ]; then inventory=$NODE_ROOT/secrets; else inventory=$retained/secrets; fi
  for name in database-url enrollment-hmac-v1 join-credential-v1; do
    expected=$(jq -er --arg name "$name" '.nodeBroker.digests[$name]' "$UPGRADE_RECEIPT")
    [ "$expected" = "sha256:$(sha256_file "$inventory/$name")" ] || die "installed Node broker secret differs from rollback receipt: $name"
  done
  if [ -d "$NODE_ROOT" ] && [ ! -e "$retained" ]; then mv -- "$NODE_ROOT" "$retained"; sync_path "$RETAIN_PARENT"; elif [ -d "$retained" ] && [ ! -e "$NODE_ROOT" ]; then :; else die "secret retention transition is ambiguous"; fi
  if [ -f "$CREATE_JOURNAL" ]; then mv -- "$CREATE_JOURNAL" "$retained/secret-create-journal.json"; sync_path "$(dirname -- "$CREATE_JOURNAL")"; fi
  plan_retained=$retained/node-plan
  if [ -d "$PLAN_ROOT" ]; then
    [ "$(jq -er .nodePlan.publicKeyFingerprint "$UPGRADE_RECEIPT")" = "$(jq -er .publicKeyFingerprint "$PLAN_ROOT/signing-public-v1.json")" ] || die "installed Node plan fingerprint differs from rollback receipt"
    [ "$(jq -er .nodePlan.templateDigest "$UPGRADE_RECEIPT")" = "sha256:$(sha256_file "$PLAN_ROOT/node-install-plan-template-v1.json")" ] || die "installed Node plan template differs from rollback receipt"
    [ ! -e "$plan_retained" ] || die "Node plan retention target already exists"
    mv -- "$PLAN_ROOT" "$plan_retained"; sync_path "$(dirname -- "$PLAN_ROOT")"
  elif [ ! -d "$plan_retained" ]; then
    die "Node plan retention transition is ambiguous"
  fi
  if [ -f "$PLAN_CREATE_JOURNAL" ]; then mv -- "$PLAN_CREATE_JOURNAL" "$retained/plan-material-create-journal.json"; sync_path "$(dirname -- "$PLAN_CREATE_JOURNAL")"; fi
  write_phase secrets-retained "$retained"; phase=secrets-retained; test_fault secrets-retained
fi

if [ "$phase" = secrets-retained ]; then
  cp --preserve=mode,timestamps -- "$ENV_FILE" "$retained/control-plane.env.after"; chmod 0600 "$retained/control-plane.env.after"; sync_path "$retained/control-plane.env.after"
  restore_file "$(jq -er .inputs.environment.backupPath "$UPGRADE_RECEIPT")" "$(jq -er .inputs.environment.digest "$UPGRADE_RECEIPT")" "$ENV_FILE"
  write_phase environment-restored "$retained"; phase=environment-restored; test_fault environment-restored
fi
if [ "$phase" = environment-restored ]; then
  prior_present=$(jq -r .inputs.buildReceipt.present "$UPGRADE_RECEIPT")
  if [ -e "$BUILD_RECEIPT" ]; then cp --preserve=mode,timestamps -- "$BUILD_RECEIPT" "$retained/control-api-build.after.json"; chmod 0600 "$retained/control-api-build.after.json"; sync_path "$retained/control-api-build.after.json"; fi
  if [ "$prior_present" = true ]; then restore_file "$(jq -er .inputs.buildReceipt.backupPath "$UPGRADE_RECEIPT")" "$(jq -er .inputs.buildReceipt.digest "$UPGRADE_RECEIPT")" "$BUILD_RECEIPT"; elif [ -e "$BUILD_RECEIPT" ]; then mv -- "$BUILD_RECEIPT" "$retained/control-api-build.created.json"; sync_path "$(dirname -- "$BUILD_RECEIPT")"; fi
  write_phase build-restored "$retained"; phase=build-restored; test_fault build-restored
fi
if [ "$phase" = build-restored ]; then
  cp --preserve=mode,timestamps -- "$MAIN_RECEIPT" "$retained/control-plane.after.json"; chmod 0600 "$retained/control-plane.after.json"; sync_path "$retained/control-plane.after.json"
  restore_file "$(jq -er .inputs.mainReceipt.backupPath "$UPGRADE_RECEIPT")" "$(jq -er .inputs.mainReceipt.digest "$UPGRADE_RECEIPT")" "$MAIN_RECEIPT"
  write_phase main-restored "$retained"; phase=main-restored; test_fault main-restored
fi
if [ "$phase" = main-restored ]; then
  write_phase source-restore-required "$retained"; phase=source-restore-required; test_fault source-restore-required
fi
if [ "$phase" = source-restore-required ]; then
  complete_source_restore
fi
printf 'Node broker prerequisites rolled back; recovery evidence retained at %s\n' "$retained"
