#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
[ "$#" -eq 1 ] || { printf 'usage: %s OUTPUT_DIRECTORY\n' "$0" >&2; exit 64; }
output=$1
[ ! -e "$output" ] || { printf 'refusing to overwrite output directory: %s\n' "$output" >&2; exit 1; }
: "${BLAZN_EXISTING_CLUSTER_QUEUE:?set the reviewed existing ClusterQueue name}"
: "${BLAZN_SYNTHETIC_IMAGE:?set an immutable synthetic image reference}"
case "$BLAZN_SYNTHETIC_IMAGE" in *@sha256:*) ;; *) printf 'synthetic image must be digest pinned\n' >&2; exit 1 ;; esac

runtime_trust=qualified-runtime
runtime_line=''
approval='not-applicable'
runtime_admission=''
if [ -n "${BLAZN_RUNTIME_CLASS:-}" ]; then
  case "$BLAZN_RUNTIME_CLASS" in *[!a-z0-9.-]*|'') printf 'invalid RuntimeClass name\n' >&2; exit 1 ;; esac
  : "${BLAZN_EXPECTED_RUNTIME_HANDLER:?set the reviewed RuntimeClass handler}"
  [ "$(kubectl get runtimeclass "$BLAZN_RUNTIME_CLASS" -o jsonpath='{.handler}')" = "$BLAZN_EXPECTED_RUNTIME_HANDLER" ] || {
    printf 'RuntimeClass handler does not match the reviewed capability\n' >&2
    exit 1
  }
  runtime_line="      runtimeClassName: $BLAZN_RUNTIME_CLASS"
  runtime_admission="object.metadata.labels['blazn.dev/runtime-trust'] == 'qualified-runtime' \&\& object.spec.podTemplate.spec.runtimeClassName == '$BLAZN_RUNTIME_CLASS'"
else
  [ "${BLAZN_ORCHESTRATION_ONLY_ACK:-}" = 'approved-non-sensitive-phase4c-canary' ] || {
    printf 'no qualified RuntimeClass; explicit non-sensitive orchestration-only approval is required\n' >&2
    exit 1
  }
  runtime_trust=orchestration-only
  approval=approved
  runtime_admission="object.metadata.labels['blazn.dev/runtime-trust'] == 'orchestration-only' \&\& !has(object.spec.podTemplate.spec.runtimeClassName) \&\& object.metadata.annotations['blazn.dev/non-sensitive-poc'] == 'approved'"
fi

[ "$(kubectl get clusterqueue.kueue.x-k8s.io "$BLAZN_EXISTING_CLUSTER_QUEUE" -o jsonpath='{.status.conditions[?(@.type=="Active")].status}')" = True ] || {
  printf 'existing ClusterQueue is not Active\n' >&2
  exit 1
}
[ -n "$(kubectl get nodes -l blazn.dev/sandbox-eligible=true -o name)" ] || {
  printf 'no existing sandbox-eligible node is available; this prep never labels shared nodes\n' >&2
  exit 1
}
mkdir -m 0700 "$output"
sed "s|BLAZN_EXISTING_CLUSTER_QUEUE|$BLAZN_EXISTING_CLUSTER_QUEUE|g" "$ROOT/blazn-poc.yaml.in" >"$output/blazn-poc.yaml"
sed "s|BLAZN_RUNTIME_ADMISSION_EXPRESSION|$runtime_admission|g" "$ROOT/controller-boundary.yaml.in" >"$output/controller-boundary.yaml"
sed \
  -e "s|BLAZN_RUNTIME_TRUST|$runtime_trust|g" \
  -e "s|BLAZN_NON_SENSITIVE_APPROVAL|$approval|g" \
  -e "s|BLAZN_RUNTIME_CLASS_LINE|$runtime_line|g" \
  -e "s|BLAZN_SYNTHETIC_IMAGE|$BLAZN_SYNTHETIC_IMAGE|g" \
  "$ROOT/synthetic-canary.yaml.in" >"$output/synthetic-canary.yaml"
chmod 0400 "$output"/*.yaml
printf 'Rendered Phase 4C fixtures in %s (%s)\n' "$output" "$runtime_trust"
