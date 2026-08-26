#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
M2_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/../../milestone-2" && pwd)
# shellcheck disable=SC1091
. "$M2_ROOT/scripts/common.sh"

operation=forward
case "$#" in
  0) ;;
  1) [ "$1" = --rollback ] || die "usage: rotate-plan-materials.sh [--rollback]"; operation=rollback ;;
  *) die "usage: rotate-plan-materials.sh [--rollback]" ;;
esac

[ "$(id -u)" -eq 0 ] || die "Node plan rotation must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Node plan rotation must run through the control-plane lock"
for command_name in cmp jq node sha256sum stat sync; do require_command "$command_name"; done

ROOT=${BLAZN_NODE_PLAN_ROOT:-/etc/blazn/node-plan}
JOURNAL=${BLAZN_NODE_PLAN_CREATE_JOURNAL:-/var/lib/blazn/ownership/node-plan-material-upgrade-create.json}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
UPGRADE_RECEIPT=${BLAZN_NODE_BROKER_UPGRADE_RECEIPT:-/var/lib/blazn/ownership/node-broker-upgrade.json}
HISTORY_ROOT=${BLAZN_NODE_PLAN_ROTATION_ROOT:-/var/lib/blazn/ownership/node-plan-material-rotations}
SOURCE_TEMPLATE=${BLAZN_NODE_PLAN_TEMPLATE_SOURCE:-$SCRIPT_DIR/../templates/node-install-plan-template-v1.json}
SOURCE_TEMPLATES=${BLAZN_NODE_PLAN_SOURCE_TEMPLATES:-$SCRIPT_DIR/../templates}
CORRELATION=${BLAZN_CORRELATION_ID:-}
TEST_MODE=${BLAZN_NODE_PLAN_ROTATION_TEST_MODE:-0}

case "$CORRELATION" in ''|*[!a-zA-Z0-9._-]*) die "Node plan rotation correlation ID is invalid" ;; esac
for path in "$ROOT" "$JOURNAL" "$MAIN_RECEIPT" "$UPGRADE_RECEIPT" "$HISTORY_ROOT" "$SOURCE_TEMPLATE" "$SOURCE_TEMPLATES"; do
  require_absolute_path node-plan-rotation-path "$path"
  assert_not_symlink_chain "$path"
