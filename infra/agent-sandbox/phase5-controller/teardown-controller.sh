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
owned_uid() {
  if [ "$3" = - ]; then kubectl get "$1" "$2" -o json
  else kubectl get "$1" "$2" -n "$3" -o json
  fi | jq -er --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" 'select(.metadata.annotations["blazn.dev/phase5-transaction"] == $tx) | .metadata.uid'
}
recover_inventory() {
  entries=$transaction/.owned-entries.jsonl
  : >"$entries"
  for object in deployment/blazn-sandbox-controller:blazn-poc-system service/blazn-sandbox-access:blazn-poc-system serviceaccount/blazn-sandbox-controller:blazn-poc-system role/blazn-sandbox-controller:blazn-poc-sandboxes rolebinding/blazn-sandbox-controller:blazn-poc-sandboxes clusterrole/blazn-sandbox-controller-node-observer:- clusterrolebinding/blazn-sandbox-controller-node-observer:- networkpolicy/blazn-sandbox-controller-access-ingress:blazn-poc-system networkpolicy/blazn-sandbox-controller-egress:blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny:blazn-poc-system; do
    ref=${object%%:*}; ns=${object#*:}; kind=${ref%%/*}; name=${ref#*/}
    absent "$kind" "$name" "$ns" && continue
    uid=$(owned_uid "$kind" "$name" "$ns") || { rm -f "$entries"; printf 'existing controller object is not owned by this transaction: %s\n' "$ref" >&2; exit 1; }
    jq -cn --arg key "$ref" --arg value "$uid" '{key:$key,value:$value}' >>"$entries"
  done
  jq -s 'from_entries' "$entries" >"$uids.tmp"
  rm -f "$entries"
  mv "$uids.tmp" "$uids"; chmod 0600 "$uids"
}
phase=$(cat "$transaction/phase")
case "$phase" in
  sealed) write_phase rollback-complete; printf 'controller transaction rolled back before any apply\n'; exit 0 ;;
  apply-intent) ;;
  applied|scaled|complete|rollback-intent) ;;
  rollback-complete) printf 'controller transaction already rolled back\n'; exit 0 ;;
  *) printf 'controller transaction phase is invalid\n' >&2; exit 1 ;;
esac
uids=$transaction/owned-uids.json
if [ "$phase" = apply-intent ] && [ ! -f "$uids" ]; then
  # Apply may have completed before the process crashed. Recover only objects
  # carrying this transaction's sealed annotation; an unowned same-name object
  # fails closed and is never deleted.
  recover_inventory
  if [ "$(jq 'length' "$uids")" -eq 0 ]; then write_phase rollback-complete; printf 'controller transaction rolled back before any apply\n'; exit 0; fi
fi
# The install transaction records every owned UID immediately after apply, so
# any post-apply phase must carry the identity file.
[ -f "$uids" ] || { printf 'owned controller identities are missing; reconcile the transaction by hand\n' >&2; exit 1; }

# Scale to zero first so no controller Pod is reconciling while its RBAC and
# egress are removed.
if ! absent deployment blazn-sandbox-controller blazn-poc-system; then
  kubectl scale deployment blazn-sandbox-controller -n blazn-poc-system --replicas=0 >/dev/null
  attempt=0
  until [ "$(kubectl get pods -n blazn-poc-system -l app.kubernetes.io/name=blazn-sandbox-controller --no-headers 2>/dev/null | grep -c . || :)" = 0 ]; do
    attempt=$((attempt + 1)); [ "$attempt" -le 30 ] || { printf 'controller Pods did not drain\n' >&2; exit 1; }; sleep 2
  done
fi
write_phase rollback-intent

phase4c_start_uid_proxy "$transaction"
trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM
# Each delete is guarded by an absence pre-check so a resume after a partial
# teardown skips the already-removed objects instead of aborting on a 404.
delete_owned() {
  owned_kind=$1; owned_name=$2; owned_ns=$3; owned_key=$4; owned_path=$5
  absent "$owned_kind" "$owned_name" "$owned_ns" && return 0
  owned_uid=$(jq -er --arg key "$owned_key" '.[$key] // empty' "$uids") || return 0
  [ -n "$owned_uid" ] || return 0
  phase4c_delete_uid "$owned_path" "$owned_uid" Background
}
delete_owned deployment blazn-sandbox-controller blazn-poc-system deployment/blazn-sandbox-controller /apis/apps/v1/namespaces/blazn-poc-system/deployments/blazn-sandbox-controller
delete_owned service blazn-sandbox-access blazn-poc-system service/blazn-sandbox-access /api/v1/namespaces/blazn-poc-system/services/blazn-sandbox-access
delete_owned networkpolicy blazn-sandbox-controller-access-ingress blazn-poc-system networkpolicy/blazn-sandbox-controller-access-ingress /apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-access-ingress
delete_owned networkpolicy blazn-sandbox-controller-egress blazn-poc-system networkpolicy/blazn-sandbox-controller-egress /apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-egress
delete_owned networkpolicy blazn-sandbox-controller-default-deny blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny /apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-default-deny
delete_owned clusterrolebinding blazn-sandbox-controller-node-observer - clusterrolebinding/blazn-sandbox-controller-node-observer /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-sandbox-controller-node-observer
delete_owned clusterrole blazn-sandbox-controller-node-observer - clusterrole/blazn-sandbox-controller-node-observer /apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-sandbox-controller-node-observer
delete_owned rolebinding blazn-sandbox-controller blazn-poc-sandboxes rolebinding/blazn-sandbox-controller /apis/rbac.authorization.k8s.io/v1/namespaces/blazn-poc-sandboxes/rolebindings/blazn-sandbox-controller
delete_owned role blazn-sandbox-controller blazn-poc-sandboxes role/blazn-sandbox-controller /apis/rbac.authorization.k8s.io/v1/namespaces/blazn-poc-sandboxes/roles/blazn-sandbox-controller
delete_owned serviceaccount blazn-sandbox-controller blazn-poc-system serviceaccount/blazn-sandbox-controller /api/v1/namespaces/blazn-poc-system/serviceaccounts/blazn-sandbox-controller
phase4c_stop_uid_proxy
trap - EXIT HUP INT TERM

for gone in deployment/blazn-sandbox-controller:blazn-poc-system service/blazn-sandbox-access:blazn-poc-system serviceaccount/blazn-sandbox-controller:blazn-poc-system role/blazn-sandbox-controller:blazn-poc-sandboxes rolebinding/blazn-sandbox-controller:blazn-poc-sandboxes clusterrole/blazn-sandbox-controller-node-observer:- clusterrolebinding/blazn-sandbox-controller-node-observer:- networkpolicy/blazn-sandbox-controller-access-ingress:blazn-poc-system networkpolicy/blazn-sandbox-controller-egress:blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny:blazn-poc-system; do
  gone_ref=${gone%%:*}; gone_ns=${gone#*:}
  gone_kind=${gone_ref%%/*}; gone_name=${gone_ref#*/}
  attempt=0
  until absent "$gone_kind" "$gone_name" "$gone_ns"; do
    attempt=$((attempt + 1)); [ "$attempt" -le 60 ] || { printf '%s was not removed\n' "$gone" >&2; exit 1; }; sleep 2
  done
done
write_phase rollback-complete
printf 'Phase 5 sandbox controller torn down to zero residue\n'
