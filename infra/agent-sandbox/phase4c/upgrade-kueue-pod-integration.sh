#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"

[ "$(id -u)" -eq 0 ] || { printf 'Kueue integration upgrade must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s KUEUE_CHART_TGZ\n' "$0" >&2; exit 64; }
chart=$1
phase4c_require_mutation_authority
: "${BLAZN_EXPECTED_KUEUE_REVISION:?set reviewed Helm revision}"
: "${BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256:?set reviewed Helm manifest digest}"
: "${BLAZN_EXPECTED_KUEUE_CONFIG_SHA256:?set reviewed Kueue config digest}"
: "${BLAZN_EXPECTED_ADMITTED_WORKLOADS:?set reviewed admitted Workload count}"
case "$BLAZN_EXPECTED_KUEUE_REVISION:$BLAZN_EXPECTED_ADMITTED_WORKLOADS" in *[!0-9:]*) printf 'reviewed Kueue counters must be numeric\n' >&2; exit 1 ;; esac
command -v helm >/dev/null 2>&1 || { printf 'helm is required\n' >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
[ -f "$chart" ] && [ ! -L "$chart" ] && [ "$(stat -c '%h' "$chart")" = 1 ] || { printf 'Kueue chart file is unsafe\n' >&2; exit 1; }
sealed=$(mktemp -d /tmp/blazn-kueue-sealed.XXXXXX)
chmod 0700 "$sealed"
install -o root -g root -m 0400 "$chart" "$sealed/kueue-0.14.3.tgz"
install -o root -g root -m 0400 "$ROOT/kueue-pod-config.yaml" "$sealed/controller-manager.yaml"
sealed_chart=$sealed/kueue-0.14.3.tgz
sealed_config=$sealed/controller-manager.yaml
current_manifest=''
before_workloads=''
cleanup() { [ -z "$current_manifest" ] || find "$current_manifest" -type f -delete 2>/dev/null || true; [ -z "$before_workloads" ] || find "$before_workloads" -type f -delete 2>/dev/null || true; find "$sealed" -xdev -type f -delete 2>/dev/null || true; find "$sealed" -xdev -depth -type d -empty -delete 2>/dev/null || true; }
trap cleanup EXIT HUP INT TERM
[ "$(stat -c '%u:%a:%h' "$sealed_chart")" = 0:400:1 ] && [ "$(stat -c '%u:%a:%h' "$sealed_config")" = 0:400:1 ] || { printf 'sealed Kueue inputs are unsafe\n' >&2; exit 1; }
[ "$(sha256sum "$sealed_chart" | awk '{print $1}')" = 314d2b21e9a7ea6a31fc7fed1cf7db825e62ce11ad2a849e2b8b450213b9ba09 ] || { printf 'Kueue chart checksum mismatch\n' >&2; exit 1; }
[ "$(sha256sum "$sealed_config" | awk '{print $1}')" = 0f26fd3a1097b6f879504931d14757f3fd8f81f6996ef58c5c59ed0b09aab9e0 ] || { printf 'Kueue configuration checksum mismatch\n' >&2; exit 1; }

current_revision=$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0] | select(.chart=="kueue-0.14.3" and .app_version=="v0.14.3" and .status=="deployed") | .revision')
[ "$current_revision" = "$BLAZN_EXPECTED_KUEUE_REVISION" ] || { printf 'Kueue Helm revision changed\n' >&2; exit 1; }
current_manifest=$(mktemp /tmp/blazn-kueue-manifest.XXXXXX)
before_workloads=$(mktemp /tmp/blazn-kueue-workloads.XXXXXX)
helm -n kueue-system get manifest kueue >"$current_manifest"
[ "$(sha256sum "$current_manifest" | awk '{print $1}')" = "$BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256" ] || { printf 'Kueue manifest digest changed\n' >&2; exit 1; }
current_config=$(kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}')
[ "$(printf '%s' "$current_config" | sha256sum | awk '{print $1}')" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] || { printf 'Kueue manager config digest changed\n' >&2; exit 1; }
kubectl get workloads.kueue.x-k8s.io -A -o json | jq -S '[.items[] | select(any(.status.conditions[]?; .type=="Admitted" and .status=="True")) | {uid:.metadata.uid,namespace:.metadata.namespace,name:.metadata.name,clusterQueue:.status.admission.clusterQueue}] | sort_by(.uid)' >"$before_workloads"
[ "$(jq 'length' "$before_workloads")" = "$BLAZN_EXPECTED_ADMITTED_WORKLOADS" ] || { printf 'admitted Workload baseline changed\n' >&2; exit 1; }

upgraded=false
rollback_on_failure() {
  code=$?
  trap - EXIT HUP INT TERM
  if [ "$code" -ne 0 ] && [ "$upgraded" = true ]; then
    helm -n kueue-system rollback kueue "$current_revision" --wait --timeout 300s >/dev/null || printf 'automatic Kueue rollback failed\n' >&2
  fi
  cleanup
  exit "$code"
}
trap rollback_on_failure EXIT
trap 'exit 130' HUP INT TERM
upgraded=true
helm upgrade kueue "$sealed_chart" -n kueue-system --reuse-values \
  --set-file managerConfig.controllerManagerConfigYaml="$sealed_config" \
  --atomic --wait --timeout 300s >/dev/null
kubectl wait deployment/kueue-controller-manager -n kueue-system --for=condition=Available --timeout=180s >/dev/null
configured=$(kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}')
printf '%s' "$configured" | grep -F -- '- pod' >/dev/null
printf '%s' "$configured" | grep -F -- '- blazn-poc-sandboxes' >/dev/null
pod_webhook_index=$(kubectl get mutatingwebhookconfiguration kueue-mutating-webhook-configuration -o json | jq -er '[.webhooks[].name] | to_entries | map(select(.value=="mpod.kb.io")) | select(length==1) | .[0].key')
pod_selector='{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["blazn-poc","blazn-poc-sandboxes"]}]}'
kubectl patch mutatingwebhookconfiguration kueue-mutating-webhook-configuration --type=json -p="[{\"op\":\"test\",\"path\":\"/webhooks/$pod_webhook_index/name\",\"value\":\"mpod.kb.io\"},{\"op\":\"replace\",\"path\":\"/webhooks/$pod_webhook_index/namespaceSelector\",\"value\":$pod_selector}]" >/dev/null
kubectl get mutatingwebhookconfiguration kueue-mutating-webhook-configuration -o json | jq -e --argjson selector "$pod_selector" '.webhooks[] | select(.name=="mpod.kb.io") | .namespaceSelector==$selector' >/dev/null
after_workloads=$(kubectl get workloads.kueue.x-k8s.io -A -o json | jq -S '[.items[] | select(any(.status.conditions[]?; .type=="Admitted" and .status=="True")) | {uid:.metadata.uid,namespace:.metadata.namespace,name:.metadata.name,clusterQueue:.status.admission.clusterQueue}] | sort_by(.uid)')
[ "$after_workloads" = "$(cat "$before_workloads")" ] || { printf 'existing admitted Workload identity changed\n' >&2; exit 1; }
upgraded=false
trap - EXIT HUP INT TERM
cleanup
printf 'Kueue Pod integration enabled only for the two reviewed Blazn namespaces\n'
