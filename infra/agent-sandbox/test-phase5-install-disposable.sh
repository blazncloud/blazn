#!/bin/sh
set -eu

# Full-stack rehearsal of the Phase 5 target on a disposable kind cluster:
# patched Kueue with the Pod integration, the boundary, the real Phase 5
# installation transaction (including a mid-flight crash and resume), a real
# admitted Sandbox whose Kueue Workload reserves the exact requested
# resources, teardown, and a UID-fenced rollback to zero residue.
if [ "$(id -u)" -ne 0 ]; then exec sudo -n "$0" "$@"; fi
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
BOUNDARY=$ROOT/phase5-boundary
INSTALL=$ROOT/phase5-install
# shellcheck disable=SC1091
. "$ROOT/versions.env"
for required in docker python3 curl tar jq flock sha256sum openssl; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
python3 -c 'import yaml' 2>/dev/null || { printf 'python3 yaml module is required\n' >&2; exit 1; }
REAL_HELM=$(command -v helm) || { printf 'helm is required\n' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-install.XXXXXX")
cluster=''
tx_root=/var/lib/blazn/phase5
tx_prefix=install-e2e$$
blazn_root_created=0
test_lock_owned=0
cleanup() {
  if [ -n "$cluster" ]; then "$tmp/kind" delete cluster --name "$cluster" >/dev/null 2>&1 || :; fi
  for owned in "$tx_root/$tx_prefix"*; do
    if [ -d "$owned" ]; then find "$owned" -mindepth 1 -xdev -delete; rmdir "$owned"; fi
  done
  if [ "$test_lock_owned" -eq 1 ] && [ -d /run/lock/blazn ]; then
    find /run/lock/blazn -xdev -type f -delete
    find /run/lock/blazn -xdev -depth -type d -empty -delete
  fi
  if [ "$blazn_root_created" -eq 1 ]; then
    rmdir "$tx_root" 2>/dev/null || :
    rmdir /var/lib/blazn 2>/dev/null || :
  fi
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM
[ ! -e /run/lock/blazn ] || { printf 'live lock path unexpectedly exists; refusing the disposable harness here\n' >&2; exit 1; }
test_lock_owned=1
if [ ! -d "$tx_root" ]; then blazn_root_created=1; install -d -o root -g root -m 0700 /var/lib/blazn "$tx_root"; fi

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
chmod 0700 "$tmp/kind" "$tmp/kubectl"

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
patch -s -f -d "$tmp/chart-src" -p1 <"$ROOT/phase4c/kueue-pod-webhook-selector.patch"

suffix=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
cluster=blazn-p5i-$suffix
if "$tmp/kind" get clusters 2>/dev/null | grep -Fxq "$cluster"; then printf 'cluster name collision\n' >&2; cluster=''; exit 1; fi
"$tmp/kind" create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --wait 180s >/dev/null 2>&1
export KUBECONFIG="$tmp/kubeconfig"
"$tmp/kind" export kubeconfig --name "$cluster" --kubeconfig "$KUBECONFIG" >/dev/null
k() { "$tmp/kubectl" "$@"; }
node=$(k get nodes -o name | head -1)
k label "$node" blazn.dev/sandbox-eligible=true >/dev/null

"$REAL_HELM" install kueue "$tmp/chart-src/kueue" -n kueue-system --create-namespace \
  --set-file managerConfig.controllerManagerConfigYaml="$ROOT/phase4c/kueue-pod-config.yaml" \
  --wait --timeout 300s >/dev/null
cat >"$tmp/queues.yaml" <<'EOF'
apiVersion: kueue.x-k8s.io/v1beta1
kind: ResourceFlavor
metadata:
  name: default-flavor
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: m1-light
spec:
  namespaceSelector: {}
  resourceGroups:
  - coveredResources: ["cpu", "memory", "ephemeral-storage"]
    flavors:
    - name: default-flavor
      resources:
      - name: cpu
        nominalQuota: 4
      - name: memory
        nominalQuota: 8Gi
      - name: ephemeral-storage
        nominalQuota: 20Gi
EOF
attempt=0
until k apply -f "$tmp/queues.yaml" >/dev/null 2>&1; do
  attempt=$((attempt + 1)); [ "$attempt" -le 30 ] || { printf 'queue fixtures never applied\n' >&2; exit 1; }; sleep 2
done
attempt=0
until [ "$(k get clusterqueue m1-light -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)" = True ]; do
  attempt=$((attempt + 1)); [ "$attempt" -le 30 ] || { printf 'ClusterQueue never became Active\n' >&2; exit 1; }; sleep 2
done

# Installing before the boundary must be refused.
run_install() {
  run_render=$1; run_tx=$2; shift 2
  set +e
  "$ROOT/phase4c/with-live-lock.sh" env PATH="$tmp:$PATH" KUBECONFIG="$KUBECONFIG" \
    BLAZN_EXPECTED_CONTEXT="kind-$cluster" \
    BLAZN_EXPECTED_KUBE_SYSTEM_UID="$kube_uid" \
    BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
    BLAZN_PHASE5_TRANSACTION_DIR="$run_tx" \
    BLAZN_PHASE5_TRANSACTION_ID="$tx_uuid" \
    BLAZN_EXPECTED_INSTALL_SHA256="$install_sha" \
    BLAZN_EXPECTED_RBAC_SHA256="$rbac_sha" \
    BLAZN_EXPECTED_BOOTSTRAP_SHA256="$bootstrap_sha" \
    "$@" \
    "$INSTALL/install-phase5.sh" "$run_render" >"$tmp/last-out" 2>"$tmp/last-err"
  last_code=$?
  set -e
}
kube_uid=$(k get namespace kube-system -o jsonpath='{.metadata.uid}')
tx_uuid=99999999-9999-4999-8999-999999999999
BLAZN_PHASE5_TRANSACTION_ID=$tx_uuid "$INSTALL/render-install-phase5.sh" "$tmp/render"
install_sha=$(sha256sum "$tmp/render/install.yaml" | awk '{print $1}')
rbac_sha=$(sha256sum "$tmp/render/production-rbac.yaml" | awk '{print $1}')
bootstrap_sha=$(sha256sum "$tmp/render/bootstrap.yaml" | awk '{print $1}')

run_install "$tmp/render" "$tx_root/$tx_prefix-early"
[ "$last_code" -eq 1 ] || { cat "$tmp/last-err" >&2; exit 1; }
grep -Fq 'boundary is not installed' "$tmp/last-err"

BLAZN_PHASE5_TRANSACTION_ID=$tx_uuid BLAZN_EXISTING_CLUSTER_QUEUE=m1-light "$BOUNDARY/render-boundary.sh" "$tmp/boundary.yaml" >/dev/null
k apply --server-side --field-manager blazn-phase5-boundary -f "$tmp/boundary.yaml" >/dev/null

# Crash after install-applied, then resume the durable phase to completion.
run_install "$tmp/render" "$tx_root/$tx_prefix-main" BLAZN_PHASE4C_FAIL_AFTER=install-applied BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ] || { cat "$tmp/last-err" >&2; exit 1; }
[ "$(cat "$tx_root/$tx_prefix-main/phase")" = install-applied ]
run_install "$tmp/render" "$tx_root/$tx_prefix-main"
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
[ "$(cat "$tx_root/$tx_prefix-main/phase")" = complete ]
run_install "$tmp/render" "$tx_root/$tx_prefix-main"
[ "$last_code" -eq 0 ]
grep -Fq 'already complete' "$tmp/last-out"
if ! k get serviceaccount blazn-agent-sandbox-ca-bootstrap -n agent-sandbox-system >/dev/null 2>&1; then :; else printf 'bootstrap privilege was not removed\n' >&2; exit 1; fi

# A real adapter-shaped Sandbox is admitted, gated by Kueue, and runs with
# exactly the requested reservation on the eligible node.
controller=system:serviceaccount:blazn-poc-system:blazn-sandbox-controller
k create serviceaccount blazn-sandbox-controller -n blazn-poc-system >/dev/null
k create role sandbox-editor -n blazn-poc-sandboxes --verb=create,get,patch,update,delete --resource=sandboxes.agents.x-k8s.io,sandboxes.agents.x-k8s.io/status >/dev/null
k create rolebinding sandbox-editor -n blazn-poc-sandboxes --role=sandbox-editor --serviceaccount=blazn-poc-system:blazn-sandbox-controller >/dev/null
BLAZN_TEST_MAIN_IMAGE=$SYNTHETIC_IMAGE BLAZN_TEST_MAIN_COMMAND='["sh","-c","trap : TERM INT; sleep 3600 & wait"]' python3 "$BOUNDARY/good-sandbox.py" minimal >"$tmp/sandbox.json"
sandbox_name=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["metadata"]["name"])' "$tmp/sandbox.json")
attempt=0
until k create --as="$controller" -f "$tmp/sandbox.json" >/dev/null 2>"$tmp/create.err"; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 20 ] || { printf 'Sandbox creation was denied:\n' >&2; cat "$tmp/create.err" >&2; exit 1; }
  sleep 3
