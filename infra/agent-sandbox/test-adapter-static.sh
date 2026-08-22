#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
SCHEMA=$ROOT/packages/contracts/sandbox-control-adapter-receipt.schema.json

jq -e '
  ."$id" == "https://blazn.dev/schemas/sandbox-control-adapter-receipt-v1.json" and
  .additionalProperties == false and
  .properties.namespace.const == "blazn-poc-sandboxes" and
  .properties.queueName.const == "blazn-poc" and
  .properties.operation.enum == ["create","delete","finalize"] and
  (.required | index("artifactContractDigest")) != null and
  (.properties.artifactContractDigest.pattern | length) > 0 and
  (.properties.state.enum | sort) == (["pending","queued","starting","ready","failed","stopping","deleted"] | sort) and
  ."x-blazn-error-status" == {
    "sandbox_invalid_request":400,"sandbox_identity_boundary":404,"sandbox_queue_required":502,
    "sandbox_runtime_untrusted":403,"sandbox_conflict":409,"sandbox_not_found":404,
    "sandbox_backend_failure":502,"sandbox_artifact_export_failed":502,
    "sandbox_cleanup_incomplete":409,"sandbox_resource_version_stale":409
  }' "$SCHEMA" >/dev/null

for marker in \
  'APIVersion          = "agents.x-k8s.io/v1beta1"' \
  'Namespace           = "blazn-poc-sandboxes"' \
  'QueueName           = "blazn-poc"' \
  'QueueLabel          = "kueue.x-k8s.io/queue-name"' \
  'CleanupFinalizer    = "sandboxes.blazn.dev/artifact-cleanup"' \
  'ServiceAccountName  = "blazn-sandbox-runner"'; do
  grep -F "$marker" "$ROOT/internal/sandboxcontrol/contract.go" >/dev/null
done
grep -F 'node.k8s.io/v1/runtimeclasses/' "$ROOT/internal/sandboxcontrol/adapter.go" >/dev/null
grep -F 'application/merge-patch+json' "$ROOT/internal/sandboxcontrol/adapter.go" >/dev/null
grep -F 'propagationPolicy": "Foreground"' "$ROOT/internal/sandboxcontrol/adapter.go" >/dev/null
if grep -R -E 'queue-name.*(optional|fallback)|unmanaged.*fallback' "$ROOT/internal/sandboxcontrol" >/dev/null; then
  printf 'adapter contains an unmanaged queue fallback\n' >&2
  exit 1
fi

printf 'Sandbox control adapter static contract passed\n'
