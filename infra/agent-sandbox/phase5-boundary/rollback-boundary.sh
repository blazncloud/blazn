#!/bin/sh
set -eu

# UID-fenced rollback of a Phase 5 boundary transaction. Deletes only the
# exact objects recorded by install-boundary.sh, then proves absence.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the boundary rollback must run as root\n' >&2; exit 1; }
[ "$#" -eq 0 ] || { printf 'usage: %s\n' "$0" >&2; exit 64; }
phase4c_require_mutation_authority
: "${BLAZN_PHASE5_TRANSACTION_DIR:?set the transaction directory to roll back}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the transaction UUID}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
transaction=$BLAZN_PHASE5_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase5/boundary-*) ;; *) printf 'boundary transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase5/}
case "$transaction_name" in */*|*..*|'') printf 'boundary transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'boundary transaction directory is unsafe\n' >&2; exit 1; fi

write_phase() { phase4c_write_phase "$transaction" "$1"; }
absent() {
  if [ "$#" -eq 3 ]; then discovered=$(kubectl get "$1" "$2" -n "$3" --ignore-not-found -o name) || return 2; else discovered=$(kubectl get "$1" "$2" --ignore-not-found -o name) || return 2; fi
  [ -z "$discovered" ]
}
phase=$(cat "$transaction/phase")
case "$phase" in
  sealed|apply-intent) write_phase rollback-complete; printf 'boundary transaction rolled back before any apply\n'; exit 0 ;;
  applied|complete|rollback-intent) ;;
  rollback-complete) printf 'boundary transaction already rolled back\n'; exit 0 ;;
  *) printf 'boundary transaction phase is invalid\n' >&2; exit 1 ;;
esac
uids=$transaction/owned-uids.json
if [ ! -f "$uids" ]; then
  if [ "$phase" = applied ]; then
    printf 'apply began but owned identities were never recorded; reconcile by hand with the transaction annotation before rolling back\n' >&2
    exit 1
  fi
  printf 'owned identities are missing\n' >&2; exit 1
fi
write_phase rollback-intent

phase4c_start_uid_proxy "$transaction"
trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM
delete_owned() {
  owned_key=$1; api_path=$2
  owned_uid_value=$(jq -er --arg key "$owned_key" '.[$key]' "$uids")
  phase4c_delete_uid "$api_path" "$owned_uid_value" Foreground
}
if ! absent validatingadmissionpolicybinding blazn-sandbox-boundary; then delete_owned validatingadmissionpolicybinding/blazn-sandbox-boundary /apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/blazn-sandbox-boundary; fi
if ! absent validatingadmissionpolicy blazn-sandbox-boundary; then delete_owned validatingadmissionpolicy/blazn-sandbox-boundary /apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/blazn-sandbox-boundary; fi
for doomed_namespace in blazn-poc-sandboxes blazn-poc-system; do
  if ! absent namespace "$doomed_namespace"; then
    if ! pod_listing=$(kubectl get pods -n "$doomed_namespace" --no-headers 2>/dev/null); then printf 'Pod discovery in %s failed; refusing rollback\n' "$doomed_namespace" >&2; exit 1; fi
    remaining_pods=$(printf '%s' "$pod_listing" | grep -c . || :)
    [ "$remaining_pods" = 0 ] || { printf 'namespace %s still runs Pods; refusing rollback\n' "$doomed_namespace" >&2; exit 1; }
    if sandbox_listing=$(kubectl get sandboxes.agents.x-k8s.io -n "$doomed_namespace" --no-headers 2>/dev/null); then
      remaining_sandboxes=$(printf '%s' "$sandbox_listing" | grep -c . || :)
    elif absent crd sandboxes.agents.x-k8s.io; then
      remaining_sandboxes=0
    else
      printf 'Sandbox discovery in %s failed; refusing rollback\n' "$doomed_namespace" >&2; exit 1
    fi
    [ "$remaining_sandboxes" = 0 ] || { printf 'namespace %s still holds Sandboxes; refusing rollback\n' "$doomed_namespace" >&2; exit 1; }
    delete_owned "namespace/$doomed_namespace" "/api/v1/namespaces/$doomed_namespace"
  fi
done
phase4c_stop_uid_proxy
trap - EXIT HUP INT TERM

for gone in validatingadmissionpolicybinding/blazn-sandbox-boundary validatingadmissionpolicy/blazn-sandbox-boundary namespace/blazn-poc-sandboxes namespace/blazn-poc-system; do
  gone_kind=${gone%%/*}; gone_name=${gone#*/}
  attempt=0
  until absent "$gone_kind" "$gone_name"; do
    attempt=$((attempt + 1))
    [ "$attempt" -le 60 ] || { printf '%s was not removed\n' "$gone" >&2; exit 1; }
    sleep 2
  done
done
write_phase rollback-complete
printf 'Phase 5 boundary rolled back to zero residue\n'