done
attempt=0
until [ "$(k get pods -n blazn-poc-sandboxes -l "blazn.dev/sandbox-id=$sandbox_name" -o jsonpath='{.items[0].status.phase}' 2>/dev/null)" = Running ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 60 ] || { printf 'sandbox Pod never ran\n' >&2; k get pods -n blazn-poc-sandboxes -o wide >&2 || :; k get events -n blazn-poc-sandboxes >&2 | tail -20 || :; exit 1; }
  sleep 3
done
pod_node=$(k get pods -n blazn-poc-sandboxes -l "blazn.dev/sandbox-id=$sandbox_name" -o jsonpath='{.items[0].spec.nodeName}')
[ "node/$pod_node" = "$node" ] || { printf 'Pod ran on %s, not the eligible node\n' "$pod_node" >&2; exit 1; }
workload=$(k get workloads.kueue.x-k8s.io -n blazn-poc-sandboxes -o json)
printf '%s' "$workload" | jq -e '.items | length == 1' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].status.conditions[] | select(.type=="Admitted") | .status == "True"' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].status.admission.clusterQueue == "m1-light"' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].spec.podSets[0].template.spec.containers[0].resources.requests == {"cpu":"100m","memory":"64Mi","ephemeral-storage":"64Mi"}' >/dev/null
printf '%s' "$workload" | jq -e --arg name "$sandbox_name" '.items[0].metadata.labels["blazn.dev/sandbox-id"] == $name' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].metadata.labels["blazn.dev/managed"] == "true"' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].metadata.labels["blazn.dev/workspace"] == "32000000-0000-4000-8000-000000000002"' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].metadata.labels["blazn.dev/owner"] == "33000000-0000-4000-8000-000000000003"' >/dev/null
printf '%s' "$workload" | jq -e '.items[0].status.admission.podSetAssignments[0].resourceUsage == {"cpu":"100m","memory":"64Mi","ephemeral-storage":"64Mi"}' >/dev/null
sandbox_uid=$(k get sandbox.agents.x-k8s.io "$sandbox_name" -n blazn-poc-sandboxes -o jsonpath='{.metadata.uid}')
pod_uid=$(k get pods -n blazn-poc-sandboxes -l "blazn.dev/sandbox-id=$sandbox_name" -o jsonpath='{.items[0].metadata.uid}')
workload_uid=$(printf '%s' "$workload" | jq -r '.items[0].metadata.uid')
printf 'placement node=%s podUid=%s sandboxUid=%s workloadUid=%s\n' "$pod_node" "$pod_uid" "$sandbox_uid" "$workload_uid"