done
if [ "$TEST_MODE" != 1 ]; then
  [ "$ROOT" = /etc/blazn/node-plan ] || die "Node plan material root is outside the reviewed path"
  [ "$JOURNAL" = /var/lib/blazn/ownership/node-plan-material-upgrade-create.json ] || die "Node plan journal is outside the reviewed path"
  [ "$MAIN_RECEIPT" = /var/lib/blazn/ownership/control-plane.json ] || die "main receipt is outside the reviewed path"
  [ "$UPGRADE_RECEIPT" = /var/lib/blazn/ownership/node-broker-upgrade.json ] || die "upgrade receipt is outside the reviewed path"
  [ "$HISTORY_ROOT" = /var/lib/blazn/ownership/node-plan-material-rotations ] || die "rotation history is outside the reviewed path"
  case "$SOURCE_TEMPLATE" in /opt/blazn-releases/*/infra/node/templates/node-install-plan-template-v1.json) ;; *) die "source template is outside an immutable release" ;; esac
  systemctl is-active --quiet blazn-control-plane.service && die "stop blazn-control-plane.service before rotating Node plan material"
fi

sha() { sha256_file "$1"; }
digest() { printf 'sha256:%s\n' "$(sha "$1")"; }
sync_path() { sync -f "$1"; }
atomic_copy() {
  source=$1; target=$2; mode=$3
  temporary=$target.tmp.$$
  cp -- "$source" "$temporary"
  chown 0:0 "$temporary"; chmod "$mode" "$temporary"; sync_path "$temporary"
  mv -- "$temporary" "$target"; sync_path "$(dirname -- "$target")"
}
fault() {
  [ "$TEST_MODE" = 1 ] || return 0
  [ "${BLAZN_NODE_PLAN_ROTATION_FAIL_AFTER:-}" != "$1" ] || die "injected Node plan rotation fault after $1"
}
plan_object() {
  BLAZN_NODE_PLAN_TEST_MODE=$TEST_MODE BLAZN_NODE_PLAN_ROOT="$ROOT" BLAZN_NODE_PLAN_CREATE_JOURNAL="$JOURNAL" \
    "$SCRIPT_DIR/plan-material-object.sh"
}
verify_plan() {
  BLAZN_NODE_PLAN_ROOT="$ROOT" BLAZN_NODE_PLAN_SOURCE_TEMPLATES="$SOURCE_TEMPLATES" \
    node "$SCRIPT_DIR/verify-plan-materials.mjs" >/dev/null
}

assert_regular_file_owned_mode "$SOURCE_TEMPLATE" 0 444
assert_directory_owned_mode "$ROOT" 0 700
for material in signing-private-v1.b64url signing-public-v1.b64url signing-public-v1.json node-install-plan-template-v1.json; do
  assert_regular_file_owned_mode "$ROOT/$material" 0 444
done
for receipt_file in "$JOURNAL" "$MAIN_RECEIPT" "$UPGRADE_RECEIPT"; do assert_regular_file_owned_mode "$receipt_file" 0 600; done
jq -e '.schemaVersion=="blazn.dev/node-plan-material-create/v1" and .owner=="blazn-poc" and .phase=="published"' "$JOURNAL" >/dev/null || die "Node plan creation journal is invalid"
jq -e '.schemaVersion=="blazn.dev/node-broker-upgrade/v2" and .owner=="blazn-poc" and .phase=="complete"' "$UPGRADE_RECEIPT" >/dev/null || die "Node broker upgrade receipt is invalid"

if [ ! -e "$HISTORY_ROOT" ]; then
  mkdir -- "$HISTORY_ROOT"; chown 0:0 "$HISTORY_ROOT"; chmod 0700 "$HISTORY_ROOT"; sync_path "$(dirname -- "$HISTORY_ROOT")"
fi
assert_directory_owned_mode "$HISTORY_ROOT" 0 700
ROTATION_DIR=$HISTORY_ROOT/$CORRELATION
ROTATION_RECEIPT=$ROTATION_DIR/receipt.json

write_phase() {
  next=$1; temporary=$ROTATION_RECEIPT.tmp.$$
  jq --arg phase "$next" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$ROTATION_RECEIPT" >"$temporary"
  chown 0:0 "$temporary"; chmod 0600 "$temporary"; sync_path "$temporary"
  mv -- "$temporary" "$ROTATION_RECEIPT"; sync_path "$ROTATION_DIR"
}

if [ ! -e "$ROTATION_DIR" ]; then
  [ "$operation" = forward ] || die "Node plan rotation receipt does not exist"
  current_plan=$(plan_object)
  [ "$(printf '%s' "$current_plan" | jq -cS .)" = "$(jq -cS .nodePlan "$MAIN_RECEIPT")" ] || die "main receipt does not bind the current Node plan"
  [ "$(printf '%s' "$current_plan" | jq -cS .)" = "$(jq -cS .nodePlan "$UPGRADE_RECEIPT")" ] || die "upgrade receipt does not bind the current Node plan"
  [ "$(jq -er .sourceDigest "$JOURNAL")" = "$(digest "$ROOT/node-install-plan-template-v1.json")" ] || die "creation journal does not bind the installed template"

  temporary_dir=$HISTORY_ROOT/.rotation-$CORRELATION.$$
  mkdir -- "$temporary_dir"; chown 0:0 "$temporary_dir"; chmod 0700 "$temporary_dir"
  mkdir -- "$temporary_dir/before" "$temporary_dir/after"
  chown 0:0 "$temporary_dir/before" "$temporary_dir/after"; chmod 0700 "$temporary_dir/before" "$temporary_dir/after"
  atomic_copy "$ROOT/node-install-plan-template-v1.json" "$temporary_dir/before/template.json" 0444
  atomic_copy "$JOURNAL" "$temporary_dir/before/journal.json" 0600
  atomic_copy "$MAIN_RECEIPT" "$temporary_dir/before/main-receipt.json" 0600
  atomic_copy "$UPGRADE_RECEIPT" "$temporary_dir/before/upgrade-receipt.json" 0600
  atomic_copy "$SOURCE_TEMPLATE" "$temporary_dir/after/template.json" 0444
  created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  jq --arg source "$SOURCE_TEMPLATE" --arg sourceDigest "$(digest "$SOURCE_TEMPLATE")" --arg updatedAt "$created_at" \
    '.source=$source|.sourceDigest=$sourceDigest|.updatedAt=$updatedAt' "$JOURNAL" >"$temporary_dir/after/journal.json"
  chown 0:0 "$temporary_dir/after/journal.json"; chmod 0600 "$temporary_dir/after/journal.json"; sync_path "$temporary_dir/after/journal.json"
  after_plan=$(printf '%s' "$current_plan" | jq -cS --arg templateDigest "$(digest "$SOURCE_TEMPLATE")" --arg journalDigest "$(digest "$temporary_dir/after/journal.json")" '.templateDigest=$templateDigest|.creationJournal.digest=$journalDigest')
  jq --argjson nodePlan "$after_plan" --arg updatedAt "$created_at" '.nodePlan=$nodePlan|.updatedAt=$updatedAt' "$UPGRADE_RECEIPT" >"$temporary_dir/after/upgrade-receipt.json"
  jq --argjson nodePlan "$after_plan" '.nodePlan=$nodePlan' "$MAIN_RECEIPT" >"$temporary_dir/after/main-receipt.json"
  chown 0:0 "$temporary_dir/after/upgrade-receipt.json" "$temporary_dir/after/main-receipt.json"
  chmod 0600 "$temporary_dir/after/upgrade-receipt.json" "$temporary_dir/after/main-receipt.json"
  sync_path "$temporary_dir/after/upgrade-receipt.json"; sync_path "$temporary_dir/after/main-receipt.json"
  jq -cnS \
    --arg host "$(hostname)" --arg correlationId "$CORRELATION" --arg createdAt "$created_at" \
    --arg sourceTemplate "$SOURCE_TEMPLATE" --arg root "$ROOT" --arg journal "$JOURNAL" --arg mainReceipt "$MAIN_RECEIPT" --arg upgradeReceipt "$UPGRADE_RECEIPT" \
    --arg beforeTemplate "$(digest "$temporary_dir/before/template.json")" --arg beforeJournal "$(digest "$temporary_dir/before/journal.json")" \
    --arg beforeMain "$(digest "$temporary_dir/before/main-receipt.json")" --arg beforeUpgrade "$(digest "$temporary_dir/before/upgrade-receipt.json")" \
    --arg afterTemplate "$(digest "$temporary_dir/after/template.json")" --arg afterJournal "$(digest "$temporary_dir/after/journal.json")" \
    --arg afterMain "$(digest "$temporary_dir/after/main-receipt.json")" --arg afterUpgrade "$(digest "$temporary_dir/after/upgrade-receipt.json")" \
    '{schemaVersion:"blazn.dev/node-plan-material-rotation/v1",owner:"blazn-poc",host:$host,correlationId:$correlationId,phase:"initialized",createdAt:$createdAt,updatedAt:$createdAt,paths:{sourceTemplate:$sourceTemplate,root:$root,journal:$journal,mainReceipt:$mainReceipt,upgradeReceipt:$upgradeReceipt},before:{templateDigest:$beforeTemplate,journalDigest:$beforeJournal,mainReceiptDigest:$beforeMain,upgradeReceiptDigest:$beforeUpgrade},after:{templateDigest:$afterTemplate,journalDigest:$afterJournal,mainReceiptDigest:$afterMain,upgradeReceiptDigest:$afterUpgrade}}' >"$temporary_dir/receipt.json"
  chown 0:0 "$temporary_dir/receipt.json"; chmod 0600 "$temporary_dir/receipt.json"; sync_path "$temporary_dir/receipt.json"
  sync_path "$temporary_dir/before"; sync_path "$temporary_dir/after"; sync_path "$temporary_dir"
  mv -- "$temporary_dir" "$ROTATION_DIR"; sync_path "$HISTORY_ROOT"
  fault initialized
fi

assert_directory_owned_mode "$ROTATION_DIR" 0 700
assert_regular_file_owned_mode "$ROTATION_RECEIPT" 0 600
jq -e --arg host "$(hostname)" --arg correlation "$CORRELATION" --arg root "$ROOT" --arg journal "$JOURNAL" --arg main "$MAIN_RECEIPT" --arg upgrade "$UPGRADE_RECEIPT" \
  '.schemaVersion=="blazn.dev/node-plan-material-rotation/v1" and .owner=="blazn-poc" and .host==$host and .correlationId==$correlation and .paths.root==$root and .paths.journal==$journal and .paths.mainReceipt==$main and .paths.upgradeReceipt==$upgrade' "$ROTATION_RECEIPT" >/dev/null || die "Node plan rotation receipt is invalid"
for relative in before/template.json before/journal.json before/main-receipt.json before/upgrade-receipt.json after/template.json after/journal.json after/main-receipt.json after/upgrade-receipt.json; do
  case "$relative" in */template.json) expected_mode=444 ;; *) expected_mode=600 ;; esac
  assert_regular_file_owned_mode "$ROTATION_DIR/$relative" 0 "$expected_mode"
