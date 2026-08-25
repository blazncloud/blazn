#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
M2_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/../../milestone-2" && pwd)
# shellcheck disable=SC1091
. "$M2_ROOT/scripts/common.sh"

RETRY_CORRELATION=
case "$#" in
  0) ;;
  2)
    [ "$1" = --retry-after-rollback ] || die "usage: upgrade-control-plane.sh [--retry-after-rollback CORRELATION_ID]"
    RETRY_CORRELATION=$2
    ;;
  *) die "usage: upgrade-control-plane.sh [--retry-after-rollback CORRELATION_ID]" ;;
esac

[ "$(id -u)" -eq 0 ] || die "Node infrastructure upgrade must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Node infrastructure upgrade must run through the control-plane lock"
for command_name in docker find jq openssl sha256sum sort stat sync; do require_command "$command_name"; done
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"

ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
BUILD_RECEIPT=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
UPGRADE_RECEIPT=${BLAZN_NODE_BROKER_UPGRADE_RECEIPT:-/var/lib/blazn/ownership/node-broker-upgrade.json}
BACKUP_ROOT=${BLAZN_NODE_BROKER_UPGRADE_BACKUP_ROOT:-/var/lib/blazn/ownership/node-broker-upgrade-inputs}
RETRY_HISTORY_ROOT=${BLAZN_NODE_BROKER_UPGRADE_RETRY_ROOT:-$(dirname -- "$UPGRADE_RECEIPT")/node-broker-upgrade-retries}
OWNERSHIP_ROOT=$(dirname -- "$UPGRADE_RECEIPT")
[ "$(dirname -- "$BACKUP_ROOT")" = "$OWNERSHIP_ROOT" ] || die "upgrade backup root must share the receipt ownership directory"
[ "$(dirname -- "$RETRY_HISTORY_ROOT")" = "$OWNERSHIP_ROOT" ] || die "upgrade retry history must share the receipt ownership directory"
NODE_ROOT=/etc/blazn/node-broker
CREATE_JOURNAL=/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json
PLAN_ROOT=/etc/blazn/node-plan
PLAN_CREATE_JOURNAL=/var/lib/blazn/ownership/node-plan-material-upgrade-create.json
if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ]; then
  NODE_ROOT=${BLAZN_NODE_INFRA_TEST_NODE_ROOT:?test Node root is required}
  CREATE_JOURNAL=${BLAZN_NODE_INFRA_TEST_CREATE_JOURNAL:?test create journal is required}
  PLAN_ROOT=${BLAZN_NODE_INFRA_TEST_PLAN_ROOT:?test plan root is required}
  PLAN_CREATE_JOURNAL=${BLAZN_NODE_INFRA_TEST_PLAN_CREATE_JOURNAL:?test plan create journal is required}
fi
NODE_SECRETS=${BLAZN_NODE_BROKER_SECRETS_ROOT:-$NODE_ROOT/secrets}
DEFER_CONFIG=${BLAZN_NODE_UPGRADE_DEFER_CONFIG:-0}
case "$DEFER_CONFIG" in 0|1) ;; *) die "BLAZN_NODE_UPGRADE_DEFER_CONFIG must be 0 or 1" ;; esac
[ "$NODE_SECRETS" = "$NODE_ROOT/secrets" ] || die "Node broker secrets root differs from the reviewed path"
export BLAZN_NODE_BROKER_SECRETS_ROOT="$NODE_SECRETS"
export BLAZN_NODE_PLAN_ROOT="$PLAN_ROOT" BLAZN_NODE_PLAN_CREATE_JOURNAL="$PLAN_CREATE_JOURNAL"

