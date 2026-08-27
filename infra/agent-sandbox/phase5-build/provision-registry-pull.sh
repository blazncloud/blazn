#!/bin/sh
set -eu

# Copies the existing in-cluster registry credential server-side into the
# Phase 5 namespaces as blazn-registry-pull and attaches it to the Sandbox
# ServiceAccounts, so kubelet can pull the published digest-pinned images.
# The credential never leaves the cluster and is never printed.
[ "$#" -eq 0 ] || { printf 'usage: %s\n' "$0" >&2; exit 64; }
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the boundary transaction UUID for ownership annotation}"
registry_secret_namespace=${BLAZN_REGISTRY_SECRET_NAMESPACE:-frontro-agent-runtime}
registry_secret_name=${BLAZN_REGISTRY_SECRET_NAME:-frontro-registry-pull}
command -v kubectl >/dev/null 2>&1 || { printf 'kubectl is required\n' >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }

for target_namespace in blazn-poc-system blazn-poc-sandboxes; do
  kubectl get namespace "$target_namespace" >/dev/null
  kubectl get secret "$registry_secret_name" -n "$registry_secret_namespace" -o json |
    jq --arg ns "$target_namespace" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" '{
      apiVersion: "v1", kind: "Secret", type: .type, data: .data,
      metadata: {name: "blazn-registry-pull", namespace: $ns,
                 labels: {"app.kubernetes.io/part-of": "blazn-phase5"},
                 annotations: {"blazn.dev/phase5-transaction": $tx}}
    }' |
    kubectl apply --server-side --field-manager blazn-phase5-boundary -f - >/dev/null
  printf 'blazn-registry-pull provisioned in %s\n' "$target_namespace"
done

for attach in blazn-poc-sandboxes/blazn-sandbox-runner blazn-poc-system/blazn-sandbox-controller; do
  attach_namespace=${attach%%/*}; attach_name=${attach#*/}
  if kubectl get serviceaccount "$attach_name" -n "$attach_namespace" >/dev/null 2>&1; then
    kubectl patch serviceaccount "$attach_name" -n "$attach_namespace" --type strategic -p '{"imagePullSecrets":[{"name":"blazn-registry-pull"}]}' >/dev/null
    printf 'attached blazn-registry-pull to %s\n' "$attach"
  else
    printf 'serviceaccount %s absent; attach later after its install\n' "$attach"
  fi
done