done
for name in template journal mainReceipt upgradeReceipt; do
  case "$name" in
    template) before_path=before/template.json; after_path=after/template.json ;;
    journal) before_path=before/journal.json; after_path=after/journal.json ;;
    mainReceipt) before_path=before/main-receipt.json; after_path=after/main-receipt.json ;;
    upgradeReceipt) before_path=before/upgrade-receipt.json; after_path=after/upgrade-receipt.json ;;
  esac
  [ "$(jq -er --arg name "$name" '.before[$name+"Digest"]' "$ROTATION_RECEIPT")" = "$(digest "$ROTATION_DIR/$before_path")" ] || die "rotation before-image digest changed: $name"
  [ "$(jq -er --arg name "$name" '.after[$name+"Digest"]' "$ROTATION_RECEIPT")" = "$(digest "$ROTATION_DIR/$after_path")" ] || die "rotation after-image digest changed: $name"
done

artifact_state() {
  target=$1; before=$2; after=$3; actual=$(digest "$target")
  if [ "$actual" = "$(digest "$before")" ]; then printf 'before\n'; elif [ "$actual" = "$(digest "$after")" ]; then printf 'after\n'; else die "Node plan rotation observed unreceipted content: $target"; fi
}
require_state() { [ "$(artifact_state "$1" "$2" "$3")" = "$4" ] || die "Node plan rotation phase does not match $1"; }
publish_after() {
  target=$1; before=$2; after=$3; mode=$4
  state=$(artifact_state "$target" "$before" "$after")
  [ "$state" = after ] || atomic_copy "$after" "$target" "$mode"
  require_state "$target" "$before" "$after" after
}
restore_before() {
  target=$1; before=$2; after=$3; mode=$4
  state=$(artifact_state "$target" "$before" "$after")
  [ "$state" = before ] || atomic_copy "$before" "$target" "$mode"
  require_state "$target" "$before" "$after" before
}

