#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
[ "$#" -eq 3 ] || { printf 'usage: %s INSTALL_BUNDLE PREINSTALL_INVENTORY CANARY_EVIDENCE\n' "$0" >&2; exit 64; }
install_bundle=$1
pre=$2
canary_evidence=$3
phase4c_require_mutation_authority
[ "$(cat "$pre/context")" = "$BLAZN_EXPECTED_CONTEXT" ]
[ "$(cat "$pre/kube-system.uid")" = "$BLAZN_EXPECTED_KUBE_SYSTEM_UID" ]
[ "$(kubectl get namespace blazn-poc -o jsonpath='{.metadata.uid}')" = "$(cat "$canary_evidence/blazn-poc.uid")" ]
[ "$(kubectl get namespace agent-sandbox-system -o jsonpath='{.metadata.uid}')" = "$(cat "$canary_evidence/agent-sandbox-system.uid")" ]

# Refuse to erase an object that appeared in the uniquely owned workload
# namespace after the canary. Events are evidence, not durable caller data.
unexpected=''
for resource in $(kubectl api-resources --verbs=list --namespaced -o name | LC_ALL=C sort -u); do
  [ "$resource" = events ] || [ "$resource" = events.events.k8s.io ] || {
    objects=$(kubectl get "$resource" -n blazn-poc --ignore-not-found -o name)
    for object in $objects; do
      case "$object" in
        serviceaccount/default|serviceaccount/blazn-sandbox-runner|configmap/kube-root-ca.crt|localqueue.kueue.x-k8s.io/blazn-poc) ;;
        *) unexpected="$unexpected $object" ;;
      esac
    done
  }
done
[ -z "$unexpected" ] || { printf 'refusing rollback with unexpected blazn-poc objects:%s\n' "$unexpected" >&2; exit 1; }

# The namespace was proven absent before install, so deleting this exact owned
# namespace cannot erase preexisting workloads. Stop the controller first.
kubectl scale deployment/agent-sandbox-controller -n agent-sandbox-system --replicas=0
kubectl wait --for=delete pod -l app=agent-sandbox-controller -n agent-sandbox-system --timeout=120s || :
kubectl delete namespace blazn-poc --wait=true --timeout=180s
kubectl delete -f "$install_bundle" --ignore-not-found --wait=true --timeout=180s
kubectl delete -f "$ROOT/controller-boundary.yaml" --ignore-not-found --wait=true --timeout=120s

targets=$(cat "$pre/phase4c-targets")
while IFS= read -r target; do
  [ -n "$target" ] || continue
  [ "$(kubectl get "$target" --ignore-not-found -o name | wc -l | tr -d ' ')" -eq 0 ] || {
    printf 'rollback residue: %s\n' "$target" >&2
    exit 1
  }
done <<EOF
$targets
EOF
[ -z "$(kubectl get sandboxes.agents.x-k8s.io -A -o name 2>/dev/null || true)" ]

post=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-post.XXXXXX")
cleanup() {
  find "$post" -xdev -type f -delete
  find "$post" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM
kubectl api-resources -o wide >"$post/api-resources.txt"
kubectl get crd -o name | LC_ALL=C sort | grep -E '(agents\.x-k8s\.io|kueue\.x-k8s\.io)' >"$post/relevant-crds.txt" || :
kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration,validatingadmissionpolicy,validatingadmissionpolicybinding -o name | LC_ALL=C sort | grep -E '(agent-sandbox|kueue|blazn)' >"$post/relevant-admission.txt" || :
kubectl get runtimeclass -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields)' >"$post/runtimeclasses.json"
kubectl get clusterqueue.kueue.x-k8s.io -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields,.items[].metadata.resourceVersion,.items[].metadata.managedFields,.items[].status)' >"$post/clusterqueues.json"
for file in api-resources.txt relevant-crds.txt relevant-admission.txt runtimeclasses.json clusterqueues.json; do
  cmp "$pre/$file" "$post/$file" || { printf 'rollback inventory differs: %s\n' "$file" >&2; exit 1; }
done
printf 'Phase 4C rollback matches the preinstall inventory with zero owned residue\n'
