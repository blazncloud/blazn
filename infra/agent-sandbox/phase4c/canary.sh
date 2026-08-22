#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
[ "$#" -eq 3 ] || { printf 'usage: %s INSTALL_BUNDLE FIXTURE_DIRECTORY EVIDENCE_DIRECTORY\n' "$0" >&2; exit 64; }
install_bundle=$1
fixtures=$2
evidence=$3
phase4c_require_mutation_authority
[ -f "$install_bundle" ] && [ -f "$fixtures/blazn-poc.yaml" ] && [ -f "$fixtures/synthetic-canary.yaml" ]
[ ! -e "$evidence" ] || { printf 'refusing to overwrite evidence directory: %s\n' "$evidence" >&2; exit 1; }
mkdir -m 0700 "$evidence"

# Inventory guarantees unique ownership. Recheck immediately before mutation.
for target in namespace/agent-sandbox-system namespace/blazn-poc crd/sandboxes.agents.x-k8s.io clusterrole/blazn-agent-sandbox-observer validatingadmissionpolicy/blazn-agent-sandbox-boundary; do
  [ "$(kubectl get "$target" --ignore-not-found -o name | wc -l | tr -d ' ')" -eq 0 ] || {
    printf 'target appeared after inventory: %s\n' "$target" >&2
    exit 1
  }
done

# Admission and mutation RBAC precede controller startup. The rewritten
# upstream bundle contains no ClusterRole or ClusterRoleBinding.
kubectl apply --server-side -f "$fixtures/blazn-poc.yaml" >"$evidence/apply-namespace.txt"
kubectl apply --server-side -f "$ROOT/controller-boundary.yaml" >"$evidence/apply-boundary.txt"
kubectl apply --server-side -f "$install_bundle" >"$evidence/apply-controller.txt"
kubectl get namespace blazn-poc -o jsonpath='{.metadata.uid}' >"$evidence/blazn-poc.uid"
kubectl get namespace agent-sandbox-system -o jsonpath='{.metadata.uid}' >"$evidence/agent-sandbox-system.uid"
[ -s "$evidence/blazn-poc.uid" ] && [ -s "$evidence/agent-sandbox-system.uid" ]
kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=120s
kubectl wait --for=condition=Available deployment/agent-sandbox-controller -n agent-sandbox-system --timeout=180s

sa=system:serviceaccount:agent-sandbox-system:agent-sandbox-controller
[ "$(kubectl auth can-i --as="$sa" delete pods -n blazn-poc)" = yes ]
[ "$(kubectl auth can-i --as="$sa" delete pods -n default)" = no ]
[ "$(kubectl auth can-i --as="$sa" create sandboxes.agents.x-k8s.io -n blazn-poc)" = yes ]
[ "$(kubectl auth can-i --as="$sa" create sandboxes.agents.x-k8s.io -n default)" = no ]
kubectl auth can-i --list --as="$sa" -n blazn-poc >"$evidence/controller-rbac-blazn-poc.txt"
kubectl auth can-i --list --as="$sa" -n default >"$evidence/controller-rbac-default.txt"

# Server dry-run proves the admission boundary without leaving an object.
sed 's/namespace: blazn-poc/namespace: default/' "$fixtures/synthetic-canary.yaml" |
  if kubectl create --dry-run=server -f - >"$evidence/outside-boundary.txt" 2>&1; then
    printf 'admission unexpectedly allowed a Sandbox outside blazn-poc\n' >&2
    exit 1
  fi

kubectl apply --server-side -f "$fixtures/synthetic-canary.yaml" >"$evidence/apply-canary.txt"
kubectl wait --for=condition=Ready sandbox/phase4c-canary -n blazn-poc --timeout=180s
[ "$(kubectl get pod phase4c-canary -n blazn-poc -o jsonpath='{.status.phase}')" = Running ]
[ "$(kubectl get pod phase4c-canary -n blazn-poc -o jsonpath='{.metadata.labels.kueue\.x-k8s\.io/queue-name}')" = blazn-poc ]
[ "$(kubectl get workload.kueue.x-k8s.io -n blazn-poc -o jsonpath='{.items[0].status.conditions[?(@.type=="Admitted")].status}')" = True ]
[ "$(kubectl get workload.kueue.x-k8s.io -n blazn-poc -o jsonpath='{.items[0].status.admission.podSetAssignments[0].resourceUsage.cpu}')" = 100m ]
[ "$(kubectl get workload.kueue.x-k8s.io -n blazn-poc -o jsonpath='{.items[0].status.admission.podSetAssignments[0].resourceUsage.memory}')" = 64Mi ]
kubectl get sandbox,pod,workload.kueue.x-k8s.io -n blazn-poc -o yaml >"$evidence/canary-objects.yaml"
kubectl get events -n blazn-poc --sort-by=.metadata.creationTimestamp >"$evidence/events.txt"

kubectl delete sandbox phase4c-canary -n blazn-poc --wait=true --timeout=120s
kubectl wait --for=delete pod/phase4c-canary -n blazn-poc --timeout=120s
kubectl wait --for=delete workload.kueue.x-k8s.io --all -n blazn-poc --timeout=120s
[ "$(phase4c_count sandbox -n blazn-poc)" -eq 0 ]
[ "$(phase4c_count pod -n blazn-poc)" -eq 0 ]
[ "$(phase4c_count workload.kueue.x-k8s.io -n blazn-poc)" -eq 0 ]

chmod 0400 "$evidence"/*
printf 'Phase 4C synthetic canary passed; canary resources have zero residue\n'
