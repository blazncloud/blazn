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
if grep -F -- '- --extensions' "$tmp/install.yaml" >/dev/null; then exit 1; fi
if grep -F 'kind: ClusterRole' "$tmp/install.yaml" >/dev/null; then exit 1; fi
if grep -F 'kind: ClusterRoleBinding' "$tmp/install.yaml" >/dev/null; then exit 1; fi
[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/install.yaml")" -eq 4 ]

mkdir "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
case "$*" in
  *'get runtimeclass blazn-qualified'*) printf 'runsc' ;;
  *'get clusterqueue.kueue.x-k8s.io'*) printf 'True' ;;
  *'get nodes -l blazn.dev/sandbox-eligible=true'*) printf 'node/node-a' ;;
  *) printf 'unexpected fake kubectl invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod 0700 "$tmp/bin/kubectl"
PATH="$tmp/bin:$PATH" \
  BLAZN_EXISTING_CLUSTER_QUEUE=shared-capacity \
  BLAZN_SYNTHETIC_IMAGE='example.invalid/synthetic@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  BLAZN_ORCHESTRATION_ONLY_ACK=approved-non-sensitive-phase4c-canary \
  "$PHASE4C/render-fixtures.sh" "$tmp/orchestration-only" >/dev/null
grep -F 'blazn.dev/runtime-trust: orchestration-only' "$tmp/orchestration-only/synthetic-canary.yaml" >/dev/null
if grep -F 'runtimeClassName:' "$tmp/orchestration-only/synthetic-canary.yaml" >/dev/null; then exit 1; fi
grep -F "object.metadata.labels['blazn.dev/runtime-trust'] == 'orchestration-only'" "$tmp/orchestration-only/controller-boundary.yaml" >/dev/null
PATH="$tmp/bin:$PATH" \
  BLAZN_EXISTING_CLUSTER_QUEUE=shared-capacity \
  BLAZN_SYNTHETIC_IMAGE='example.invalid/synthetic@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  BLAZN_RUNTIME_CLASS=blazn-qualified \
  BLAZN_EXPECTED_RUNTIME_HANDLER=runsc \
  "$PHASE4C/render-fixtures.sh" "$tmp/qualified-runtime" >/dev/null
grep -F 'blazn.dev/runtime-trust: qualified-runtime' "$tmp/qualified-runtime/synthetic-canary.yaml" >/dev/null
grep -F 'runtimeClassName: blazn-qualified' "$tmp/qualified-runtime/synthetic-canary.yaml" >/dev/null
grep -F "object.spec.podTemplate.spec.runtimeClassName == 'blazn-qualified'" "$tmp/qualified-runtime/controller-boundary.yaml" >/dev/null

grep -F 'namespace: blazn-poc' "$PHASE4C/controller-boundary.yaml.in" >/dev/null
grep -F 'object.metadata.namespace == '\''blazn-poc'\''' "$PHASE4C/controller-boundary.yaml.in" >/dev/null
grep -F 'validationActions: [Deny]' "$PHASE4C/controller-boundary.yaml.in" >/dev/null
grep -F "c.image.matches('^.+@sha256:[0-9a-f]{64}$')" "$PHASE4C/controller-boundary.yaml.in" >/dev/null
grep -F "size(object.spec.podTemplate.spec.volumes) == 0" "$PHASE4C/controller-boundary.yaml.in" >/dev/null
grep -F 'clusterQueue: BLAZN_EXISTING_CLUSTER_QUEUE' "$PHASE4C/blazn-poc.yaml.in" >/dev/null
grep -F "approved-non-sensitive-phase4c-canary" "$PHASE4C/render-fixtures.sh" >/dev/null
grep -F 'readlink "/proc/$$/fd/9"' "$PHASE4C/lib.sh" >/dev/null
grep -F "stat -c '%u:%a'" "$PHASE4C/lib.sh" >/dev/null
# shellcheck disable=SC2016
grep -F 'cmp "$pre/$file" "$post/$file"' "$PHASE4C/rollback.sh" >/dev/null
if grep -E 'kubectl (apply|create|delete|edit|label|patch|replace|scale)' "$PHASE4C/inventory.sh" >/dev/null; then exit 1; fi

printf 'Phase 4C non-mutating preparation audit passed\n'
