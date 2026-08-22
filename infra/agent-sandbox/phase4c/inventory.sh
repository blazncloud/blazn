#!/bin/sh
set -eu

[ "$#" -eq 1 ] || { printf 'usage: %s EVIDENCE_DIRECTORY\n' "$0" >&2; exit 64; }
evidence=$1
command -v kubectl >/dev/null 2>&1 || { printf 'kubectl is required\n' >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
[ ! -e "$evidence" ] || { printf 'refusing to overwrite evidence directory: %s\n' "$evidence" >&2; exit 1; }
mkdir -m 0700 "$evidence"

context=$(kubectl config current-context)
cluster_uid=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
[ -n "$context" ] && [ -n "$cluster_uid" ]
printf '%s\n' "$context" >"$evidence/context"
printf '%s\n' "$cluster_uid" >"$evidence/kube-system.uid"
kubectl version -o json | jq -S . >"$evidence/version.json"

targets='crd/sandboxes.agents.x-k8s.io
crd/sandboxclaims.extensions.agents.x-k8s.io
crd/sandboxtemplates.extensions.agents.x-k8s.io
crd/sandboxwarmpools.extensions.agents.x-k8s.io
namespace/agent-sandbox-system
namespace/blazn-poc
clusterrole/blazn-agent-sandbox-observer
clusterrolebinding/blazn-agent-sandbox-observer
validatingadmissionpolicy/blazn-agent-sandbox-boundary
validatingadmissionpolicybinding/blazn-agent-sandbox-boundary'
printf '%s\n' "$targets" >"$evidence/phase4c-targets"
while IFS= read -r target; do
  [ -n "$target" ] || continue
  if kubectl get "$target" -o name >/dev/null 2>&1; then
    printf 'preexisting Phase 4C target: %s\n' "$target" >&2
    exit 1
  fi
done <<EOF
$targets
EOF

# The v0.5.6 manager has cluster-scoped informers. Admission is installed
# before the controller, so preexisting Sandbox objects would escape the
# reviewed namespace boundary and are an install blocker.
if kubectl get sandboxes.agents.x-k8s.io -A -o name >/dev/null 2>&1; then
  [ -z "$(kubectl get sandboxes.agents.x-k8s.io -A -o name 2>/dev/null)" ] || {
    printf 'preexisting Sandbox objects block Phase 4C\n' >&2
    exit 1
  }
fi

kubectl api-resources -o wide >"$evidence/api-resources.txt"
kubectl get crd -o name | LC_ALL=C sort | grep -E '(agents\.x-k8s\.io|kueue\.x-k8s\.io)' >"$evidence/relevant-crds.txt" || :
kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration,validatingadmissionpolicy,validatingadmissionpolicybinding -o name | LC_ALL=C sort | grep -E '(agent-sandbox|kueue|blazn)' >"$evidence/relevant-admission.txt" || :
kubectl get runtimeclass -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields)' >"$evidence/runtimeclasses.json"
kubectl get clusterqueue.kueue.x-k8s.io -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields,.items[].metadata.resourceVersion,.items[].metadata.managedFields,.items[].status)' >"$evidence/clusterqueues.json"

(
  cd "$evidence"
  sha256sum api-resources.txt relevant-crds.txt relevant-admission.txt runtimeclasses.json clusterqueues.json | LC_ALL=C sort >inventory.sha256
)
chmod 0400 "$evidence"/*
printf 'Read-only Phase 4C inventory captured for context %s, cluster %s\n' "$context" "$cluster_uid"