for path in "$ENV_FILE" "$MAIN_RECEIPT"; do assert_regular_file_owned_mode "$path" 0 600; done
for path in "$UPGRADE_RECEIPT" "$BACKUP_ROOT" "$RETRY_HISTORY_ROOT" "$CREATE_JOURNAL"; do require_absolute_path node-infra-path "$path"; assert_not_symlink_chain "$path"; done
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
plan_object() {
  BLAZN_NODE_PLAN_TEST_MODE=${BLAZN_NODE_PLAN_TEST_MODE:-${BLAZN_NODE_INFRA_TEST_MODE:-0}} \
    "$SCRIPT_DIR/plan-material-object.sh"
}
write_phase() {
  next=$1; tmp=$UPGRADE_RECEIPT.tmp.$$
  jq --arg phase "$next" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase | .updatedAt=$updatedAt' "$UPGRADE_RECEIPT" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"
}
test_fault() { [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ] || return 0; [ "${BLAZN_NODE_INFRA_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected test fault after $1"; }
evidence_tree_digest() {
  evidence_root=$1
  require_absolute_path rollback-evidence-path "$evidence_root"; assert_not_symlink_chain "$evidence_root"; assert_directory_owned_mode "$evidence_root" 0 700
  unexpected=$(find "$evidence_root" -xdev ! -type d ! -type f -print)
  [ -z "$unexpected" ] || die "rollback evidence contains an unsupported filesystem object"
  [ "$(find "$evidence_root" -xdev -type f -print | awk 'END {print NR}')" -gt 0 ] || die "rollback evidence is empty"
  find "$evidence_root" -xdev -type f -exec sha256sum {} + | LC_ALL=C sort | sha256sum | awk '{print "sha256:" $1}'
}
write_retry_phase() {
  retry_journal=$1; next=$2; tmp=$retry_journal.tmp.$$
  jq --arg phase "$next" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase | .updatedAt=$updatedAt' "$retry_journal" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$retry_journal"; sync_path "$RETRY_HISTORY_ROOT"
}
prepare_post_rollback_retry() {
  correlation=$1
  case "$correlation" in ''|*[!a-zA-Z0-9._-]*) die "post-rollback retry correlation ID is invalid" ;; esac
  case "$correlation" in [a-zA-Z0-9]*) ;; *) die "post-rollback retry correlation ID is invalid" ;; esac
  [ "${#correlation}" -le 64 ] || die "post-rollback retry correlation ID is too long"
  [ "${BLAZN_CORRELATION_ID:-}" = "$correlation" ] || die "post-rollback retry correlation differs from the control-plane lock"
  [ "${BLAZN_MUTATION_PURPOSE:-}" = node-prereqs-retry ] || die "post-rollback retry requires the node-prereqs-retry control-plane lock purpose"
  if [ ! -e "$RETRY_HISTORY_ROOT" ]; then umask 077; mkdir -- "$RETRY_HISTORY_ROOT"; chmod 0700 "$RETRY_HISTORY_ROOT"; sync_path "$(dirname -- "$RETRY_HISTORY_ROOT")"; fi
  assert_directory_owned_mode "$RETRY_HISTORY_ROOT" 0 700
  ownership_device=$(stat -c %d "$OWNERSHIP_ROOT"); retry_device=$(stat -c %d "$RETRY_HISTORY_ROOT")
  [ "$ownership_device" = "$retry_device" ] || die "upgrade retry history must share the ownership filesystem"
  retry_journal=$RETRY_HISTORY_ROOT/$correlation.json
  retry_archive=$RETRY_HISTORY_ROOT/$correlation
  for path in "$retry_journal" "$retry_archive"; do assert_not_symlink_chain "$path"; done

  if [ ! -e "$retry_journal" ]; then
    assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
    jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/node-broker-upgrade/v2" and .owner=="blazn-poc" and .host==$host and .phase=="rolled-back" and (.rollback.rolledBackAt|type=="string")' "$UPGRADE_RECEIPT" >/dev/null || die "post-rollback retry requires the verified rolled-back receipt"
    assert_directory_owned_mode "$BACKUP_ROOT" 0 700
    jq -e --arg main "$BACKUP_ROOT/control-plane.json" --arg environment "$BACKUP_ROOT/control-plane.env" --arg build "$BACKUP_ROOT/control-api-build.json" \
      '.inputs.mainReceipt.backupPath==$main and .inputs.environment.backupPath==$environment and .inputs.buildReceipt.backupPath==$build' "$UPGRADE_RECEIPT" >/dev/null || die "rolled-back receipt has unexpected backup paths"
    [ "$(stat -c %d "$BACKUP_ROOT")" = "$ownership_device" ] || die "upgrade input backups must share the ownership filesystem"
    retained=$(jq -er '.rollback.retainedPath' "$UPGRADE_RECEIPT"); retained_digest=$(evidence_tree_digest "$retained")
    if [ -e "$NODE_ROOT" ] || [ -e "$PLAN_ROOT" ] || [ -e "$CREATE_JOURNAL" ] || [ -e "$PLAN_CREATE_JOURNAL" ]; then
      die "post-rollback retry found unretained Node prerequisite state"
    fi
    for pair in mainReceipt environment; do
      backup=$(jq -er --arg pair "$pair" '.inputs[$pair].backupPath' "$UPGRADE_RECEIPT")
      expected=$(jq -er --arg pair "$pair" '.inputs[$pair].digest' "$UPGRADE_RECEIPT")
      target=$(jq -er --arg pair "$pair" '.inputs[$pair].path' "$UPGRADE_RECEIPT")
      [ "$expected" = "sha256:$(sha "$backup")" ] || die "rolled-back upgrade input backup changed: $pair"
      [ "$expected" = "sha256:$(sha "$target")" ] || die "rolled-back upgrade input was not restored: $pair"
    done
    # jq -e treats the valid JSON boolean false as a failed command. Require the
    # JSON type explicitly, then convert the boolean to shell text without -e.
    build_present=$(jq -r '.inputs.buildReceipt.present | if type == "boolean" then tostring else error("rolled-back build receipt presence must be boolean") end' "$UPGRADE_RECEIPT")
    case "$build_present" in
      true)
        build_backup=$(jq -er '.inputs.buildReceipt.backupPath' "$UPGRADE_RECEIPT"); build_target=$(jq -er '.inputs.buildReceipt.path' "$UPGRADE_RECEIPT"); build_expected=$(jq -er '.inputs.buildReceipt.digest' "$UPGRADE_RECEIPT")
        if [ "$build_expected" != "sha256:$(sha "$build_backup")" ] || [ "$build_expected" != "sha256:$(sha "$build_target")" ]; then
          die "rolled-back build receipt was not restored"
        fi
        ;;
      false) [ ! -e "$(jq -er '.inputs.buildReceipt.path' "$UPGRADE_RECEIPT")" ] || die "rolled-back build receipt presence differs from its backup" ;;
      *) die "rolled-back build receipt presence is invalid" ;;
    esac
    previous_digest=sha256:$(sha "$UPGRADE_RECEIPT")
    tmp=$RETRY_HISTORY_ROOT/.$correlation.json.tmp.$$
    jq -cn --arg host "$(hostname)" --arg correlationId "$correlation" --arg startedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
      --arg receiptSource "$UPGRADE_RECEIPT" --arg receiptRetained "$retry_archive/receipt.json" --arg receiptDigest "$previous_digest" \
      --arg inputsSource "$BACKUP_ROOT" --arg inputsRetained "$retry_archive/inputs" --arg rollbackEvidence "$retained" --arg rollbackEvidenceDigest "$retained_digest" \
      '{schemaVersion:"blazn.dev/node-broker-upgrade-retry/v1",owner:"blazn-poc",host:$host,correlationId:$correlationId,phase:"prepared",startedAt:$startedAt,previousReceipt:{sourcePath:$receiptSource,retainedPath:$receiptRetained,digest:$receiptDigest},previousInputs:{sourcePath:$inputsSource,retainedPath:$inputsRetained},rollbackEvidencePath:$rollbackEvidence,rollbackEvidenceDigest:$rollbackEvidenceDigest}' >"$tmp"
    chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$retry_journal"; sync_path "$RETRY_HISTORY_ROOT"
  fi

  assert_regular_file_owned_mode "$retry_journal" 0 600
  jq -e --arg host "$(hostname)" --arg correlation "$correlation" --arg archive "$retry_archive" --arg receipt "$UPGRADE_RECEIPT" --arg inputs "$BACKUP_ROOT" \
    '.schemaVersion=="blazn.dev/node-broker-upgrade-retry/v1" and .owner=="blazn-poc" and .host==$host and .correlationId==$correlation and .previousReceipt.sourcePath==$receipt and .previousReceipt.retainedPath==($archive+"/receipt.json") and .previousInputs.sourcePath==$inputs and .previousInputs.retainedPath==($archive+"/inputs")' "$retry_journal" >/dev/null || die "post-rollback retry journal is invalid"
  phase=$(jq -er '.phase' "$retry_journal")
  case "$phase" in prepared|inputs-retained|receipt-retained) ;; *) die "post-rollback retry journal phase is invalid" ;; esac
  retained=$(jq -er '.rollbackEvidencePath' "$retry_journal"); retained_digest=$(jq -er '.rollbackEvidenceDigest' "$retry_journal")
  [ "$retained_digest" = "$(evidence_tree_digest "$retained")" ] || die "rollback evidence changed after retry approval"
  if [ "$phase" = prepared ]; then
    if [ ! -e "$retry_archive" ]; then mkdir -- "$retry_archive"; chmod 0700 "$retry_archive"; sync_path "$RETRY_HISTORY_ROOT"; fi
    assert_directory_owned_mode "$retry_archive" 0 700
    retained_inputs=$retry_archive/inputs
    if [ -d "$BACKUP_ROOT" ] && [ ! -e "$retained_inputs" ]; then mv -- "$BACKUP_ROOT" "$retained_inputs"; sync_path "$retry_archive"; sync_path "$OWNERSHIP_ROOT"; elif [ ! -e "$BACKUP_ROOT" ] && [ -d "$retained_inputs" ]; then :; else die "post-rollback input retention transition is ambiguous"; fi
    if [ -e "$BACKUP_ROOT" ] || [ ! -d "$retained_inputs" ]; then
      die "post-rollback input retention did not reach its durable state"
    fi
    write_retry_phase "$retry_journal" inputs-retained; phase=inputs-retained; test_fault retry-inputs-retained
  fi
  if [ "$phase" = inputs-retained ]; then
    retained_receipt=$retry_archive/receipt.json; expected=$(jq -er '.previousReceipt.digest' "$retry_journal")
    if [ -f "$UPGRADE_RECEIPT" ] && [ ! -e "$retained_receipt" ]; then [ "$expected" = "sha256:$(sha "$UPGRADE_RECEIPT")" ] || die "rolled-back receipt changed during retry retention"; mv -- "$UPGRADE_RECEIPT" "$retained_receipt"; sync_path "$retry_archive"; sync_path "$OWNERSHIP_ROOT"; elif [ ! -e "$UPGRADE_RECEIPT" ] && [ -f "$retained_receipt" ]; then :; else die "post-rollback receipt retention transition is ambiguous"; fi
    if [ -e "$UPGRADE_RECEIPT" ] || [ ! -f "$retained_receipt" ]; then
      die "post-rollback receipt retention did not reach its durable state"
    fi
    [ "$expected" = "sha256:$(sha "$retained_receipt")" ] || die "retained rolled-back receipt digest differs"
    write_retry_phase "$retry_journal" receipt-retained; phase=receipt-retained; test_fault retry-receipt-retained
  fi
  retained_receipt=$retry_archive/receipt.json; retained_inputs=$retry_archive/inputs
  expected=$(jq -er '.previousReceipt.digest' "$retry_journal")
  [ "$expected" = "sha256:$(sha "$retained_receipt")" ] || die "retained rolled-back receipt digest differs"
  jq -e --arg main "$BACKUP_ROOT/control-plane.json" --arg environment "$BACKUP_ROOT/control-plane.env" --arg build "$BACKUP_ROOT/control-api-build.json" \
    '.inputs.mainReceipt.backupPath==$main and .inputs.environment.backupPath==$environment and .inputs.buildReceipt.backupPath==$build' "$retained_receipt" >/dev/null || die "retained rolled-back receipt has unexpected backup paths"
  for pair in mainReceipt environment; do
    expected=$(jq -er --arg pair "$pair" '.inputs[$pair].digest' "$retained_receipt")
    name=$(basename -- "$(jq -er --arg pair "$pair" '.inputs[$pair].backupPath' "$retained_receipt")")
    [ "$expected" = "sha256:$(sha "$retained_inputs/$name")" ] || die "retained rolled-back input backup changed: $pair"
  done
  if [ "$(jq -er '.inputs.buildReceipt.present' "$retained_receipt")" = true ]; then
    expected=$(jq -er '.inputs.buildReceipt.digest' "$retained_receipt")
    [ "$expected" = "sha256:$(sha "$retained_inputs/control-api-build.json")" ] || die "retained rolled-back build receipt backup changed"
  fi
  retry_metadata=$(jq -cS '{correlationId,previousReceipt:{path:.previousReceipt.retainedPath,digest:.previousReceipt.digest},previousInputsPath:.previousInputs.retainedPath,rollbackEvidencePath,rollbackEvidenceDigest}' "$retry_journal")
  if [ -e "$UPGRADE_RECEIPT" ]; then
    jq -e --arg correlation "$correlation" '.phase!="rolled-back" and .retry.correlationId==$correlation' "$UPGRADE_RECEIPT" >/dev/null || die "retry correlation already retained a different or rolled-back attempt; use a new correlation ID"
  else
    for pair in mainReceipt environment; do
      expected=$(jq -er --arg pair "$pair" '.inputs[$pair].digest' "$retained_receipt"); target=$(jq -er --arg pair "$pair" '.inputs[$pair].path' "$retained_receipt")
      [ "$expected" = "sha256:$(sha "$target")" ] || die "rolled-back upgrade input changed before retry: $pair"
    done
    build_target=$(jq -er '.inputs.buildReceipt.path' "$retained_receipt")
    if [ "$(jq -er '.inputs.buildReceipt.present' "$retained_receipt")" = true ]; then expected=$(jq -er '.inputs.buildReceipt.digest' "$retained_receipt"); [ "$expected" = "sha256:$(sha "$build_target")" ] || die "rolled-back build receipt changed before retry"; elif [ -e "$build_target" ]; then die "rolled-back build receipt presence changed before retry"; fi
  fi
}
backup_input() {
  source=$1; destination=$2
  if [ ! -e "$destination" ]; then cp --preserve=mode,timestamps -- "$source" "$destination"; sync_path "$destination"; sync_path "$BACKUP_ROOT"; fi
  assert_regular_file_owned_mode "$destination" 0 600
  [ "$(sha "$destination")" = "$(sha "$source")" ] || die "unbound upgrade input backup differs from its source"
}
ensure_env_binding() {
  for binding in 'BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets' 'BLAZN_NODE_PLAN_ROOT=/etc/blazn/node-plan'; do
    name=${binding%%=*}; count=$(grep -c "^$name=" "$ENV_FILE" || true)
    case "$count" in
      0) tmp=$ENV_FILE.tmp.$$; { sed -n '1,$p' "$ENV_FILE"; printf '%s\n' "$binding"; } >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$ENV_FILE"; sync_path "$(dirname -- "$ENV_FILE")" ;;
      1) grep -Fx "$binding" "$ENV_FILE" >/dev/null || die "existing $name environment binding conflicts" ;;
      *) die "duplicate $name environment bindings" ;;
    esac
  done
}

