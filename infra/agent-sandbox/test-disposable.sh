#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/versions.env"
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
command -v docker >/dev/null 2>&1 || { printf 'docker is required\n' >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { printf 'curl is required\n' >&2; exit 1; }
docker_cmd=docker
if ! docker info >/dev/null 2>&1; then
  sudo -n docker info >/dev/null 2>&1 || { printf 'Docker access or passwordless sudo is required\n' >&2; exit 1; }
  docker_cmd='sudo docker'
fi

cluster_suffix=$(tr -d '-' </proc/sys/kernel/random/uuid | cut -c1-12)
cluster=blazn-as-v056-$cluster_suffix
case "$cluster" in blazn-as-v056-[a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9]) ;; *) exit 90 ;; esac
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-as-disposable.XXXXXX")
chmod 0700 "$tmp"
cluster_absence_verified=0
creation_attempted=0
cleanup() {
  if [ -x "$tmp/kind" ] && [ "$cluster_absence_verified" -eq 1 ] && [ "$creation_attempted" -eq 1 ]; then
    if [ "$docker_cmd" = docker ]; then "$tmp/kind" delete cluster --name "$cluster" >/dev/null 2>&1 || true; else sudo "$tmp/kind" delete cluster --name "$cluster" >/dev/null 2>&1 || true; fi
  fi
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

curl -fsSL "$KIND_URL" -o "$tmp/kind"
printf '%s  %s\n' "$KIND_SHA256" "$tmp/kind" | sha256sum -c - >/dev/null
chmod 0755 "$tmp/kind"
curl -fsSL "$AGENT_SANDBOX_MANIFEST_URL" -o "$tmp/agent-sandbox.yaml"
printf '%s  %s\n' "$AGENT_SANDBOX_MANIFEST_SHA256" "$tmp/agent-sandbox.yaml" | sha256sum -c - >/dev/null
curl -fsSL "$KUEUE_MANIFEST_URL" -o "$tmp/kueue.yaml"
printf '%s  %s\n' "$KUEUE_MANIFEST_SHA256" "$tmp/kueue.yaml" | sha256sum -c - >/dev/null
pin_controller_images "$tmp/agent-sandbox.yaml" "$tmp/kueue.yaml"

if [ "$docker_cmd" = docker ]; then existing_clusters=$("$tmp/kind" get clusters); else existing_clusters=$(sudo "$tmp/kind" get clusters); fi
if printf '%s\n' "$existing_clusters" | grep -Fx "$cluster" >/dev/null; then
  printf 'refusing to reuse existing kind cluster: %s\n' "$cluster" >&2
  exit 1
fi
cluster_absence_verified=1
creation_attempted=1
if [ "$docker_cmd" = docker ]; then "$tmp/kind" create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --kubeconfig "$tmp/kubeconfig" --wait 180s; else sudo "$tmp/kind" create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --kubeconfig "$tmp/kubeconfig" --wait 180s; fi
node=$cluster-control-plane
kctl() { $docker_cmd exec "$node" kubectl "$@"; }
kapply() { $docker_cmd exec -i "$node" kubectl apply --server-side -f -; }

kapply <"$tmp/kueue.yaml" >/dev/null
kctl wait --for=condition=Available deployment/kueue-controller-manager -n kueue-system --timeout=180s
[ "$(kctl get deployment kueue-controller-manager -n kueue-system -o jsonpath='{.spec.template.spec.containers[0].image}')" = "$KUEUE_IMAGE" ]
kueue_image_id=$(kctl get pod -n kueue-system -l control-plane=controller-manager -o jsonpath='{.items[0].status.containerStatuses[0].imageID}')
image_id_matches "$kueue_image_id" "$KUEUE_IMAGE"
attempt=0
while [ -z "$(kctl get endpoints kueue-webhook-service -n kueue-system -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)" ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 60 ] || { printf 'Kueue webhook did not publish an endpoint\n' >&2; exit 1; }
  sleep 2
done
kapply <"$tmp/agent-sandbox.yaml" >/dev/null
kctl wait --for=condition=Available deployment/agent-sandbox-controller -n agent-sandbox-system --timeout=180s
[ "$(kctl get deployment agent-sandbox-controller -n agent-sandbox-system -o jsonpath='{.spec.template.spec.containers[0].image}')" = "$AGENT_SANDBOX_IMAGE" ]
agent_image_id=$(kctl get pod -n agent-sandbox-system -l app=agent-sandbox-controller -o jsonpath='{.items[0].status.containerStatuses[0].imageID}')
image_id_matches "$agent_image_id" "$AGENT_SANDBOX_IMAGE"
[ "$(kctl get crd -o name | grep -c agents.x-k8s.io)" -eq 4 ]
secret_access=$(kctl auth can-i --as=system:serviceaccount:agent-sandbox-system:agent-sandbox-controller list secrets --all-namespaces || true)
[ "$secret_access" = no ]
pod_delete=$(kctl auth can-i --as=system:serviceaccount:agent-sandbox-system:agent-sandbox-controller delete pods --all-namespaces || true)
[ "$pod_delete" = yes ]

# Exercise the Phase 4B adapter through the real v1beta1 API in a separate
# Blazn-owned namespace and Kueue LocalQueue. The proxy is reachable only on
# this uniquely owned disposable kind bridge and dies with the cluster.
kctl create namespace blazn-poc-sandboxes >/dev/null
kctl create serviceaccount blazn-sandbox-runner -n blazn-poc-sandboxes >/dev/null
kctl label node "$node" blazn.dev/sandbox-eligible=true --overwrite >/dev/null
cat <<EOF | kapply >/dev/null
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: blazn-adapter-$cluster_suffix
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: blazn-adapter-$cluster_suffix
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: blazn-poc-sandboxes
  resourceGroups:
  - coveredResources: ["cpu", "memory"]
    flavors:
    - name: blazn-adapter-$cluster_suffix
      resources:
      - name: cpu
        nominalQuota: "1"
      - name: memory
        nominalQuota: 1Gi
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: blazn-poc
  namespace: blazn-poc-sandboxes
spec:
  clusterQueue: blazn-adapter-$cluster_suffix
EOF
$docker_cmd exec -d "$node" kubectl proxy --address=0.0.0.0 --accept-hosts='.*' --port=8001
node_ip=$($docker_cmd inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$node")
attempt=0
until curl -fsS "http://$node_ip:8001/version" >/dev/null; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 30 ] || { printf 'disposable Kubernetes API proxy did not become ready\n' >&2; exit 1; }
  sleep 1
done
$docker_cmd run --rm --network kind \
    -v "$(CDPATH='' cd -- "$ROOT/../.." && pwd):/src:ro" -w /src \
    -e "BLAZN_SANDBOX_KIND_PROXY_URL=http://$node_ip:8001" \
    -e "BLAZN_SANDBOX_KIND_IMAGE=$SYNTHETIC_IMAGE" \
    -e "BLAZN_SANDBOX_KIND_SUFFIX=$cluster_suffix" \
    "$GO_TEST_IMAGE" go test ./internal/sandboxcontrol -run '^TestDisposableKindLifecycle$' -count=1 -v
[ "$(kctl get sandbox -n blazn-poc-sandboxes --no-headers 2>/dev/null | wc -l)" -eq 0 ]
[ "$(kctl get pod -n blazn-poc-sandboxes --no-headers 2>/dev/null | wc -l)" -eq 0 ]
[ "$(kctl get workload -n blazn-poc-sandboxes --no-headers 2>/dev/null | wc -l)" -eq 0 ]
kctl delete namespace blazn-poc-sandboxes --wait=true --timeout=120s >/dev/null
kctl delete clusterqueue "blazn-adapter-$cluster_suffix" --wait=true --timeout=120s >/dev/null
kctl delete resourceflavor "blazn-adapter-$cluster_suffix" --wait=true --timeout=120s >/dev/null
kctl label node "$node" blazn.dev/sandbox-eligible- >/dev/null

sed \
  -e "s|SYNTHETIC_IMAGE|$SYNTHETIC_IMAGE|" \
  "$ROOT/synthetic-sandbox.yaml" | kapply >/dev/null
kctl wait --for=condition=Ready sandbox/synthetic -n blazn-spike --timeout=180s
[ "$(kctl get pod synthetic -n blazn-spike -o jsonpath='{.status.phase}')" = Running ]
[ "$(kctl get pod synthetic -n blazn-spike -o jsonpath='{.metadata.labels.kueue\.x-k8s\.io/queue-name}')" = blazn-spike ]
[ "$(kctl get workload -n blazn-spike -o jsonpath='{.items[0].status.admission.clusterQueue}')" = blazn-spike ]
[ "$(kctl get workload -n blazn-spike -o jsonpath='{.items[0].status.conditions[?(@.type=="Admitted")].status}')" = True ]
[ "$(kctl get workload -n blazn-spike -o jsonpath='{.items[0].status.admission.podSetAssignments[0].resourceUsage.cpu}')" = 100m ]
[ "$(kctl get workload -n blazn-spike -o jsonpath='{.items[0].status.admission.podSetAssignments[0].resourceUsage.memory}')" = 64Mi ]

kctl delete sandbox synthetic -n blazn-spike --wait=true --timeout=120s
kctl wait --for=delete pod/synthetic -n blazn-spike --timeout=120s
kctl wait --for=delete workload --all -n blazn-spike --timeout=120s
[ "$(kctl get workload -n blazn-spike --no-headers 2>/dev/null | wc -l)" -eq 0 ]
kctl delete namespace blazn-spike --wait=true --timeout=120s
kctl delete clusterqueue blazn-spike --ignore-not-found >/dev/null
kctl delete resourceflavor blazn-default --ignore-not-found >/dev/null
$docker_cmd exec -i "$node" kubectl delete -f - --ignore-not-found --wait=true --timeout=180s <"$tmp/agent-sandbox.yaml" >/dev/null
[ "$(kctl get crd -o name | grep -c agents.x-k8s.io || true)" -eq 0 ]
[ "$(kctl get clusterrole,clusterrolebinding -o name | grep -c agent-sandbox || true)" -eq 0 ]
[ "$(kctl get mutatingwebhookconfiguration,validatingwebhookconfiguration -o name | grep -c agent-sandbox || true)" -eq 0 ]
[ "$(kctl get namespace agent-sandbox-system --ignore-not-found -o name | wc -l)" -eq 0 ]
$docker_cmd exec -i "$node" kubectl delete -f - --ignore-not-found --wait=true --timeout=240s <"$tmp/kueue.yaml" >/dev/null
[ "$(kctl get crd -o name | grep -c kueue.x-k8s.io || true)" -eq 0 ]
[ "$(kctl get clusterrole,clusterrolebinding -o name | grep -c kueue || true)" -eq 0 ]
[ "$(kctl get apiservice -o name | grep -c visibility.kueue.x-k8s.io || true)" -eq 0 ]
[ "$(kctl get mutatingwebhookconfiguration,validatingwebhookconfiguration -o name | grep -c kueue || true)" -eq 0 ]
[ "$(kctl get clusterqueue blazn-spike --ignore-not-found -o name | wc -l)" -eq 0 ]
[ "$(kctl get resourceflavor blazn-default --ignore-not-found -o name | wc -l)" -eq 0 ]
[ "$(kctl get namespace blazn-poc-sandboxes --ignore-not-found -o name | wc -l)" -eq 0 ]
[ "$(kctl get clusterqueue "blazn-adapter-$cluster_suffix" --ignore-not-found -o name | wc -l)" -eq 0 ]
[ "$(kctl get resourceflavor "blazn-adapter-$cluster_suffix" --ignore-not-found -o name | wc -l)" -eq 0 ]
[ "$(kctl get namespace kueue-system --ignore-not-found -o name | wc -l)" -eq 0 ]

if [ "$docker_cmd" = docker ]; then "$tmp/kind" delete cluster --name "$cluster"; else sudo "$tmp/kind" delete cluster --name "$cluster"; fi
[ "$($docker_cmd ps -a --filter "name=$cluster" -q | wc -l)" -eq 0 ]
[ "$($docker_cmd network ls --format '{{.Name}}' | grep -c "$cluster" || true)" -eq 0 ]
[ "$($docker_cmd volume ls --format '{{.Name}}' | grep -c "$cluster" || true)" -eq 0 ]
creation_attempted=0
trap - EXIT HUP INT TERM
cleanup
printf 'Disposable Agent Sandbox/Kueue lifecycle and residue test passed\n'
