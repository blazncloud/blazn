#!/bin/sh
set -eu

# Live admission proof for the Phase 5 boundary on a disposable kind cluster:
# the exact adapter-shaped Sandbox is admitted, and every reviewed mutation is
# denied by the rule that owns it. Never touches a shared cluster.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
BOUNDARY=$ROOT/phase5-boundary
# shellcheck disable=SC1091
. "$ROOT/versions.env"
command -v docker >/dev/null 2>&1 || { printf 'docker is required\n' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { printf 'python3 is required\n' >&2; exit 1; }
python3 -c 'import yaml' 2>/dev/null || { printf 'python3 yaml module is required\n' >&2; exit 1; }
REAL_HELM=$(command -v helm) || { printf 'helm is required\n' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-boundary.XXXXXX")
cluster=''
cleanup() {
  if [ -n "$cluster" ]; then "$tmp/kind" delete cluster --name "$cluster" >/dev/null 2>&1 || :; fi
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

fetch() {
  fetch_url=$1; fetch_sha=$2; fetch_out=$3
  attempt=0
  while :; do
    attempt=$((attempt + 1))
    if curl --fail --silent --show-error --location --output "$fetch_out" "$fetch_url"; then
      [ "$(sha256sum "$fetch_out" | awk '{print $1}')" = "$fetch_sha" ] || { printf 'checksum mismatch for %s\n' "$fetch_url" >&2; exit 1; }
      return 0
    fi
    [ "$attempt" -lt 3 ] || { printf 'could not download %s\n' "$fetch_url" >&2; exit 1; }
    sleep 5
  done
}
fetch "$KIND_URL" "$KIND_SHA256" "$tmp/kind"
fetch "$KUBECTL_URL" "$KUBECTL_SHA256" "$tmp/kubectl"
fetch "$AGENT_SANDBOX_MANIFEST_URL" "$AGENT_SANDBOX_MANIFEST_SHA256" "$tmp/agent-sandbox.yaml"
chmod 0700 "$tmp/kind" "$tmp/kubectl"

# Queue CRDs come from the same pinned chart the live cluster runs, so the
# LocalQueue apply exercises the exact served v1beta1 storage version.
chart_tgz=$tmp/kueue-$LIVE_KUEUE_CHART_VERSION.tgz
attempt=0
while [ ! -f "$chart_tgz" ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 3 ] || { printf 'could not pull the pinned Kueue chart\n' >&2; exit 1; }
  (cd "$tmp" && "$REAL_HELM" pull "$LIVE_KUEUE_CHART_OCI" --version "$LIVE_KUEUE_CHART_VERSION" >/dev/null 2>&1) || sleep 5
done
[ "$(sha256sum "$chart_tgz" | awk '{print $1}')" = "$LIVE_KUEUE_CHART_SHA256" ] || { printf 'pulled Kueue chart checksum mismatch\n' >&2; exit 1; }
mkdir -m 0700 "$tmp/chart-src"
tar -xzf "$chart_tgz" -C "$tmp/chart-src" --no-same-owner --no-same-permissions
"$REAL_HELM" template kueue "$tmp/chart-src/kueue" -n kueue-system >"$tmp/kueue-rendered.yaml"

python3 - "$tmp/agent-sandbox.yaml" "$tmp/kueue-rendered.yaml" "$tmp/crds.yaml" <<'PY'
import sys, yaml
crds = []
for path in sys.argv[1:3]:
    for doc in yaml.safe_load_all(open(path)):
        if not doc or doc.get("kind") != "CustomResourceDefinition":
            continue
        name = doc["metadata"]["name"]
        if name in ("sandboxes.agents.x-k8s.io", "localqueues.kueue.x-k8s.io", "clusterqueues.kueue.x-k8s.io"):
            doc.get("spec", {}).pop("conversion", None)
            crds.append(doc)
assert len(crds) == 3, [c["metadata"]["name"] for c in crds]
yaml.safe_dump_all(crds, open(sys.argv[3], "w"))
PY

suffix=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
cluster=blazn-p5b-$suffix
if "$tmp/kind" get clusters 2>/dev/null | grep -Fxq "$cluster"; then printf 'cluster name collision\n' >&2; cluster=''; exit 1; fi
"$tmp/kind" create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --wait 180s >/dev/null 2>&1
export KUBECONFIG="$tmp/kubeconfig"
"$tmp/kind" export kubeconfig --name "$cluster" --kubeconfig "$KUBECONFIG" >/dev/null
k() { "$tmp/kubectl" "$@"; }

k apply -f "$tmp/crds.yaml" >/dev/null
k wait --for=condition=Established crd/sandboxes.agents.x-k8s.io crd/localqueues.kueue.x-k8s.io --timeout=90s >/dev/null

BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 BLAZN_EXISTING_CLUSTER_QUEUE=m1-light "$BOUNDARY/render-boundary.sh" "$tmp/boundary.yaml" >/dev/null
k apply --server-side --field-manager blazn-phase5-boundary -f "$tmp/boundary.yaml" >/dev/null

# Test-only stand-ins for identities the phase5 controller install provides.
controller=system:serviceaccount:blazn-poc-system:blazn-sandbox-controller
upstream=system:serviceaccount:agent-sandbox-system:agent-sandbox-controller
attacker=system:serviceaccount:default:phase5-attacker
k create serviceaccount blazn-sandbox-controller -n blazn-poc-system >/dev/null
k create serviceaccount phase5-attacker -n default >/dev/null
k create role sandbox-editor -n blazn-poc-sandboxes --verb=create,get,patch,update,delete --resource=sandboxes.agents.x-k8s.io,sandboxes.agents.x-k8s.io/status >/dev/null
k create rolebinding sandbox-editor -n blazn-poc-sandboxes --role=sandbox-editor --serviceaccount=blazn-poc-system:blazn-sandbox-controller --serviceaccount=default:phase5-attacker >/dev/null
k create rolebinding sandbox-upstream -n blazn-poc-sandboxes --role=sandbox-editor --serviceaccount=agent-sandbox-system:agent-sandbox-controller >/dev/null

python3 "$BOUNDARY/good-sandbox.py" >"$tmp/good.json"
python3 "$BOUNDARY/good-sandbox.py" bad-name >"$tmp/activation-probe.json"
attempt=0
until ! k create --dry-run=server --as="$controller" -f "$tmp/activation-probe.json" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 30 ] || { printf 'boundary policy never became enforcing\n' >&2; exit 1; }
  sleep 2
done

k create --dry-run=server --as="$controller" -f "$tmp/good.json" >/dev/null 2>"$tmp/good.err" || { printf 'adapter-shaped Sandbox was denied:\n' >&2; cat "$tmp/good.err" >&2; exit 1; }
python3 "$BOUNDARY/good-sandbox.py" many-sources >"$tmp/many.json"
k create --dry-run=server --as="$controller" -f "$tmp/many.json" >/dev/null 2>"$tmp/many.err" || { printf 'wide legal source shape was denied:\n' >&2; cat "$tmp/many.err" >&2; exit 1; }

expect_denied() {
  denied_case=$1; denied_pattern=$2; denied_as=$3
  if [ "$denied_case" = good ]; then cp "$tmp/good.json" "$tmp/case.json"; else python3 "$BOUNDARY/good-sandbox.py" "$denied_case" >"$tmp/case.json"; fi
  if k create --dry-run=server --as="$denied_as" -f "$tmp/case.json" >/dev/null 2>"$tmp/case.err"; then
    printf 'mutation %s was admitted\n' "$denied_case" >&2; exit 1
  fi
  grep -Eq -- "$denied_pattern" "$tmp/case.err" || { printf 'mutation %s was denied by the wrong rule:\n' "$denied_case" >&2; cat "$tmp/case.err" >&2; exit 1; }
}
expect_denied bad-name 'canonical lowercase UUIDs' "$controller"
expect_denied missing-managed-label 'sandbox-id labels|no such key' "$controller"
expect_denied wrong-queue 'blazn-poc LocalQueue' "$controller"
expect_denied tag-image 'digest-pinned' "$controller"
expect_denied host-network 'host namespaces' "$controller"
expect_denied extra-node-selector 'eligibility selector' "$controller"
expect_denied wrong-service-account 'tokenless blazn-sandbox-runner' "$controller"
expect_denied token-automount 'tokenless blazn-sandbox-runner' "$controller"
expect_denied over-cpu 'reviewed bounds' "$controller"
expect_denied host-path-volume 'emptyDir volumes' "$controller"
expect_denied extra-container 'exactly one main container' "$controller"
expect_denied env-injection 'widen their execution surface' "$controller"
expect_denied runtime-class 'RuntimeClass' "$controller"
expect_denied missing-trust 'intent-digest annotations|no such key' "$controller"
expect_denied priv-escalation 'restricted security context' "$controller"
expect_denied foreign-helper 'digest-pinned IO helpers' "$controller"
expect_denied shutdown-retain 'shutdownPolicy must be Delete' "$controller"
expect_denied wrong-workspace-shape 'sandbox-id labels' "$controller"
expect_denied mount-traversal 'reviewed workspace paths' "$controller"
expect_denied mount-subpath 'reviewed workspace paths' "$controller"
expect_denied init-ephemeral-oversize 'digest-pinned IO helpers' "$controller"
expect_denied volume-size-oversize 'size-bounded emptyDir volumes' "$controller"
expect_denied good 'Only the Blazn sandbox controller may create' "$attacker"

# Update fencing: mutations are denied for every identity; the controller's
# finalizer-removal patch and the upstream status manager stay within bounds.
k create --as="$controller" -f "$tmp/good.json" >/dev/null
name=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["metadata"]["name"])' "$tmp/good.json")
if k patch sandbox.agents.x-k8s.io "$name" -n blazn-poc-sandboxes --dry-run=server --as="$controller" --type=merge -p '{"metadata":{"labels":{"blazn.dev/owner":"34000000-0000-4000-8000-000000000004"}},"spec":{"podTemplate":{"metadata":{"labels":{"blazn.dev/owner":"34000000-0000-4000-8000-000000000004"}}}}}' >/dev/null 2>"$tmp/update.err"; then
  printf 'label mutation was admitted\n' >&2; exit 1
fi
grep -Fq 'immutable after admission' "$tmp/update.err"
if k patch sandbox.agents.x-k8s.io "$name" -n blazn-poc-sandboxes --dry-run=server --as="$upstream" --type=merge -p '{"spec":{"shutdownPolicy":"Retain"}}' >/dev/null 2>"$tmp/update2.err"; then
  printf 'spec mutation was admitted\n' >&2; exit 1
fi
grep -Eq 'immutable after admission|shutdownPolicy must be Delete' "$tmp/update2.err"
if k patch sandbox.agents.x-k8s.io "$name" -n blazn-poc-sandboxes --dry-run=server --as="$attacker" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>"$tmp/update3.err"; then
  printf 'attacker finalizer removal was admitted\n' >&2; exit 1
fi
grep -Fq 'immutable after admission' "$tmp/update3.err"
k patch sandbox.agents.x-k8s.io "$name" -n blazn-poc-sandboxes --dry-run=server --as="$controller" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null

# The status subresource is inside the boundary: the upstream controller may
# manage status, and every other identity is denied.
if k patch sandbox.agents.x-k8s.io "$name" -n blazn-poc-sandboxes --subresource=status --dry-run=server --as="$attacker" --type=merge -p '{"status":{"conditions":[{"type":"Ready","status":"True","reason":"Forged","message":"forged","lastTransitionTime":"2026-08-27T00:00:00Z"}]}}' >/dev/null 2>"$tmp/status.err"; then
  printf 'forged status update was admitted\n' >&2; exit 1
fi
grep -Fq 'immutable after admission' "$tmp/status.err"
k patch sandbox.agents.x-k8s.io "$name" -n blazn-poc-sandboxes --subresource=status --dry-run=server --as="$upstream" --type=merge -p '{"status":{"conditions":[{"type":"Ready","status":"True","reason":"Provisioned","message":"ok","lastTransitionTime":"2026-08-27T00:00:00Z"}]}}' >/dev/null 2>"$tmp/status2.err" || { printf 'upstream status update was denied:\n' >&2; cat "$tmp/status2.err" >&2; exit 1; }

printf 'Phase 5 boundary admission matrix passed\n'