retry_metadata=null
if [ -n "$RETRY_CORRELATION" ]; then
  prepare_post_rollback_retry "$RETRY_CORRELATION"
elif [ -e "$UPGRADE_RECEIPT" ]; then
  assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
  jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/node-broker-upgrade/v2" and .owner=="blazn-poc" and .host==$host' "$UPGRADE_RECEIPT" >/dev/null || die "upgrade receipt is invalid"
  existing_phase=$(jq -er '.phase' "$UPGRADE_RECEIPT")
  case "$existing_phase" in
    inputs-backed-up|role-ready|environment-bound|build-ready|complete) ;;
    rolled-back) die "upgrade was rolled back; inspect retained evidence, then retry explicitly with --retry-after-rollback CORRELATION_ID" ;;
    rollback-started|role-removed|secrets-retained|environment-restored|build-restored|main-restored|source-restore-required) die "upgrade is in rollback recovery" ;;
    *) die "upgrade phase is invalid" ;;
  esac
elif [ ! -e "$UPGRADE_RECEIPT" ] && [ -d "$RETRY_HISTORY_ROOT" ]; then
  die "post-rollback retry retention is incomplete; rerun with its exact --retry-after-rollback CORRELATION_ID"
fi

BLAZN_NODE_BROKER_SECRETS_ROOT="$NODE_SECRETS" \
BLAZN_NODE_BROKER_CREATE_JOURNAL="$CREATE_JOURNAL" \
  "$SCRIPT_DIR/create-secrets.sh" >/dev/null
