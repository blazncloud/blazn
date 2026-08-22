#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PHASE4C=$ROOT/phase4c
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-static.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

for script in "$PHASE4C"/*.sh; do sh -n "$script"; done
"$PHASE4C/render-install.sh" "$tmp/install.yaml"
grep -F 'image: registry.k8s.io/agent-sandbox/agent-sandbox-controller:v0.5.6@sha256:' "$tmp/install.yaml" >/dev/null
grep -F -- '- --leader-election-namespace=agent-sandbox-system' "$tmp/install.yaml" >/dev/null
grep -F -- '- --cache-label-selectors=true' "$tmp/install.yaml" >/dev/null
! grep -F -- '- --extensions' "$tmp/install.yaml" >/dev/null
! grep -F 'kind: ClusterRole' "$tmp/install.yaml" >/dev/null
! grep -F 'kind: ClusterRoleBinding' "$tmp/install.yaml" >/dev/null
[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/install.yaml")" -eq 4 ]

grep -F 'namespace: blazn-poc' "$PHASE4C/controller-boundary.yaml" >/dev/null
grep -F 'object.metadata.namespace == '\''blazn-poc'\''' "$PHASE4C/controller-boundary.yaml" >/dev/null
grep -F 'validationActions: [Deny]' "$PHASE4C/controller-boundary.yaml" >/dev/null
grep -F 'clusterQueue: BLAZN_EXISTING_CLUSTER_QUEUE' "$PHASE4C/blazn-poc.yaml.in" >/dev/null
grep -F "approved-non-sensitive-phase4c-canary" "$PHASE4C/render-fixtures.sh" >/dev/null
grep -F 'readlink "/proc/$$/fd/9"' "$PHASE4C/lib.sh" >/dev/null
grep -F "stat -c '%u:%a'" "$PHASE4C/lib.sh" >/dev/null
grep -F 'cmp "$pre/$file" "$post/$file"' "$PHASE4C/rollback.sh" >/dev/null

printf 'Phase 4C non-mutating preparation audit passed\n'