live_template=$ROOT/node-install-plan-template-v1.json
before_template=$ROTATION_DIR/before/template.json; after_template=$ROTATION_DIR/after/template.json
before_journal=$ROTATION_DIR/before/journal.json; after_journal=$ROTATION_DIR/after/journal.json
before_upgrade=$ROTATION_DIR/before/upgrade-receipt.json; after_upgrade=$ROTATION_DIR/after/upgrade-receipt.json
before_main=$ROTATION_DIR/before/main-receipt.json; after_main=$ROTATION_DIR/after/main-receipt.json

if [ "$operation" = rollback ]; then
  phase=$(jq -er .phase "$ROTATION_RECEIPT")
  case "$phase" in rolled-back) ;; rollback-*) ;; initialized|template-published|journal-published|upgrade-receipt-published|main-receipt-published|complete) write_phase rollback-started ;; *) die "Node plan rotation phase is invalid" ;; esac
  phase=$(jq -er .phase "$ROTATION_RECEIPT")
  if [ "$phase" = rollback-started ]; then restore_before "$MAIN_RECEIPT" "$before_main" "$after_main" 0600; write_phase rollback-main-restored; fault rollback-main-restored; phase=rollback-main-restored; fi
  if [ "$phase" = rollback-main-restored ]; then restore_before "$UPGRADE_RECEIPT" "$before_upgrade" "$after_upgrade" 0600; write_phase rollback-upgrade-restored; fault rollback-upgrade-restored; phase=rollback-upgrade-restored; fi
  if [ "$phase" = rollback-upgrade-restored ]; then restore_before "$JOURNAL" "$before_journal" "$after_journal" 0600; write_phase rollback-journal-restored; fault rollback-journal-restored; phase=rollback-journal-restored; fi
  if [ "$phase" = rollback-journal-restored ]; then restore_before "$live_template" "$before_template" "$after_template" 0444; write_phase rolled-back; fault rolled-back; phase=rolled-back; fi
  [ "$phase" = rolled-back ] || die "Node plan rollback did not finish"
  require_state "$MAIN_RECEIPT" "$before_main" "$after_main" before
  require_state "$UPGRADE_RECEIPT" "$before_upgrade" "$after_upgrade" before
  require_state "$JOURNAL" "$before_journal" "$after_journal" before
  require_state "$live_template" "$before_template" "$after_template" before
  restored_plan=$(plan_object)
  [ "$(printf '%s' "$restored_plan" | jq -cS .)" = "$(jq -cS .nodePlan "$MAIN_RECEIPT")" ] || die "restored main receipt does not bind the Node plan"
  [ "$(printf '%s' "$restored_plan" | jq -cS .)" = "$(jq -cS .nodePlan "$UPGRADE_RECEIPT")" ] || die "restored upgrade receipt does not bind the Node plan"
  printf 'Node plan rotation rolled back; evidence retained at %s\n' "$ROTATION_DIR"
  exit 0