# Teardown: delete, release the artifact finalizer as the controller, and
# prove everything cascades before rolling the installation back.
k delete sandbox.agents.x-k8s.io "$sandbox_name" -n blazn-poc-sandboxes --as="$controller" --wait=false >/dev/null
k patch sandbox.agents.x-k8s.io "$sandbox_name" -n blazn-poc-sandboxes --as="$controller" --type merge -p '{"metadata":{"finalizers":[]}}' >/dev/null
attempt=0
until [ "$(k get sandboxes.agents.x-k8s.io -n blazn-poc-sandboxes --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && [ "$(k get pods -n blazn-poc-sandboxes --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && [ "$(k get workloads.kueue.x-k8s.io -n blazn-poc-sandboxes --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 40 ] || { printf 'sandbox residue was not cleaned\n' >&2; exit 1; }
  sleep 3
done

set +e
"$ROOT/phase4c/with-live-lock.sh" env PATH="$tmp:$PATH" KUBECONFIG="$KUBECONFIG" \
  BLAZN_EXPECTED_CONTEXT="kind-$cluster" \
  BLAZN_EXPECTED_KUBE_SYSTEM_UID="$kube_uid" \
  BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
  BLAZN_PHASE5_TRANSACTION_DIR="$tx_root/$tx_prefix-main" \
  BLAZN_PHASE5_TRANSACTION_ID="$tx_uuid" \
  "$INSTALL/rollback-phase5.sh" >"$tmp/rollback-out" 2>"$tmp/rollback-err"
rollback_code=$?
set -e
[ "$rollback_code" -eq 0 ] || { cat "$tmp/rollback-err" >&2; exit 1; }
[ "$(cat "$tx_root/$tx_prefix-main/phase")" = rollback-complete ]
for gone in namespace/agent-sandbox-system crd/sandboxes.agents.x-k8s.io clusterrole/blazn-agent-sandbox-observer; do
  gone_kind=${gone%%/*}; gone_name=${gone#*/}
  [ -z "$(k get "$gone_kind" "$gone_name" --ignore-not-found -o name)" ] || { printf '%s survived rollback\n' "$gone" >&2; exit 1; }
done

printf 'Phase 5 installation full-stack rehearsal passed\n'
