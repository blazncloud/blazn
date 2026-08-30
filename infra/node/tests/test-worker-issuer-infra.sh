#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INSTALLER=$TEST_DIR/../scripts/install-worker-issuer.sh
UPGRADE=$TEST_DIR/../scripts/upgrade-worker-issuer-observation.sh
ROLLBACK=$TEST_DIR/../scripts/rollback-worker-issuer.sh
COMPOSE=$TEST_DIR/../../milestone-2/compose.yaml
command -v sudo >/dev/null 2>&1 || { printf 'worker issuer infra test skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'worker issuer infra test skipped: passwordless sudo unavailable\n'; exit 0; }
top=${TMPDIR:-/tmp}/blazn-worker-issuer-infra-$$
mkdir "$top"
cleanup(){ sudo find "$top" -xdev -type l -print | grep . >/dev/null && return 1; sudo find "$top" -xdev -type f -delete; sudo find "$top" -xdev -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM

helper=$top/helper
printf '#!/bin/sh\nexit 0\n' >"$helper"; chmod 0755 "$helper"
observed_helper=$top/helper-observed
printf '#!/bin/sh\n# observation-enforced\nexit 0\n' >"$observed_helper"; chmod 0755 "$observed_helper"
corrected_helper=$top/helper-observed-corrected
printf '#!/bin/sh\n# observation-enforced corrected\nexit 0\n' >"$corrected_helper"; chmod 0755 "$corrected_helper"
systemctl=$top/systemctl
# shellcheck disable=SC2016
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >>"$BLAZN_TEST_LOG"\nif [ "${BLAZN_TEST_FAIL_START:-0}" = 1 ] && [ "$1" = start ] && grep -q observation-enforced "$BLAZN_ISSUER_BINARY_PATH"; then exit 1; fi\n' >"$systemctl"; chmod 0755 "$systemctl"
tmpfiles=$top/tmpfiles
printf '#!/bin/sh\nexit 0\n' >"$tmpfiles"; chmod 0755 "$tmpfiles"

run_install(){
  install_root=$1; install_fault=${2:-}; installer=${3:-$INSTALLER}
  sudo env BLAZN_FENCING_TOKEN=test BLAZN_ISSUER_INFRA_TEST_MODE=1 BLAZN_ISSUER_TEST_FAIL_AFTER="$install_fault" BLAZN_TEST_LOG="$install_root/systemctl.log" \
    BLAZN_ISSUER_BINARY_SOURCE="$helper" BLAZN_ISSUER_BINARY_SHA256="sha256:$(sha256sum "$helper" | awk '{print $1}')" BLAZN_NODE_BROKER_UID=65532 BLAZN_ISSUER_TEST_BROKER_GID=65531 BLAZN_ISSUER_TEST_MICROK8S_GID=1001 BLAZN_ISSUER_TEST_REVISION=9072 \
    BLAZN_ISSUER_TEST_SYSTEMCTL="$systemctl" BLAZN_ISSUER_TEST_TMPFILES="$tmpfiles" \
    BLAZN_ISSUER_CONFIG_ROOT="$install_root/etc/issuer" BLAZN_ISSUER_BINARY_PATH="$install_root/usr/libexec/issuer" BLAZN_ISSUER_UNIT_PATH="$install_root/etc/systemd/issuer.service" BLAZN_ISSUER_TMPFILES_PATH="$install_root/etc/tmpfiles/issuer.conf" \
    BLAZN_ISSUER_STATE_ROOT="$install_root/issuer-state" BLAZN_ISSUER_RECEIPT_PATH="$install_root/ownership/issuer.json" BLAZN_ISSUER_RECOVERY_ROOT="$install_root/ownership/recovery" BLAZN_CONTROL_PLANE_ENV_FILE="$install_root/control-plane.env" BLAZN_RECEIPT_PATH="$install_root/control-plane.json" "$installer"
}
run_rollback(){ rollback_root=$1; rollback_fault=${2:-}; sudo env BLAZN_FENCING_TOKEN=test BLAZN_ISSUER_INFRA_TEST_MODE=1 BLAZN_ISSUER_ROLLBACK_TEST_FAIL_AFTER="$rollback_fault" BLAZN_ISSUER_STATE_ROOT="$rollback_root/issuer-state" BLAZN_ISSUER_RECEIPT_PATH="$rollback_root/ownership/issuer.json" BLAZN_ISSUER_TEST_SYSTEMCTL="$systemctl" BLAZN_TEST_LOG="$rollback_root/systemctl.log" "$ROLLBACK"; }
run_upgrade(){
  upgrade_root=$1; upgrade_fault=${2:-}
  upgrade_start_failure=${3:-0}
  upgrade_helper=${4:-$observed_helper}
  upgrade_now=${5:-}
  sudo env BLAZN_FENCING_TOKEN=test BLAZN_ISSUER_INFRA_TEST_MODE=1 BLAZN_ISSUER_UPGRADE_TEST_FAIL_AFTER="$upgrade_fault" BLAZN_ISSUER_UPGRADE_TEST_NOW="$upgrade_now" BLAZN_TEST_FAIL_START="$upgrade_start_failure" BLAZN_TEST_LOG="$upgrade_root/systemctl.log" \
    BLAZN_ISSUER_BINARY_SOURCE="$upgrade_helper" BLAZN_ISSUER_BINARY_SHA256="sha256:$(sha256sum "$upgrade_helper" | awk '{print $1}')" BLAZN_ISSUER_TEST_SYSTEMCTL="$systemctl" \
    BLAZN_ISSUER_BINARY_PATH="$upgrade_root/usr/libexec/issuer" BLAZN_ISSUER_RECEIPT_PATH="$upgrade_root/ownership/issuer.json" BLAZN_ISSUER_RECOVERY_ROOT="$upgrade_root/ownership/recovery" BLAZN_RECEIPT_PATH="$upgrade_root/control-plane.json" "$UPGRADE"
}
make_blocked_fixture(){
  fixture_root=$1
  mkdir -p "$fixture_root/ownership"; printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$fixture_root/control-plane.env"; printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$fixture_root/control-plane.json"; : >"$fixture_root/systemctl.log"
  sudo chown -R 0:0 "$fixture_root"; sudo chmod 0700 "$fixture_root" "$fixture_root/ownership"; sudo chmod 0600 "$fixture_root/control-plane.env" "$fixture_root/control-plane.json"
  run_install "$fixture_root" >/dev/null
  sudo jq '.liveJoinBlocked=true' "$fixture_root/ownership/issuer.json" | sudo tee "$fixture_root/ownership/issuer.json.tmp" >/dev/null
  sudo chmod 0600 "$fixture_root/ownership/issuer.json.tmp"; sudo mv -- "$fixture_root/ownership/issuer.json.tmp" "$fixture_root/ownership/issuer.json"
  digest=$(sudo jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$fixture_root/ownership/issuer.json"|sha256sum|awk '{print "sha256:"$1}')
  sudo jq --arg digest "$digest" '.microk8sIssuer.materialDigest=$digest' "$fixture_root/control-plane.json" | sudo tee "$fixture_root/control-plane.json.tmp" >/dev/null
  sudo chmod 0600 "$fixture_root/control-plane.json.tmp"; sudo mv -- "$fixture_root/control-plane.json.tmp" "$fixture_root/control-plane.json"
}

for fault in recovery-created initialized key-pending recovery-key-pending secret-created config-bound files-installed service-started complete; do
  root=$top/$fault; mkdir -p "$root/ownership"; printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$root/control-plane.env"; printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$root/control-plane.json"; : >"$root/systemctl.log"; sudo chown -R 0:0 "$root"; sudo chmod 0700 "$root" "$root/ownership"; sudo chmod 0600 "$root/control-plane.env" "$root/control-plane.json"
  if run_install "$root" "$fault" >"$top/$fault.out" 2>"$top/$fault.err"; then printf 'issuer fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected issuer fault after $fault" "$top/$fault.err" >/dev/null
  run_install "$root" >"$top/$fault-retry.out"
  sudo jq -e '.phase=="complete" and .liveJoinBlocked==false and .secret.decodedBytes==32 and .socket.path=="/run/blazn/microk8s-worker-issuer.sock"' "$root/ownership/issuer.json" >/dev/null
  before=$(sudo sha256sum "$root/etc/issuer/issuer-hmac-v1" "$root/etc/issuer/config.json" "$root/usr/libexec/issuer")
  run_install "$root" >"$top/$fault-idempotent.out"
  after=$(sudo sha256sum "$root/etc/issuer/issuer-hmac-v1" "$root/etc/issuer/config.json" "$root/usr/libexec/issuer")
  [ "$before" = "$after" ] || { printf 'issuer retry changed receipt-bound material\n' >&2; exit 1; }
  [ "$(sudo stat -c '%u:%a' "$root/etc/issuer/issuer-hmac-v1")" = 0:400 ]
  [ "$(sudo sh -c 'wc -c <"$1"' sh "$root/etc/issuer/issuer-hmac-v1" | tr -d ' ')" = 43 ]
  sudo cmp "$root/etc/issuer/issuer-hmac-v1" "$root/ownership/recovery/issuer-hmac-v1"
  sudo grep -Fx 'BLAZN_NODE_BROKER_LOOPBACK=enabled' "$root/control-plane.env" >/dev/null
  sudo grep -Fx 'BLAZN_NODE_BROKER_UID=65532' "$root/control-plane.env" >/dev/null
  sudo grep -Fx 'BLAZN_NODE_BROKER_GID=65531' "$root/control-plane.env" >/dev/null
  sudo jq -e '.microk8sIssuer.materialDigest|test("^sha256:[a-f0-9]{64}$")' "$root/control-plane.json" >/dev/null
  if sudo grep -F -- "$(sudo sed -n '1p' "$root/etc/issuer/issuer-hmac-v1")" "$root/ownership/issuer.json" "$root/ownership/recovery/inventory.json" >/dev/null; then printf 'issuer secret leaked into evidence\n' >&2; exit 1; fi
  sudo mkdir -p "$root/issuer-state"; sudo chmod 0700 "$root/issuer-state"
  if [ "$fault" = complete ]; then
    sudo sh -c 'receipt=$1; tmp=$receipt.later; jq '\''.["later-reviewed-field"]={generation:2}'\'' "$receipt" >"$tmp"; chmod 0600 "$tmp"; mv -- "$tmp" "$receipt"' sh "$root/control-plane.json"
  fi
  run_rollback "$root" >/dev/null
  sudo jq -e '.phase=="rolled-back" and .rollback.retainedRecovery==true and .rollback.groupRetained==true' "$root/ownership/issuer.json" >/dev/null
  sudo test -f "$root/ownership/recovery/issuer-hmac-v1"
  sudo test ! -e "$root/usr/libexec/issuer"
  sudo test ! -e "$root/issuer-state"
  [ "$(sudo sed -n '1,2p' "$root/control-plane.env")" = 'BASELINE=value
BLAZN_NODE_BROKER_LOOPBACK=disabled' ]
  sudo jq -e 'has("microk8sIssuer")|not' "$root/control-plane.json" >/dev/null
  if [ "$fault" = complete ]; then sudo jq -e '.["later-reviewed-field"]=={generation:2}' "$root/control-plane.json" >/dev/null; fi
done

for fault in journal-created upgrade-initialized upgrade-service-stopped upgrade-binary-installed upgrade-receipt-updated upgrade-main-bound upgrade-service-started complete; do
  root=$top/upgrade-$fault; make_blocked_fixture "$root"
  recovery_before=$(sudo sha256sum "$root/ownership/recovery/inventory.json" "$root/ownership/recovery/issuer-hmac-v1" "$root/ownership/recovery/control-plane.env" "$root/ownership/recovery/control-plane.json")
  if run_upgrade "$root" "$fault" >"$top/upgrade-$fault.out" 2>"$top/upgrade-$fault.err"; then printf 'issuer upgrade fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected issuer upgrade fault after $fault" "$top/upgrade-$fault.err" >/dev/null
  run_upgrade "$root" >"$top/upgrade-$fault-retry.out"
  recovery_after=$(sudo sha256sum "$root/ownership/recovery/inventory.json" "$root/ownership/recovery/issuer-hmac-v1" "$root/ownership/recovery/control-plane.env" "$root/ownership/recovery/control-plane.json")
  [ "$recovery_before" = "$recovery_after" ] || { printf 'issuer upgrade changed recovery material\n' >&2; exit 1; }
  sudo jq -e --arg new "sha256:$(sha256sum "$observed_helper"|awk '{print $1}')" '.phase=="complete" and .liveJoinBlocked==false and .binary.digest==$new and .upgrade.resultBinaryDigest==$new' "$root/ownership/issuer.json" >/dev/null
  sudo jq -e '.schemaVersion=="blazn.dev/microk8s-worker-issuer-observation-upgrade/v1" and .priorBinaryDigest!=.resultBinaryDigest and .priorMaterialDigest!=.resultMaterialDigest and .priorMainDigest!=.resultMainDigest' "$root/ownership/recovery/observation-upgrade-active/journal.json" >/dev/null
  main_binding=$(sudo jq -er .microk8sIssuer.materialDigest "$root/control-plane.json")
  receipt_binding=$(sudo jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$root/ownership/issuer.json"|sha256sum|awk '{print "sha256:"$1}')
  [ "$main_binding" = "$receipt_binding" ] || { printf 'main receipt does not bind upgraded issuer\n' >&2; exit 1; }
  before=$(sudo sha256sum "$root/ownership/issuer.json" "$root/control-plane.json" "$root/usr/libexec/issuer")
  run_upgrade "$root" >/dev/null
  after=$(sudo sha256sum "$root/ownership/issuer.json" "$root/control-plane.json" "$root/usr/libexec/issuer")
  [ "$before" = "$after" ] || { printf 'completed issuer upgrade is not idempotent\n' >&2; exit 1; }
  if [ "$fault" = complete ]; then
    sudo mkdir -p "$root/issuer-state"; sudo chmod 0700 "$root/issuer-state"
    run_rollback "$root" >/dev/null
    sudo jq -e '.phase=="rolled-back" and .rollback.retainedRecovery==true' "$root/ownership/issuer.json" >/dev/null
    sudo test -f "$root/ownership/recovery/observation-upgrade-active/journal.json"
  fi
done

archive_collision=$top/upgrade-archive-collision; make_blocked_fixture "$archive_collision"
for collision_attempt in 1 2; do
  if run_upgrade "$archive_collision" '' 1 "$observed_helper" '2026-08-30T12:00:00Z' >"$top/archive-collision-$collision_attempt.out" 2>"$top/archive-collision-$collision_attempt.err"; then printf 'same-time failed issuer upgrade unexpectedly completed\n' >&2; exit 1; fi
  grep -F 'prior binary and receipt bindings were restored' "$top/archive-collision-$collision_attempt.err" >/dev/null
done
[ "$(sudo find "$archive_collision/ownership/recovery" -maxdepth 1 -type d -name 'observation-upgrade-failed-*' | wc -l | tr -d ' ')" = 2 ] || { printf 'same-time issuer failures did not create distinct archives\n' >&2; exit 1; }
run_upgrade "$archive_collision" '' 0 "$corrected_helper" '2026-08-30T12:00:00Z' >/dev/null
sudo jq -e --arg digest "sha256:$(sha256sum "$corrected_helper" | awk '{print $1}')" '.phase=="complete" and .liveJoinBlocked==false and .binary.digest==$digest' "$archive_collision/ownership/issuer.json" >/dev/null

failed_start=$top/upgrade-failed-start; make_blocked_fixture "$failed_start"
prior_failed_start=$(sudo sha256sum "$failed_start/ownership/issuer.json" "$failed_start/control-plane.json" "$failed_start/usr/libexec/issuer")
if run_upgrade "$failed_start" '' 1 >"$top/upgrade-failed-start.out" 2>"$top/upgrade-failed-start.err"; then printf 'issuer upgrade with failed service start unexpectedly completed\n' >&2; exit 1; fi
grep -F 'prior binary and receipt bindings were restored' "$top/upgrade-failed-start.err" >/dev/null
after_failed_start=$(sudo sha256sum "$failed_start/ownership/issuer.json" "$failed_start/control-plane.json" "$failed_start/usr/libexec/issuer")
[ "$prior_failed_start" = "$after_failed_start" ] || { printf 'failed issuer upgrade did not restore exact prior state\n' >&2; exit 1; }
sudo jq -e '.phase=="complete" and .liveJoinBlocked==true and (has("upgrade")|not)' "$failed_start/ownership/issuer.json" >/dev/null
sudo find "$failed_start/ownership/recovery" -maxdepth 2 -type f -name prior-binary | grep . >/dev/null
run_upgrade "$failed_start" >/dev/null
sudo jq -e '.phase=="complete" and .liveJoinBlocked==false' "$failed_start/ownership/issuer.json" >/dev/null

for rollback_fault in upgrade-rollback-started upgrade-rollback-binary-restored upgrade-rollback-main-restored upgrade-rollback-receipt-restored; do
  rollback_root=$top/$rollback_fault; make_blocked_fixture "$rollback_root"
  if run_upgrade "$rollback_root" "$rollback_fault" 1 >"$top/$rollback_fault.out" 2>"$top/$rollback_fault.err"; then printf 'issuer rollback fault unexpectedly completed: %s\n' "$rollback_fault" >&2; exit 1; fi
  grep -F "injected issuer upgrade fault after $rollback_fault" "$top/$rollback_fault.err" >/dev/null
  if ! run_upgrade "$rollback_root" '' 0 "$corrected_helper" >"$top/$rollback_fault-reconcile.out" 2>"$top/$rollback_fault-reconcile.err"; then
    run_upgrade "$rollback_root" '' 0 "$corrected_helper" >"$top/$rollback_fault-corrected.out"
  fi
  corrected_digest=sha256:$(sha256sum "$corrected_helper" | awk '{print $1}')
  sudo jq -e --arg digest "$corrected_digest" '.phase=="complete" and .liveJoinBlocked==false and .binary.digest==$digest' "$rollback_root/ownership/issuer.json" >/dev/null
  [ "sha256:$(sudo sha256sum "$rollback_root/usr/libexec/issuer" | awk '{print $1}')" = "$corrected_digest" ]
  sudo find "$rollback_root/ownership/recovery" -maxdepth 1 -type d -name 'observation-upgrade-failed-*' | grep . >/dev/null
done

conflict=$top/upgrade-main-conflict; make_blocked_fixture "$conflict"
sudo sh -c 'tmp=$1.tmp; jq ".unreviewed=true" "$1" >"$tmp"; chmod 0600 "$tmp"; mv -- "$tmp" "$1"' sh "$conflict/control-plane.json"
if run_upgrade "$conflict" >"$top/upgrade-main-conflict.out" 2>"$top/upgrade-main-conflict.err"; then printf 'issuer upgrade accepted an unbound main receipt\n' >&2; exit 1; fi
grep -F 'main receipt does not bind blocked issuer' "$top/upgrade-main-conflict.err" >/dev/null
sudo jq -e '.phase=="complete" and .liveJoinBlocked==true' "$conflict/ownership/issuer.json" >/dev/null

mutable_source=$top/mutable-source
mkdir -p "$mutable_source/scripts" "$mutable_source/systemd" "$mutable_source/install/ownership"
cp "$INSTALLER" "$mutable_source/scripts/install-worker-issuer.sh"
cp "$TEST_DIR/../systemd/blazn-microk8s-worker-issuer.service" "$mutable_source/systemd/blazn-microk8s-worker-issuer.service"
cp "$TEST_DIR/../systemd/blazn-microk8s-worker-issuer.tmpfiles" "$mutable_source/systemd/blazn-microk8s-worker-issuer.tmpfiles"
printf '\n# unreviewed replacement\n' >>"$mutable_source/systemd/blazn-microk8s-worker-issuer.service"
printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$mutable_source/install/control-plane.env"
printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$mutable_source/install/control-plane.json"
: >"$mutable_source/install/systemctl.log"
sudo chown -R 0:0 "$mutable_source/install"
sudo chmod 0700 "$mutable_source/install" "$mutable_source/install/ownership"
sudo chmod 0600 "$mutable_source/install/control-plane.env" "$mutable_source/install/control-plane.json"
if run_install "$mutable_source/install" '' "$mutable_source/scripts/install-worker-issuer.sh" >"$top/mutable-source.out" 2>"$top/mutable-source.err"; then
  printf 'issuer install accepted an unreviewed privileged source artifact\n' >&2
  exit 1
fi
grep -F 'issuer artifact source differs from reviewed digest' "$top/mutable-source.err" >/dev/null

unsafe_parent=$top/unsafe-parent
mkdir -p "$unsafe_parent/ownership" "$unsafe_parent/usr/libexec"
printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$unsafe_parent/control-plane.env"
printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$unsafe_parent/control-plane.json"
: >"$unsafe_parent/systemctl.log"
sudo chown -R 0:0 "$unsafe_parent"
sudo chmod 0700 "$unsafe_parent" "$unsafe_parent/ownership"
sudo chmod 0600 "$unsafe_parent/control-plane.env" "$unsafe_parent/control-plane.json"
sudo chmod 0777 "$unsafe_parent/usr/libexec"
if run_install "$unsafe_parent" >"$top/unsafe-parent.out" 2>"$top/unsafe-parent.err"; then
  printf 'issuer install accepted an unsafe managed parent\n' >&2
  exit 1
fi
grep -F 'managed issuer parent owner or mode is unsafe' "$top/unsafe-parent.err" >/dev/null

unsafe_state_parent=$top/unsafe-state-parent
mkdir -p "$unsafe_state_parent/ownership"
printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$unsafe_state_parent/control-plane.env"
printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$unsafe_state_parent/control-plane.json"
: >"$unsafe_state_parent/systemctl.log"
sudo chown -R 0:0 "$unsafe_state_parent"
sudo chmod 0777 "$unsafe_state_parent"
sudo chmod 0700 "$unsafe_state_parent/ownership"
sudo chmod 0600 "$unsafe_state_parent/control-plane.env" "$unsafe_state_parent/control-plane.json"
if run_install "$unsafe_state_parent" >"$top/unsafe-state-parent.out" 2>"$top/unsafe-state-parent.err"; then printf 'issuer install accepted an unsafe state parent\n' >&2; exit 1; fi
grep -F 'issuer state parent is unsafe' "$top/unsafe-state-parent.err" >/dev/null

unsafe_state=$top/unsafe-state
mkdir -p "$unsafe_state/ownership"
printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$unsafe_state/control-plane.env"
printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$unsafe_state/control-plane.json"
: >"$unsafe_state/systemctl.log"
sudo chown -R 0:0 "$unsafe_state"
sudo chmod 0700 "$unsafe_state" "$unsafe_state/ownership"
sudo chmod 0600 "$unsafe_state/control-plane.env" "$unsafe_state/control-plane.json"
run_install "$unsafe_state" >/dev/null
sudo mkdir -p "$unsafe_state/issuer-state"
sudo chmod 0777 "$unsafe_state/issuer-state"
if run_rollback "$unsafe_state" >"$top/unsafe-state.out" 2>"$top/unsafe-state.err"; then printf 'issuer rollback accepted an unsafe state root\n' >&2; exit 1; fi
grep -F 'issuer state root is unsafe' "$top/unsafe-state.err" >/dev/null
sudo chmod 0700 "$unsafe_state/issuer-state"
run_rollback "$unsafe_state" >/dev/null

for fault in service-stopped-before-phase rollback-validated-before-phase binary-removed-before-phase config-removed-before-phase secret-removed-before-phase unit-removed-before-phase tmpfiles-removed-before-phase state-removed-before-phase environment-restored-before-phase main-removal-intent-before-phase main-removal-intent main-restored-before-phase files-restored-before-phase rolled-back-before-phase; do
  root=$top/rollback-$fault; mkdir -p "$root/ownership"; printf 'BASELINE=value\nBLAZN_NODE_BROKER_LOOPBACK=disabled\n' >"$root/control-plane.env"; printf '{"schemaVersion":"blazn.dev/control-plane-ownership/v1","owner":"blazn-poc"}\n' >"$root/control-plane.json"; : >"$root/systemctl.log"; sudo chown -R 0:0 "$root"; sudo chmod 0700 "$root" "$root/ownership"; sudo chmod 0600 "$root/control-plane.env" "$root/control-plane.json"; run_install "$root" >/dev/null; sudo mkdir -p "$root/issuer-state"; sudo chmod 0700 "$root/issuer-state"
  if run_rollback "$root" "$fault" >"$top/rollback-$fault.out" 2>"$top/rollback-$fault.err"; then printf 'rollback fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected issuer rollback fault after $fault" "$top/rollback-$fault.err" >/dev/null
  run_rollback "$root" >/dev/null; sudo jq -e '.phase=="rolled-back"' "$root/ownership/issuer.json" >/dev/null; sudo test ! -e "$root/issuer-state"; sudo jq -e 'has("microk8sIssuer")|not' "$root/control-plane.json" >/dev/null
done

grep -F 'source: /run/blazn/microk8s-worker-issuer.sock' "$COMPOSE" >/dev/null
grep -F 'target: /run/blazn/microk8s-worker-issuer.sock' "$COMPOSE" >/dev/null
grep -F 'network_mode: "service:api"' "$COMPOSE" >/dev/null
# shellcheck disable=SC2016
grep -F 'BLAZN_NODE_BROKER_LOOPBACK: ${BLAZN_NODE_BROKER_LOOPBACK:-disabled}' "$COMPOSE" >/dev/null
grep -A2 -F '      api:' "$COMPOSE" | grep -F 'condition: service_started' >/dev/null
broker_body=$(sed -n '/  node-broker:/,/^  [a-z]/p' "$COMPOSE")
printf '%s\n' "$broker_body" | grep -F "fetch('http://127.0.0.1:8081/healthz')" >/dev/null
if sed -n '/  node-broker:/,/^  [a-z]/p' "$COMPOSE" | grep -E '/var/snap/microk8s|kubeconfig|docker.sock' >/dev/null; then printf 'node broker has an unreviewed host capability\n' >&2; exit 1; fi
if sed -n '/  api:/,/^  [a-z]/p' "$COMPOSE" | grep -E 'node_broker_database_url|node_broker_join_credential|issuer-hmac|microk8s-worker-issuer.sock' >/dev/null; then printf 'API container received a broker/provider secret\n' >&2; exit 1; fi
for script in start-control-plane.sh run-control-plane.sh stop-control-plane.sh; do grep -F -- '--profile node-broker' "$TEST_DIR/../../milestone-2/scripts/$script" >/dev/null; done
for script in start-control-plane.sh run-control-plane.sh; do grep -F 'running healthy' "$TEST_DIR/../../milestone-2/scripts/$script" >/dev/null; done
unit=$TEST_DIR/../systemd/blazn-microk8s-worker-issuer.service
tmpfiles_policy=$TEST_DIR/../systemd/blazn-microk8s-worker-issuer.tmpfiles
grep -Fx 'ReadWritePaths=/run/blazn /var/lib/blazn-node-root/microk8s-worker-issuer /var/snap/microk8s/current/credentials' "$unit" >/dev/null
grep -Fx 'd /var/lib/blazn-node-root/microk8s-worker-issuer 0700 root root -' "$tmpfiles_policy" >/dev/null
# shellcheck disable=SC2016
grep -F 'STATE_ROOT=${BLAZN_ISSUER_STATE_ROOT:-/var/lib/blazn-node-root/microk8s-worker-issuer}' "$ROLLBACK" >/dev/null
for policy in 'CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER' 'ProtectKernelTunables=true' 'ProtectKernelModules=true' 'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK' 'RestrictSUIDSGID=true'; do grep -Fx "$policy" "$unit" >/dev/null || { printf 'issuer systemd hardening is missing: %s\n' "$policy" >&2; exit 1; }; done
trap - EXIT HUP INT TERM
cleanup
printf 'worker issuer journal, secret encoding, and narrow Compose boundary passed\n'