fi

phase=$(jq -er .phase "$ROTATION_RECEIPT")
case "$phase" in rollback-*|rolled-back) die "Node plan rotation is in rollback recovery" ;; initialized|template-published|journal-published|upgrade-receipt-published|main-receipt-published|complete) ;; *) die "Node plan rotation phase is invalid" ;; esac
if [ "$phase" = initialized ]; then
  require_state "$JOURNAL" "$before_journal" "$after_journal" before; require_state "$UPGRADE_RECEIPT" "$before_upgrade" "$after_upgrade" before; require_state "$MAIN_RECEIPT" "$before_main" "$after_main" before
  publish_after "$live_template" "$before_template" "$after_template" 0444; verify_plan
  write_phase template-published; fault template-published; phase=template-published
fi
if [ "$phase" = template-published ]; then
  require_state "$live_template" "$before_template" "$after_template" after; require_state "$UPGRADE_RECEIPT" "$before_upgrade" "$after_upgrade" before; require_state "$MAIN_RECEIPT" "$before_main" "$after_main" before
  publish_after "$JOURNAL" "$before_journal" "$after_journal" 0600
  write_phase journal-published; fault journal-published; phase=journal-published
fi
if [ "$phase" = journal-published ]; then
  require_state "$live_template" "$before_template" "$after_template" after; require_state "$JOURNAL" "$before_journal" "$after_journal" after; require_state "$MAIN_RECEIPT" "$before_main" "$after_main" before
  publish_after "$UPGRADE_RECEIPT" "$before_upgrade" "$after_upgrade" 0600
  write_phase upgrade-receipt-published; fault upgrade-receipt-published; phase=upgrade-receipt-published
fi
if [ "$phase" = upgrade-receipt-published ]; then
  require_state "$UPGRADE_RECEIPT" "$before_upgrade" "$after_upgrade" after
  publish_after "$MAIN_RECEIPT" "$before_main" "$after_main" 0600
  write_phase main-receipt-published; fault main-receipt-published; phase=main-receipt-published
fi
if [ "$phase" = main-receipt-published ]; then
  current_plan=$(plan_object)
  [ "$(printf '%s' "$current_plan" | jq -cS .)" = "$(jq -cS .nodePlan "$MAIN_RECEIPT")" ] || die "rotated main receipt does not bind the Node plan"
  [ "$(printf '%s' "$current_plan" | jq -cS .)" = "$(jq -cS .nodePlan "$UPGRADE_RECEIPT")" ] || die "rotated upgrade receipt does not bind the Node plan"
  verify_plan; write_phase complete; fault complete; phase=complete
fi
[ "$phase" = complete ] || die "Node plan rotation did not finish"
for tuple in "$live_template:$before_template:$after_template" "$JOURNAL:$before_journal:$after_journal" "$UPGRADE_RECEIPT:$before_upgrade:$after_upgrade" "$MAIN_RECEIPT:$before_main:$after_main"; do
  target=${tuple%%:*}; rest=${tuple#*:}; before=${rest%%:*}; after=${rest#*:}; require_state "$target" "$before" "$after" after
done
printf 'Node plan material rotation is complete; evidence retained at %s\n' "$ROTATION_DIR"
