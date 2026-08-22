#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck disable=SC1091
. "$ROOT/versions.env"
# shellcheck disable=SC1091
. "$ROOT/lib.sh"

[ "$#" -eq 1 ] || { printf 'usage: %s OUTPUT\n' "$0" >&2; exit 64; }
output=$1
[ ! -e "$output" ] || { printf 'refusing to overwrite output: %s\n' "$output" >&2; exit 1; }
: "${BLAZN_PHASE4C_TRANSACTION_ID:?set the reviewed Phase 4C transaction UUID}"
case "$BLAZN_PHASE4C_TRANSACTION_ID" in ????????-????-4???-[89ab]???-????????????) ;; *) printf 'invalid Phase 4C transaction UUID\n' >&2; exit 1 ;; esac
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-render.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
abort() {
  trap - EXIT HUP INT TERM
  cleanup
  exit 130
}
trap cleanup EXIT
trap abort HUP INT TERM

curl -fsSL "$AGENT_SANDBOX_MANIFEST_URL" -o "$tmp/upstream.yaml"
printf '%s  %s\n' "$AGENT_SANDBOX_MANIFEST_SHA256" "$tmp/upstream.yaml" | sha256sum -c - >/dev/null

# Drop the six upstream RBAC documents. Their cluster-wide Pod delete and
# extension privileges must never exist, even transiently. Extensions remain
# disabled and the reviewed replacement RBAC is applied separately first.
awk '
function reset_doc() { doc=""; kind=""; name=""; inmeta=0 }
function emit_doc(    skip) {
  if (doc == "") return
  skip = ((kind == "Role" && name == "agent-sandbox-controller") ||
          (kind == "RoleBinding" && name == "agent-sandbox-controller") ||
          (kind == "ClusterRole" && (name == "agent-sandbox-controller" || name == "agent-sandbox-controller-extensions")) ||
          (kind == "ClusterRoleBinding" && (name == "agent-sandbox-controller" || name == "agent-sandbox-controller-extensions")))
  if (kind == "Namespace" && name == "agent-sandbox-system") { namespaces_removed++; return }
  if (skip) { removed++; return }
  if (emitted > 0) print "---"
  printf "%s", doc
  emitted++
}
BEGIN { reset_doc() }
$0 == "---" { emit_doc(); reset_doc(); next }
{
  if ($0 ~ /^kind: / && kind == "") { kind=$0; sub(/^kind: /, "", kind) }
  if ($0 == "metadata:") {
    inmeta=1
    doc = doc $0 "\n  annotations:\n    blazn.dev/phase4c-transaction: " ENVIRON["BLAZN_PHASE4C_TRANSACTION_ID"] "\n"
    next
  }
  else if (inmeta && $0 ~ /^  name: / && name == "") { name=$0; sub(/^  name: /, "", name) }
  else if (inmeta && $0 !~ /^  / && $0 != "metadata:") inmeta=0
  if ($0 == "        - --extensions") { extensions_removed++; next }
  doc = doc $0 "\n"
  if (kind == "Deployment" && name == "agent-sandbox-controller" && $0 == "        - --leader-elect=true") {
    doc = doc "        - --leader-election-namespace=agent-sandbox-system\n"
    doc = doc "        - --cache-label-selectors=true\n"
    doc = doc "        - --manage-webhook-certs=false\n"
  }
  if (kind == "Deployment" && name == "agent-sandbox-controller" && $0 == "  template:") in_template=1
  if (kind == "Deployment" && name == "agent-sandbox-controller" && in_template && $0 == "    metadata:") {
    doc = doc "      annotations:\n        blazn.dev/phase4c-transaction: " ENVIRON["BLAZN_PHASE4C_TRANSACTION_ID"] "\n"
  }
  if (kind == "Deployment" && name == "agent-sandbox-controller" && in_template && $0 == "    spec:") {
    doc = doc "      automountServiceAccountToken: false\n"
    doc = doc "      securityContext:\n        runAsNonRoot: true\n        runAsUser: 65532\n        runAsGroup: 65532\n        fsGroup: 65532\n        seccompProfile:\n          type: RuntimeDefault\n"
  }
  if (kind == "Deployment" && name == "agent-sandbox-controller" && $0 == "        name: agent-sandbox-controller") {
    doc = doc "        resources:\n          requests:\n            cpu: 50m\n            memory: 64Mi\n          limits:\n            cpu: 500m\n            memory: 256Mi\n"
    doc = doc "        securityContext:\n          allowPrivilegeEscalation: false\n          capabilities:\n            drop: [\"ALL\"]\n          privileged: false\n          readOnlyRootFilesystem: true\n          runAsNonRoot: true\n"
    doc = doc "        readinessProbe:\n          httpGet:\n            path: /healthz\n            port: healthz\n          periodSeconds: 5\n        livenessProbe:\n          httpGet:\n            path: /healthz\n            port: healthz\n          periodSeconds: 10\n"
  }
  if (kind == "Deployment" && name == "agent-sandbox-controller" && $0 == "        volumeMounts:") {
    doc = doc "        - mountPath: /tmp/k8s-webhook-server/serving-certs\n          name: webhook-certs\n          readOnly: true\n"
  }
  if (kind == "Deployment" && name == "agent-sandbox-controller" && $0 == "      volumes:") {
    doc = doc "      - name: webhook-certs\n        secret:\n          secretName: agent-sandbox-webhook-certs\n"
  }
}
END {
  emit_doc()
  if (removed != 6) { printf "expected six upstream RBAC documents, removed %d\n", removed > "/dev/stderr"; exit 91 }
  if (extensions_removed != 1) { printf "expected one extensions argument, removed %d\n", extensions_removed > "/dev/stderr"; exit 92 }
  if (namespaces_removed != 1) { printf "expected one controller namespace, removed %d\n", namespaces_removed > "/dev/stderr"; exit 93 }
}
' "$tmp/upstream.yaml" >"$tmp/scoped-upstream.yaml"

# Reuse the checksum-locked image rewrite, with an empty Kueue fixture solely
# to preserve the helper's two-manifest invariant.
printf 'image: %s\n' "${KUEUE_IMAGE%%@*}" >"$tmp/kueue-placeholder.yaml"
pin_controller_images "$tmp/scoped-upstream.yaml" "$tmp/kueue-placeholder.yaml"

if grep -F -- '- --extensions' "$tmp/scoped-upstream.yaml" >/dev/null; then printf 'extensions argument survived rewrite\n' >&2; exit 1; fi
if grep -F 'kind: ClusterRoleBinding' "$tmp/scoped-upstream.yaml" >/dev/null; then printf 'upstream ClusterRoleBinding survived rewrite\n' >&2; exit 1; fi
if grep -F 'kind: ClusterRole' "$tmp/scoped-upstream.yaml" >/dev/null; then printf 'upstream ClusterRole survived rewrite\n' >&2; exit 1; fi
grep -F -- '- --leader-election-namespace=agent-sandbox-system' "$tmp/scoped-upstream.yaml" >/dev/null
grep -F -- '- --cache-label-selectors=true' "$tmp/scoped-upstream.yaml" >/dev/null
grep -F -- '- --manage-webhook-certs=false' "$tmp/scoped-upstream.yaml" >/dev/null
grep -F 'readOnlyRootFilesystem: true' "$tmp/scoped-upstream.yaml" >/dev/null
grep -F 'secretName: agent-sandbox-webhook-certs' "$tmp/scoped-upstream.yaml" >/dev/null

cp "$tmp/scoped-upstream.yaml" "$output"
chmod 0400 "$output"
trap - EXIT HUP INT TERM
cleanup
