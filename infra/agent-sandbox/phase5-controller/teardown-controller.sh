#!/bin/sh
set -eu

# UID-fenced teardown of a Phase 5 controller deployment transaction. Scales
# the controller to zero, then deletes only the recorded controller
# identities (never the shared namespaces or Secrets) and proves absence.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the controller teardown must run as root\n' >&2; exit 1; }
[ "$#" -eq 0 ] || { printf 'usage: %s\n' "$0" >&2; exit 64; }
phase4c_require_mutation_authority
: "${BLAZN_CONTROLLER_TRANSACTION_DIR:?set the controller transaction directory to tear down}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the transaction UUID}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
transaction=$BLAZN_CONTROLLER_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase5/controller-*) ;; *) printf 'controller transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase5/}
case "$transaction_name" in */*|*..*|'') printf 'controller transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'controller transaction directory is unsafe\n' >&2; exit 1; fi

write_phase() { phase4c_write_phase "$transaction" "$1"; }
absent() {
  if [ "$3" = - ]; then discovered=$(kubectl get "$1" "$2" --ignore-not-found -o name) || return 2
  else discovered=$(kubectl get "$1" "$2" -n "$3" --ignore-not-found -o name) || return 2
  fi
  [ -z "$discovered" ]
}
current_uid() {
  if [ "$3" = - ]; then kubectl get "$1" "$2" -o json 2>/dev/null
  else kubectl get "$1" "$2" -n "$3" -o json 2>/dev/null
  fi | jq -er '.metadata.uid' 2>/dev/null
}
validate_uid_journal() {
  [ -f "$uids" ] && [ ! -L "$uids" ] && [ "$(stat -c '%u:%a:%h' "$uids")" = 0:600:1 ] || { printf 'owned UID journal metadata is unsafe\n' >&2; return 1; }
  jq -e '
    ["serviceaccount/blazn-sandbox-controller","role/blazn-sandbox-controller","clusterrole/blazn-sandbox-controller-node-observer","deployment/blazn-sandbox-controller","service/blazn-sandbox-access","networkpolicy/blazn-sandbox-controller-default-deny","networkpolicy/blazn-sandbox-controller-access-ingress","networkpolicy/blazn-sandbox-controller-egress","rolebinding/blazn-sandbox-controller","clusterrolebinding/blazn-sandbox-controller-node-observer"] as $allowed |
    (to_entries) as $entries | ($entries | length) <= ($allowed | length) and
    all(range(0; ($entries | length)); $entries[.].key == $allowed[.]) and
    all($entries[]; .value | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$uids" >/dev/null || { printf 'owned UID journal schema is invalid\n' >&2; return 1; }
}
phase=$(cat "$transaction/phase")
anchor_name=blazn-phase5-anchor-$BLAZN_PHASE5_TRANSACTION_ID
anchor_record=$transaction/anchor.json
case "$phase" in
  sealed) write_phase rollback-complete; printf 'controller transaction rolled back before any apply\n'; exit 0 ;;
  anchor-intent)
    absent clusterrole "$anchor_name" - && { write_phase rollback-complete; printf 'controller transaction rolled back before anchor creation\n'; exit 0; }
    kubectl get clusterrole "$anchor_name" -o json >"$anchor_record.tmp"
    jq -e --arg name "$anchor_name" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" '
      .apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "ClusterRole" and
      .metadata.name == $name and .metadata.annotations == {"blazn.dev/phase5-transaction":$tx} and
      ((.metadata.labels // {}) == {}) and ((.metadata.ownerReferences // []) == []) and
      ((.metadata.finalizers // []) == []) and ((.aggregationRule // null) == null) and
      .rules == [] and (.metadata.uid | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$anchor_record.tmp" >/dev/null || { printf 'un-journaled transaction anchor is not the exact inert anchor; recovery is required\n' >&2; exit 1; }
    chmod 0600 "$anchor_record.tmp"; sync -f "$anchor_record.tmp"; mv "$anchor_record.tmp" "$anchor_record"; sync -f "$transaction"
    write_phase anchor-journaled; phase=anchor-journaled
    ;;
  anchor-journaled|apply-intent) ;;
  applied|scaled|complete|rollback-intent) ;;
  rollback-complete) printf 'controller transaction already rolled back\n'; exit 0 ;;
  *) printf 'controller transaction phase is invalid\n' >&2; exit 1 ;;
esac
uids=$transaction/owned-uids.json
ambiguous=$transaction/recovery-required
attempt_residual=$transaction/.recovery-required.current
: >"$attempt_residual"
resuming_rollback=0
[ "$phase" = rollback-intent ] && resuming_rollback=1
[ -f "$anchor_record" ] && [ ! -L "$anchor_record" ] && [ "$(stat -c '%u:%a:%h' "$anchor_record")" = 0:600:1 ] || { printf 'anchor journal metadata is unsafe\n' >&2; exit 1; }
jq -e --arg name "$anchor_name" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" '
  .apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "ClusterRole" and .metadata.name == $name and
  .metadata.annotations == {"blazn.dev/phase5-transaction":$tx} and ((.metadata.labels // {}) == {}) and
  ((.metadata.ownerReferences // []) == []) and ((.metadata.finalizers // []) == []) and
  ((.aggregationRule // null) == null) and .rules == [] and
  (.metadata.uid | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$anchor_record" >/dev/null || { printf 'anchor journal schema is invalid\n' >&2; exit 1; }
anchor_uid=$(jq -er '.metadata.uid' "$anchor_record") || { printf 'journaled transaction anchor identity is missing\n' >&2; exit 1; }
anchor_safe=0
if anchor_live=$(kubectl get clusterrole "$anchor_name" -o json 2>/dev/null) && printf '%s' "$anchor_live" | jq -e --arg uid "$anchor_uid" --arg name "$anchor_name" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" '
  select(.metadata.uid == $uid and .metadata.name == $name) | select(.metadata.annotations == {"blazn.dev/phase5-transaction":$tx}) |
  select((.metadata.labels // {}) == {} and (.metadata.ownerReferences // []) == [] and (.metadata.finalizers // []) == []) |
  select((.aggregationRule // null) == null and .rules == [])' >/dev/null; then
  anchor_safe=1
elif [ "$resuming_rollback" -eq 0 ] || ! absent clusterrole "$anchor_name" -; then
  printf 'clusterrole/%s\n' "$anchor_name" >>"$attempt_residual"
fi
if [ ! -f "$uids" ]; then printf '{}\n' >"$uids.tmp"; chmod 0600 "$uids.tmp"; sync -f "$uids.tmp"; mv "$uids.tmp" "$uids"; sync -f "$transaction"; fi
validate_uid_journal

# Persist intent before scale-to-zero or any delete so a crash resumes cleanup.
[ "$phase" = rollback-intent ] || { write_phase rollback-intent; phase=rollback-intent; }

# Scale to zero first so no controller Pod is reconciling while its RBAC and
# egress are removed.
deployment_uid=$(jq -r '."deployment/blazn-sandbox-controller" // empty' "$uids")
if [ -n "$deployment_uid" ] && [ "$(current_uid deployment blazn-sandbox-controller blazn-poc-system || :)" = "$deployment_uid" ]; then
  kubectl scale deployment blazn-sandbox-controller -n blazn-poc-system --replicas=0 >/dev/null
  attempt=0
  until [ "$(kubectl get pods -n blazn-poc-system -l app.kubernetes.io/name=blazn-sandbox-controller --no-headers 2>/dev/null | grep -c . || :)" = 0 ]; do
    attempt=$((attempt + 1)); [ "$attempt" -le 30 ] || { printf 'controller Pods did not drain\n' >&2; exit 1; }; sleep 2
  done
fi
phase4c_start_uid_proxy "$transaction"
trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM
# Each delete is guarded by an absence pre-check so a resume after a partial
# teardown skips the already-removed objects instead of aborting on a 404.
delete_owned() {
  owned_kind=$1; owned_name=$2; owned_ns=$3; owned_key=$4; owned_path=$5
  absent "$owned_kind" "$owned_name" "$owned_ns" && return 0
  owned_uid=$(jq -er --arg key "$owned_key" '.[$key] // empty' "$uids") || return 0
  [ -n "$owned_uid" ] || return 0
  if ! phase4c_delete_uid "$owned_path" "$owned_uid" Background; then
    live_after_error=$(current_uid "$owned_kind" "$owned_name" "$owned_ns" || :)
    [ -z "$live_after_error" ] && return 0
    if [ "$live_after_error" != "$owned_uid" ] || ! phase4c_delete_uid "$owned_path" "$owned_uid" Background; then
      printf '%s\n' "$owned_key" >>"$attempt_residual"
    fi
  fi
  return 0
}
# Revoke bindings and roles before deleting non-authority objects.
delete_owned clusterrolebinding blazn-sandbox-controller-node-observer - clusterrolebinding/blazn-sandbox-controller-node-observer /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-sandbox-controller-node-observer
delete_owned rolebinding blazn-sandbox-controller blazn-poc-sandboxes rolebinding/blazn-sandbox-controller /apis/rbac.authorization.k8s.io/v1/namespaces/blazn-poc-sandboxes/rolebindings/blazn-sandbox-controller
delete_owned clusterrole blazn-sandbox-controller-node-observer - clusterrole/blazn-sandbox-controller-node-observer /apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-sandbox-controller-node-observer
delete_owned role blazn-sandbox-controller blazn-poc-sandboxes role/blazn-sandbox-controller /apis/rbac.authorization.k8s.io/v1/namespaces/blazn-poc-sandboxes/roles/blazn-sandbox-controller
delete_owned deployment blazn-sandbox-controller blazn-poc-system deployment/blazn-sandbox-controller /apis/apps/v1/namespaces/blazn-poc-system/deployments/blazn-sandbox-controller
delete_owned service blazn-sandbox-access blazn-poc-system service/blazn-sandbox-access /api/v1/namespaces/blazn-poc-system/services/blazn-sandbox-access
delete_owned networkpolicy blazn-sandbox-controller-access-ingress blazn-poc-system networkpolicy/blazn-sandbox-controller-access-ingress /apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-access-ingress
delete_owned networkpolicy blazn-sandbox-controller-egress blazn-poc-system networkpolicy/blazn-sandbox-controller-egress /apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-egress
delete_owned networkpolicy blazn-sandbox-controller-default-deny blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny /apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-default-deny
delete_owned serviceaccount blazn-sandbox-controller blazn-poc-system serviceaccount/blazn-sandbox-controller /api/v1/namespaces/blazn-poc-system/serviceaccounts/blazn-sandbox-controller
if [ "$anchor_safe" -eq 1 ]; then
  if ! phase4c_delete_uid "/apis/rbac.authorization.k8s.io/v1/clusterroles/$anchor_name" "$anchor_uid" Foreground; then
    anchor_after_error=$(current_uid clusterrole "$anchor_name" - || :)
    if [ -n "$anchor_after_error" ] && { [ "$anchor_after_error" != "$anchor_uid" ] || ! phase4c_delete_uid "/apis/rbac.authorization.k8s.io/v1/clusterroles/$anchor_name" "$anchor_uid" Foreground; }; then
      printf 'clusterrole/%s\n' "$anchor_name" >>"$attempt_residual"
    fi
  fi
fi
phase4c_stop_uid_proxy
trap - EXIT HUP INT TERM

gc_attempts=${BLAZN_CONTROLLER_GC_ATTEMPTS:-60}
case "$gc_attempts" in ''|*[!0-9]*|0) gc_attempts=60 ;; esac
attempt=0
while :; do
  pending=0
  for gone in deployment/blazn-sandbox-controller:blazn-poc-system service/blazn-sandbox-access:blazn-poc-system serviceaccount/blazn-sandbox-controller:blazn-poc-system role/blazn-sandbox-controller:blazn-poc-sandboxes rolebinding/blazn-sandbox-controller:blazn-poc-sandboxes clusterrole/blazn-sandbox-controller-node-observer:- clusterrolebinding/blazn-sandbox-controller-node-observer:- networkpolicy/blazn-sandbox-controller-access-ingress:blazn-poc-system networkpolicy/blazn-sandbox-controller-egress:blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny:blazn-poc-system; do
    gone_ref=${gone%%:*}; gone_ns=${gone#*:}; gone_kind=${gone_ref%%/*}; gone_name=${gone_ref#*/}
    expected_uid=$(jq -r --arg key "$gone_ref" '.[$key] // empty' "$uids")
    live_uid_now=$(current_uid "$gone_kind" "$gone_name" "$gone_ns" || :)
    if [ -n "$live_uid_now" ] && { [ -z "$expected_uid" ] || [ "$live_uid_now" = "$expected_uid" ]; }; then pending=1; fi
    if [ -n "$expected_uid" ] && [ -n "$live_uid_now" ] && [ "$live_uid_now" != "$expected_uid" ]; then printf '%s\n' "$gone_ref" >>"$attempt_residual"; fi
  done
  live_anchor_uid=$(current_uid clusterrole "$anchor_name" - || :)
  [ "$live_anchor_uid" = "$anchor_uid" ] && pending=1
  if [ -n "$live_anchor_uid" ] && [ "$live_anchor_uid" != "$anchor_uid" ]; then printf 'clusterrole/%s\n' "$anchor_name" >>"$attempt_residual"; fi
  [ "$pending" -eq 0 ] && break
  attempt=$((attempt + 1)); [ "$attempt" -lt "$gc_attempts" ] || break
  sleep 2