BLAZN_NODE_PLAN_TEST_MODE=${BLAZN_NODE_PLAN_TEST_MODE:-${BLAZN_NODE_INFRA_TEST_MODE:-0}} \
BLAZN_NODE_PLAN_CREATE_JOURNAL="$PLAN_CREATE_JOURNAL" \
  "$SCRIPT_DIR/create-plan-materials.sh" >/dev/null
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
  node_json=$(node_object); plan_json=$(plan_object); tmp=$UPGRADE_RECEIPT.tmp.$$
  jq -cn \
    --arg host "$(hostname)" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg main "$MAIN_RECEIPT" --arg mainBackup "$BACKUP_ROOT/control-plane.json" --arg mainDigest "sha256:$(sha "$MAIN_RECEIPT")" \
    --arg env "$ENV_FILE" --arg envBackup "$BACKUP_ROOT/control-plane.env" --arg envDigest "sha256:$(sha "$ENV_FILE")" \
    --arg build "$BUILD_RECEIPT" --arg buildBackup "$BACKUP_ROOT/control-api-build.json" --arg buildDigest "$build_digest" \
    --argjson buildPresent "$build_present" --arg sourceDigest "$(jq -er '.controlApi.sourceDigest // ""' "$MAIN_RECEIPT")" \
    --arg configDigest "$(jq -er '.configDigest // ""' "$MAIN_RECEIPT")" --argjson nodeBroker "$node_json" --argjson nodePlan "$plan_json" --argjson retry "$retry_metadata" \
    '{schemaVersion:"blazn.dev/node-broker-upgrade/v2",owner:"blazn-poc",host:$host,phase:"inputs-backed-up",createdAt:$createdAt,inputs:{mainReceipt:{path:$main,backupPath:$mainBackup,digest:$mainDigest},environment:{path:$env,backupPath:$envBackup,digest:$envDigest},buildReceipt:{path:$build,backupPath:$buildBackup,present:$buildPresent,digest:$buildDigest},sourceDigest:$sourceDigest,configDigest:$configDigest},nodeBroker:$nodeBroker,nodePlan:$nodePlan} | if $retry==null then . else .retry=$retry end' >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"
