#!/bin/sh
set -eu

# Crash-resumable, UID-fenced installation of the Phase 5 boundary. Consumes
# only a sealed transaction directory produced from render-boundary.sh output.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the boundary installation must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s RENDERED_MANIFEST\n' "$0" >&2; exit 64; }
manifest=$1
phase4c_require_mutation_authority
: "${BLAZN_PHASE5_TRANSACTION_DIR:?set a durable transaction directory}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the transaction UUID used at render time}"
: "${BLAZN_EXPECTED_BOUNDARY_SHA256:?set the separately reviewed rendered manifest digest}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
transaction=$BLAZN_PHASE5_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase5/boundary-*) ;; *) printf 'boundary transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase5/}
case "$transaction_name" in */*|*..*|'') printf 'boundary transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac

write_phase() { phase4c_write_phase "$transaction" "$1"; }
namespace_absent() { discovered=$(kubectl get namespace "$1" --ignore-not-found -o name) || return 2; [ -z "$discovered" ]; }
object_absent() { discovered=$(kubectl get "$1" "$2" --ignore-not-found -o name) || return 2; [ -z "$discovered" ]; }
owned_uid() {
  if [ "$#" -eq 3 ]; then kubectl get "$1" "$2" -n "$3" -o json; else kubectl get "$1" "$2" -o json; fi |
    jq -er --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" 'select(.metadata.annotations["blazn.dev/phase5-transaction"] == $tx) | .metadata.uid'
}

if [ ! -e "$transaction" ]; then
  if ! { [ -f "$manifest" ] && [ ! -L "$manifest" ] && [ "$(stat -c '%h' "$manifest")" = 1 ]; }; then printf 'rendered boundary manifest is unsafe\n' >&2; exit 1; fi
  install -d -o root -g root -m 0700 /var/lib/blazn/phase5 2>/dev/null || :
  install -d -o root -g root -m 0700 "$transaction"
  install -o root -g root -m 0400 "$manifest" "$transaction/boundary.yaml"
  write_phase sealed
fi
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'boundary transaction directory is unsafe\n' >&2; exit 1; fi
sealed=$transaction/boundary.yaml
if [ -L "$sealed" ] || [ ! -f "$sealed" ] || [ "$(stat -c '%u:%a:%h' "$sealed")" != 0:400:1 ]; then printf 'sealed boundary manifest is unsafe\n' >&2; exit 1; fi
[ "$(sha256sum "$sealed" | awk '{print $1}')" = "$BLAZN_EXPECTED_BOUNDARY_SHA256" ] || { printf 'sealed boundary manifest digest mismatch\n' >&2; exit 1; }
[ "$(grep -c "blazn.dev/phase5-transaction: $BLAZN_PHASE5_TRANSACTION_ID" "$sealed")" -ge 8 ] || { printf 'sealed manifest does not carry this transaction identity\n' >&2; exit 1; }
phase=$(cat "$transaction/phase")
case "$phase" in
  complete) printf 'Phase 5 boundary transaction is already complete\n'; exit 0 ;;
  rollback-complete) printf 'Phase 5 boundary transaction was rolled back; use a new transaction\n' >&2; exit 1 ;;
  sealed|apply-intent|applied) ;;
  *) printf 'boundary transaction phase is invalid\n' >&2; exit 1 ;;
esac

server_minor=$(kubectl version -o json | jq -er '.serverVersion.minor | gsub("[^0-9]"; "")')
server_major=$(kubectl version -o json | jq -er '.serverVersion.major | gsub("[^0-9]"; "")')
if [ "$server_major" -lt 1 ] || { [ "$server_major" -eq 1 ] && [ "$server_minor" -lt 30 ]; }; then printf 'the boundary admission policy requires Kubernetes 1.30 or newer\n' >&2; exit 1; fi
queue_name=$(awk '/clusterQueue:/ {print $2}' "$sealed")
cluster_queue_active=$(kubectl get clusterqueue.kueue.x-k8s.io "$queue_name" -o jsonpath='{.status.conditions[?(@.type=="Active")].status}')
[ "$cluster_queue_active" = True ] || { printf 'reviewed ClusterQueue %s is not Active\n' "$queue_name" >&2; exit 1; }
localqueue_served=$(kubectl get crd localqueues.kueue.x-k8s.io -o json | jq -er '[.spec.versions[] | select(.name=="v1beta1" and .served==true)] | length')
[ "$localqueue_served" = 1 ] || { printf 'LocalQueue v1beta1 is not served\n' >&2; exit 1; }

if [ "$phase" = sealed ]; then
  for pending_namespace in blazn-poc-system blazn-poc-sandboxes; do
    if ! namespace_absent "$pending_namespace"; then printf 'namespace %s is present or could not be verified\n' "$pending_namespace" >&2; exit 1; fi
  done
  if ! object_absent validatingadmissionpolicy blazn-sandbox-boundary; then printf 'the boundary admission policy already exists or could not be verified\n' >&2; exit 1; fi
  if ! object_absent validatingadmissionpolicybinding blazn-sandbox-boundary; then printf 'the boundary admission binding already exists or could not be verified\n' >&2; exit 1; fi
  write_phase apply-intent; phase=apply-intent
fi
if [ "$phase" = apply-intent ]; then
  for existing_namespace in blazn-poc-system blazn-poc-sandboxes; do
    if ! namespace_absent "$existing_namespace"; then
      owned_uid namespace "$existing_namespace" >/dev/null || { printf 'namespace %s exists without this transaction identity\n' "$existing_namespace" >&2; exit 1; }
    fi
  done
  kubectl apply --server-side --field-manager blazn-phase5-boundary -f "$sealed" >/dev/null
  write_phase applied; phase=applied
fi

uids=$transaction/owned-uids.json
{
  printf '{'
  printf '"namespace/blazn-poc-system":"%s",' "$(owned_uid namespace blazn-poc-system)"
  printf '"namespace/blazn-poc-sandboxes":"%s",' "$(owned_uid namespace blazn-poc-sandboxes)"
  printf '"serviceaccount/blazn-sandbox-runner":"%s",' "$(owned_uid serviceaccount blazn-sandbox-runner blazn-poc-sandboxes)"
  printf '"localqueue/blazn-poc":"%s",' "$(owned_uid localqueue.kueue.x-k8s.io blazn-poc blazn-poc-sandboxes)"
  printf '"role/blazn-agent-sandbox-controller":"%s",' "$(owned_uid role blazn-agent-sandbox-controller blazn-poc-sandboxes)"
  printf '"rolebinding/blazn-agent-sandbox-controller":"%s",' "$(owned_uid rolebinding blazn-agent-sandbox-controller blazn-poc-sandboxes)"
  printf '"validatingadmissionpolicy/blazn-sandbox-boundary":"%s",' "$(owned_uid validatingadmissionpolicy blazn-sandbox-boundary)"
  printf '"validatingadmissionpolicybinding/blazn-sandbox-boundary":"%s"' "$(owned_uid validatingadmissionpolicybinding blazn-sandbox-boundary)"
  printf '}\n'
} >"$uids.tmp"
jq -e 'to_entries | all(.value | test("^[0-9a-f-]{36}$"))' "$uids.tmp" >/dev/null || { printf 'owned object identities are incomplete\n' >&2; exit 1; }
mv "$uids.tmp" "$uids"
chmod 0600 "$uids"

[ "$(kubectl get serviceaccount blazn-sandbox-runner -n blazn-poc-sandboxes -o jsonpath='{.automountServiceAccountToken}')" = false ] || { printf 'runner ServiceAccount is not tokenless\n' >&2; exit 1; }
[ "$(kubectl get localqueue.kueue.x-k8s.io blazn-poc -n blazn-poc-sandboxes -o jsonpath='{.spec.clusterQueue}')" = "$queue_name" ] || { printf 'LocalQueue does not target the reviewed ClusterQueue\n' >&2; exit 1; }
attempt=0
until [ "$(kubectl get localqueue.kueue.x-k8s.io blazn-poc -n blazn-poc-sandboxes -o jsonpath='{.status.conditions[?(@.type=="Active")].status}')" = True ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 30 ] || { printf 'LocalQueue blazn-poc did not become Active\n' >&2; exit 1; }
  sleep 2
done
for enforced_namespace in blazn-poc-system blazn-poc-sandboxes; do
  [ "$(kubectl get namespace "$enforced_namespace" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')" = restricted ] || { printf 'namespace %s does not enforce the restricted profile\n' "$enforced_namespace" >&2; exit 1; }
done
[ "$(kubectl get validatingadmissionpolicy blazn-sandbox-boundary -o jsonpath='{.spec.failurePolicy}')" = Fail ] || { printf 'boundary policy is not fail-closed\n' >&2; exit 1; }
[ "$(kubectl get validatingadmissionpolicybinding blazn-sandbox-boundary -o jsonpath='{.spec.validationActions[0]}')" = Deny ] || { printf 'boundary binding does not deny\n' >&2; exit 1; }
write_phase complete
printf 'Phase 5 boundary installed and verified\n'
