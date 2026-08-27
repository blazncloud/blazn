#!/bin/sh
set -eu

# UID-fenced rollback of a Phase 5 installation transaction. Refuses while
# any Sandbox object exists, deletes only the recorded identities, and
# proves absence before declaring completion.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the Phase 5 rollback must run as root\n' >&2; exit 1; }
[ "$#" -eq 0 ] || { printf 'usage: %s\n' "$0" >&2; exit 64; }
phase4c_require_mutation_authority
: "${BLAZN_PHASE5_TRANSACTION_DIR:?set the transaction directory to roll back}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the transaction UUID}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
transaction=$BLAZN_PHASE5_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase5/install-*) ;; *) printf 'install transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase5/}
case "$transaction_name" in */*|*..*|'') printf 'install transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'install transaction directory is unsafe\n' >&2; exit 1; fi

write_phase() { phase4c_write_phase "$transaction" "$1"; }
absent() { discovered=$(kubectl get "$1" "$2" --ignore-not-found -o name) || return 2; [ -z "$discovered" ]; }
phase=$(cat "$transaction/phase")
case "$phase" in
  sealed|install-intent) write_phase rollback-complete; printf 'installation rolled back before any apply\n'; exit 0 ;;
  install-applied|bootstrap-intent|bootstrap-applied|bootstrap-complete|complete|rollback-intent) ;;
  rollback-complete) printf 'installation already rolled back\n'; exit 0 ;;
  *) printf 'install transaction phase is invalid\n' >&2; exit 1 ;;
esac
for populated_crd in sandboxes.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do
  if ! absent crd "$populated_crd"; then
    remaining=$(kubectl get "$populated_crd" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')
    [ "$remaining" = 0 ] || { printf '%s objects still exist; refusing rollback\n' "$populated_crd" >&2; exit 1; }
  fi
done
write_phase rollback-intent

phase4c_start_uid_proxy "$transaction"
trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM
delete_if_owned() {
  target_kind=$1; target_name=$2; target_path=$3
  if absent "$target_kind" "$target_name"; then return 0; fi
  target_uid=$(kubectl get "$target_kind" "$target_name" -o json | jq -er --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" 'select(.metadata.annotations["blazn.dev/phase5-transaction"] == $tx) | .metadata.uid') || { printf '%s/%s exists without this transaction identity; refusing\n' "$target_kind" "$target_name" >&2; exit 1; }
  phase4c_delete_uid "$target_path" "$target_uid"
}
delete_if_owned clusterrolebinding blazn-agent-sandbox-ca-bootstrap /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-agent-sandbox-ca-bootstrap
delete_if_owned clusterrole blazn-agent-sandbox-ca-bootstrap /apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-agent-sandbox-ca-bootstrap
delete_if_owned clusterrolebinding blazn-agent-sandbox-observer /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-agent-sandbox-observer
delete_if_owned clusterrole blazn-agent-sandbox-observer /apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-agent-sandbox-observer
delete_if_owned namespace agent-sandbox-system /api/v1/namespaces/agent-sandbox-system
for doomed_crd in sandboxclaims.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxes.agents.x-k8s.io; do
  delete_if_owned crd "$doomed_crd" "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/$doomed_crd"
done
phase4c_stop_uid_proxy
trap - EXIT HUP INT TERM

for gone in namespace/agent-sandbox-system clusterrole/blazn-agent-sandbox-observer clusterrolebinding/blazn-agent-sandbox-observer clusterrole/blazn-agent-sandbox-ca-bootstrap clusterrolebinding/blazn-agent-sandbox-ca-bootstrap crd/sandboxes.agents.x-k8s.io crd/sandboxclaims.extensions.agents.x-k8s.io crd/sandboxtemplates.extensions.agents.x-k8s.io crd/sandboxwarmpools.extensions.agents.x-k8s.io; do
  gone_kind=${gone%%/*}; gone_name=${gone#*/}
  attempt=0
  until absent "$gone_kind" "$gone_name"; do
    attempt=$((attempt + 1))
    [ "$attempt" -le 60 ] || { printf '%s was not removed\n' "$gone" >&2; exit 1; }
    sleep 2
  done
done
write_phase rollback-complete
printf 'Phase 5 installation rolled back to zero residue\n'