fi

assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/node-broker-upgrade/v2" and .owner=="blazn-poc" and .host==$host' "$UPGRADE_RECEIPT" >/dev/null || die "upgrade receipt is invalid"
for pair in mainReceipt environment; do backup=$(jq -er --arg pair "$pair" '.inputs[$pair].backupPath' "$UPGRADE_RECEIPT"); expected=$(jq -er --arg pair "$pair" '.inputs[$pair].digest' "$UPGRADE_RECEIPT"); [ "$expected" = "sha256:$(sha "$backup")" ] || die "upgrade input backup changed: $pair"; done
[ "$(node_object)" = "$(jq -cS .nodeBroker "$UPGRADE_RECEIPT")" ] || die "installed Node broker secrets differ from upgrade receipt"
[ "$(plan_object)" = "$(jq -cS .nodePlan "$UPGRADE_RECEIPT")" ] || die "installed Node plan material differs from upgrade receipt"
phase=$(jq -er .phase "$UPGRADE_RECEIPT")
case "$phase" in inputs-backed-up|role-ready|environment-bound|build-ready|complete) ;; rollback-*) die "upgrade is in rollback recovery" ;; rolled-back) die "upgrade was rolled back" ;; *) die "upgrade phase is invalid" ;; esac
if [ "$phase" != inputs-backed-up ]; then
  jq -e '.databaseRoles.sandboxControllerPreexisting|type=="boolean"' "$UPGRADE_RECEIPT" >/dev/null || die "Sandbox controller role preexistence receipt is absent"