done
if [ "$pending" -ne 0 ]; then
  for gone in deployment/blazn-sandbox-controller:blazn-poc-system service/blazn-sandbox-access:blazn-poc-system serviceaccount/blazn-sandbox-controller:blazn-poc-system role/blazn-sandbox-controller:blazn-poc-sandboxes rolebinding/blazn-sandbox-controller:blazn-poc-sandboxes clusterrole/blazn-sandbox-controller-node-observer:- clusterrolebinding/blazn-sandbox-controller-node-observer:- networkpolicy/blazn-sandbox-controller-access-ingress:blazn-poc-system networkpolicy/blazn-sandbox-controller-egress:blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny:blazn-poc-system; do
    gone_ref=${gone%%:*}; gone_ns=${gone#*:}; gone_kind=${gone_ref%%/*}; gone_name=${gone_ref#*/}
    [ -z "$(current_uid "$gone_kind" "$gone_name" "$gone_ns" || :)" ] || printf '%s\n' "$gone_ref" >>"$attempt_residual"
  done
  [ "$(current_uid clusterrole "$anchor_name" - || :)" != "$anchor_uid" ] || printf 'clusterrole/%s\n' "$anchor_name" >>"$attempt_residual"
fi
if [ -s "$attempt_residual" ]; then
  sort -u "$attempt_residual" >"$ambiguous.tmp"; chmod 0600 "$ambiguous.tmp"; sync -f "$ambiguous.tmp"; mv "$ambiguous.tmp" "$ambiguous"; sync -f "$transaction"
  printf 'ambiguous replacement objects were left untouched; recovery is required:\n' >&2
  cat "$ambiguous" >&2
  exit 1
fi
write_phase rollback-complete
rm -f "$ambiguous" "$attempt_residual"
printf 'Phase 5 sandbox controller torn down to zero residue\n'
