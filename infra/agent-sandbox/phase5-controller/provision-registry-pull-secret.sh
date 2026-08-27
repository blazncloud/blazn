#!/bin/sh
set -eu

# Copies the separately owned registry pull credential into only the two Phase
# 5 namespaces that pull the reviewed controller and Sandbox images. Secret
# bytes remain in a pipe and are never printed or written to a temporary file.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'registry credential provisioning must run as root\n' >&2; exit 1; }
[ "$#" -eq 0 ] || { printf 'usage: %s\n' "$0" >&2; exit 64; }
phase4c_require_mutation_authority
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the Phase 5 transaction UUID}"
: "${BLAZN_REGISTRY_PULL_SOURCE_NAMESPACE:?set the separately owned source namespace}"
: "${BLAZN_REGISTRY_PULL_SECRET_NAME:?set the registry pull Secret name}"
for required in kubectl python3; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
for value in "$BLAZN_REGISTRY_PULL_SOURCE_NAMESPACE" "$BLAZN_REGISTRY_PULL_SECRET_NAME"; do
	printf '%s\n' "$value" | LC_ALL=C grep -Eq '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$' || { printf 'registry Secret identity is invalid\n' >&2; exit 1; }
done
[ "$(kubectl get secret "$BLAZN_REGISTRY_PULL_SECRET_NAME" -n "$BLAZN_REGISTRY_PULL_SOURCE_NAMESPACE" -o jsonpath='{.type}')" = kubernetes.io/dockerconfigjson ] || {
	printf 'source registry Secret has the wrong type\n' >&2
	exit 1
}

for target in blazn-poc-system blazn-poc-sandboxes; do
	kubectl get namespace "$target" >/dev/null
	kubectl get secret "$BLAZN_REGISTRY_PULL_SECRET_NAME" -n "$BLAZN_REGISTRY_PULL_SOURCE_NAMESPACE" -o json |
		BLAZN_TARGET_NAMESPACE="$target" BLAZN_PHASE5_TRANSACTION_ID="$BLAZN_PHASE5_TRANSACTION_ID" python3 -c '
import json, os, sys
doc = json.load(sys.stdin)
doc["metadata"] = {
    "name": doc["metadata"]["name"],
    "namespace": os.environ["BLAZN_TARGET_NAMESPACE"],
    "labels": {"app.kubernetes.io/part-of": "blazn-phase5"},
    "annotations": {"blazn.dev/phase5-transaction": os.environ["BLAZN_PHASE5_TRANSACTION_ID"]},
}
json.dump(doc, sys.stdout)
' | kubectl apply --server-side --field-manager blazn-phase5-controller -f - >/dev/null
done

printf 'registry pull Secret provisioned in the Phase 5 runtime namespaces\n'