fi

if [ -f "$BUILD_RECEIPT" ]; then CONTROL_API_IMAGE=$(jq -er .image "$BUILD_RECEIPT"); else CONTROL_API_IMAGE=blazn-control-api:upgrade-placeholder; fi
export CONTROL_API_IMAGE
compose() { docker compose -f "$M2_ROOT/compose.yaml" --env-file "$ENV_FILE" "$@"; }

if [ "$phase" = inputs-backed-up ]; then
  postgres_container=$(compose ps -q postgres)
  [ -n "$postgres_container" ] || die "the exact Blazn PostgreSQL container is not running"
  [ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.State.Status}}' "$postgres_container")" = blazn-m2/postgres/running ] || die "the running PostgreSQL container is not the expected Compose service"
  controller_role_preexisting=$(jq -r 'if ((.databaseRoles? | type)=="object" and (.databaseRoles | has("sandboxControllerPreexisting"))) then .databaseRoles.sandboxControllerPreexisting else "unrecorded" end' "$UPGRADE_RECEIPT")
  if [ "$controller_role_preexisting" = unrecorded ]; then
    controller_role_count=$(compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from pg_roles where rolname='blazn_sandbox_controller'")
    case "$controller_role_count" in 0) controller_role_preexisting=false ;; 1) controller_role_preexisting=true ;; *) die "could not determine Sandbox controller role state" ;; esac
    tmp=$UPGRADE_RECEIPT.tmp.$$
    jq --argjson preexisting "$controller_role_preexisting" '.databaseRoles.sandboxControllerPreexisting=$preexisting' "$UPGRADE_RECEIPT" >"$tmp"
    chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$UPGRADE_RECEIPT"; sync_path "$(dirname -- "$UPGRADE_RECEIPT")"
  fi
  case "$controller_role_preexisting" in true|false) ;; *) die "Sandbox controller role preexistence receipt is invalid" ;; esac
  url=$(sed -n '1p' "$NODE_SECRETS/database-url"); password=${url#*://*:}; password=${password%%@*}
  {
    printf 'BEGIN;\n'
    printf "DO \$block\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='blazn_node_broker') THEN EXECUTE 'CREATE ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS'; END IF; END \$block\$;\n"
    printf "DO \$block\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='blazn_sandbox_controller') THEN EXECUTE 'CREATE ROLE blazn_sandbox_controller NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS'; END IF; END \$block\$;\n"
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
    printf 'GRANT CONNECT ON DATABASE "%s" TO blazn_node_broker, blazn_sandbox_controller; GRANT USAGE ON SCHEMA public TO blazn_node_broker, blazn_sandbox_controller; COMMIT;\n' "${POSTGRES_DB:-blazn}"
  } | compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null
  test_fault role-transaction-committed
  compose run --rm -T node-migration-preflight >/dev/null
  write_phase role-ready; phase=role-ready; test_fault role-ready
fi

if [ "$phase" = role-ready ]; then ensure_env_binding; write_phase environment-bound; phase=environment-bound; test_fault environment-bound; fi
if [ "$phase" = environment-bound ] && [ "$DEFER_CONFIG" = 1 ]; then
  grep -Fx 'BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets' "$ENV_FILE" >/dev/null || die "environment binding is absent"
  grep -Fx 'BLAZN_NODE_PLAN_ROOT=/etc/blazn/node-plan' "$ENV_FILE" >/dev/null || die "plan environment binding is absent"
  printf 'Node broker prerequisites are environment-bound; config publication is deferred until after release promotion\n'
  exit 0
fi
if [ "$phase" = environment-bound ]; then
  if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" != 1 ]; then "$M2_ROOT/scripts/build-control-api.sh" >/dev/null; load_control_api_image "$M2_ROOT"; fi
  write_phase build-ready; phase=build-ready; test_fault build-ready
fi
if [ "$phase" = build-ready ]; then
  node_json=$(jq -cS .nodeBroker "$UPGRADE_RECEIPT"); plan_json=$(jq -cS .nodePlan "$UPGRADE_RECEIPT"); tmp=$MAIN_RECEIPT.tmp.$$
  if [ -f "$BUILD_RECEIPT" ]; then
    jq --argjson nodeBroker "$node_json" --argjson nodePlan "$plan_json" --arg digest "sha256:$(control_plane_config_digest "$M2_ROOT")" \
      --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg source "$(jq -er .sourceDigest "$BUILD_RECEIPT")" \
      --arg image "$(jq -er .image "$BUILD_RECEIPT")" --arg imageId "$(jq -er .imageId "$BUILD_RECEIPT")" \
      '.nodeBroker=$nodeBroker | .nodePlan=$nodePlan | .configDigest=$digest | .configUpdatedAt=$updatedAt | .controlApi={sourceDigest:$source,image:$image,imageId:$imageId}' "$MAIN_RECEIPT" >"$tmp"
  else
    jq --argjson nodeBroker "$node_json" --argjson nodePlan "$plan_json" '.nodeBroker=$nodeBroker | .nodePlan=$nodePlan' "$MAIN_RECEIPT" >"$tmp"
  fi
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$MAIN_RECEIPT"; sync_path "$(dirname -- "$MAIN_RECEIPT")"
  write_phase complete; phase=complete; test_fault complete
fi

[ "$(jq -cS .nodeBroker "$MAIN_RECEIPT")" = "$(jq -cS .nodeBroker "$UPGRADE_RECEIPT")" ] || die "main receipt is not bound to Node broker prerequisites"
[ "$(jq -cS .nodePlan "$MAIN_RECEIPT")" = "$(jq -cS .nodePlan "$UPGRADE_RECEIPT")" ] || die "main receipt is not bound to Node plan material"
grep -Fx 'BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets' "$ENV_FILE" >/dev/null || die "environment binding is absent"
grep -Fx 'BLAZN_NODE_PLAN_ROOT=/etc/blazn/node-plan' "$ENV_FILE" >/dev/null || die "plan environment binding is absent"
printf 'Node broker infrastructure upgrade is complete and config-reconciled\n'
