#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'Kueue integration upgrade must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s KUEUE_CHART_TGZ\n' "$0" >&2; exit 64; }
chart=$1
phase4c_require_mutation_authority
: "${BLAZN_KUEUE_TRANSACTION_DIR:?set a durable transaction directory}"
: "${BLAZN_EXPECTED_KUEUE_REVISION:?set reviewed Helm revision}"
: "${BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256:?set reviewed Helm manifest digest}"
: "${BLAZN_EXPECTED_KUEUE_CONFIG_SHA256:?set reviewed Kueue config digest}"
: "${BLAZN_EXPECTED_WORKLOADS:?set reviewed total Workload count}"
case "$BLAZN_EXPECTED_KUEUE_REVISION:$BLAZN_EXPECTED_WORKLOADS" in *[!0-9:]*) printf 'reviewed Kueue counters must be numeric\n' >&2; exit 1 ;; esac
for required in helm jq patch; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
transaction=$BLAZN_KUEUE_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase4c/kueue-pod-*) ;; *) printf 'Kueue transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=$(basename "$transaction")
release_description="blazn-phase4c:$transaction_name"

write_phase() { next=$1; temporary=$(mktemp "$transaction/.phase.XXXXXX"); printf '%s\n' "$next" >"$temporary"; chmod 0600 "$temporary"; sync -f "$temporary"; mv "$temporary" "$transaction/phase"; sync -f "$transaction"; }
workload_identities() { kubectl get workloads.kueue.x-k8s.io -A -o json | jq -S '[.items[] | {uid:.metadata.uid,namespace:.metadata.namespace,name:.metadata.name}] | sort_by(.uid)'; }

if [ ! -e "$transaction" ]; then
  [ -f "$chart" ] && [ ! -L "$chart" ] && [ "$(stat -c '%h' "$chart")" = 1 ] || { printf 'Kueue chart file is unsafe\n' >&2; exit 1; }
  install -d -o root -g root -m 0700 "$transaction"
  install -o root -g root -m 0400 "$chart" "$transaction/upstream-kueue-0.14.3.tgz"
  install -o root -g root -m 0400 "$ROOT/kueue-pod-config.yaml" "$transaction/controller-manager.yaml"
  install -o root -g root -m 0400 "$ROOT/kueue-pod-webhook-selector.patch" "$transaction/webhook-selector.patch"
  write_phase sealed
fi
[ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ] || { printf 'Kueue transaction directory is unsafe\n' >&2; exit 1; }
sealed_chart=$transaction/upstream-kueue-0.14.3.tgz; sealed_config=$transaction/controller-manager.yaml; sealed_patch=$transaction/webhook-selector.patch
for sealed_input in "$sealed_chart" "$sealed_config" "$sealed_patch"; do [ "$(stat -c '%u:%a:%h' "$sealed_input")" = 0:400:1 ] || { printf 'sealed Kueue input is unsafe\n' >&2; exit 1; }; done
[ "$(sha256sum "$sealed_chart" | awk '{print $1}')" = 314d2b21e9a7ea6a31fc7fed1cf7db825e62ce11ad2a849e2b8b450213b9ba09 ] || { printf 'Kueue chart checksum mismatch\n' >&2; exit 1; }
[ "$(sha256sum "$sealed_config" | awk '{print $1}')" = 0f26fd3a1097b6f879504931d14757f3fd8f81f6996ef58c5c59ed0b09aab9e0 ] || { printf 'Kueue configuration checksum mismatch\n' >&2; exit 1; }
[ "$(sha256sum "$sealed_patch" | awk '{print $1}')" = ad232c225899a6b53015213ea9c552eb77e9a4c552d51721dc813cfce16f12b7 ] || { printf 'Kueue chart patch checksum mismatch\n' >&2; exit 1; }
phase=$(cat "$transaction/phase")
case "$phase" in complete) printf 'Kueue Pod integration transaction is already complete\n'; exit 0 ;; rollback-complete) printf 'Kueue Pod integration transaction was rolled back; use a new transaction\n' >&2; exit 1 ;; sealed|prepared|upgrade-intent|upgraded) ;; *) printf 'Kueue transaction phase is invalid\n' >&2; exit 1 ;; esac

if [ "$phase" = sealed ]; then
  [ -z "$(kubectl get namespace blazn-poc --ignore-not-found -o name)" ] && [ -z "$(kubectl get namespace blazn-poc-sandboxes --ignore-not-found -o name)" ] || { printf 'reviewed Pod namespaces must be absent before Kueue integration changes\n' >&2; exit 1; }
  current_revision=$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0] | select(.chart=="kueue-0.14.3" and .app_version=="v0.14.3" and .status=="deployed") | .revision')
  [ "$current_revision" = "$BLAZN_EXPECTED_KUEUE_REVISION" ] || { printf 'Kueue Helm revision changed\n' >&2; exit 1; }
  helm -n kueue-system get manifest kueue >"$transaction/prior-manifest.yaml"; chmod 0400 "$transaction/prior-manifest.yaml"
  [ "$(sha256sum "$transaction/prior-manifest.yaml" | awk '{print $1}')" = "$BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256" ] || { printf 'Kueue manifest digest changed\n' >&2; exit 1; }
  current_config=$(kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}')
  [ "$(printf '%s' "$current_config" | sha256sum | awk '{print $1}')" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] || { printf 'Kueue manager config digest changed\n' >&2; exit 1; }
  workload_identities >"$transaction/prior-workloads.json"; chmod 0400 "$transaction/prior-workloads.json"
  [ "$(jq 'length' "$transaction/prior-workloads.json")" = "$BLAZN_EXPECTED_WORKLOADS" ] || { printf 'Workload baseline changed\n' >&2; exit 1; }
  printf '%s\n' "$current_revision" >"$transaction/prior-revision"; chmod 0400 "$transaction/prior-revision"
  if [ -d "$transaction/chart-source" ]; then find "$transaction/chart-source" -xdev -type f -delete; find "$transaction/chart-source" -xdev -depth -type d -empty -delete; fi
  [ ! -e "$transaction/kueue-0.14.3.tgz" ] || find "$transaction/kueue-0.14.3.tgz" -type f -delete
  mkdir -m 0700 "$transaction/chart-source"; tar -xzf "$sealed_chart" -C "$transaction/chart-source" --no-same-owner --no-same-permissions
  patch -s -f -d "$transaction/chart-source" -p1 <"$sealed_patch"
  helm package "$transaction/chart-source/kueue" --destination "$transaction" >/dev/null; chmod 0400 "$transaction/kueue-0.14.3.tgz"
  write_phase prepared; phase=prepared
fi

prior_revision=$(cat "$transaction/prior-revision"); expected_upgrade_revision=$((prior_revision + 1)); owned_revision=false
rollback_on_failure() { code=$?; trap - EXIT HUP INT TERM; if [ "$code" -ne 0 ] && [ "$owned_revision" = true ]; then if helm -n kueue-system rollback kueue "$prior_revision" --wait --timeout 300s >/dev/null; then write_phase rollback-complete; else printf 'automatic Kueue rollback failed\n' >&2; fi; fi; exit "$code"; }
trap rollback_on_failure EXIT
trap 'exit 130' HUP INT TERM
if [ "$phase" = prepared ]; then
  [ -z "$(kubectl get namespace blazn-poc --ignore-not-found -o name)" ] && [ -z "$(kubectl get namespace blazn-poc-sandboxes --ignore-not-found -o name)" ] || { printf 'reviewed Pod namespaces appeared after preparation\n' >&2; exit 1; }
  workload_identities >"$transaction/current-workloads.json"; cmp "$transaction/prior-workloads.json" "$transaction/current-workloads.json" || { printf 'Workload identities changed after preparation\n' >&2; exit 1; }
  live_revision=$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0].revision'); [ "$live_revision" = "$prior_revision" ] || { printf 'Kueue revision changed after preparation\n' >&2; exit 1; }
  helm -n kueue-system get manifest kueue >"$transaction/current-manifest.yaml"; cmp "$transaction/prior-manifest.yaml" "$transaction/current-manifest.yaml" || { printf 'Kueue manifest changed after preparation\n' >&2; exit 1; }
  current_config=$(kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}'); [ "$(printf '%s' "$current_config" | sha256sum | awk '{print $1}')" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] || { printf 'Kueue config changed after preparation\n' >&2; exit 1; }
  write_phase upgrade-intent; phase=upgrade-intent
fi
if [ "$phase" = upgrade-intent ]; then
  live_revision=$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0].revision')
  if [ "$live_revision" = "$prior_revision" ]; then helm upgrade kueue "$transaction/kueue-0.14.3.tgz" -n kueue-system --reuse-values --set-file managerConfig.controllerManagerConfigYaml="$sealed_config" --description "$release_description" --atomic --wait --timeout 300s >/dev/null; live_revision=$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0].revision')
  elif [ "$live_revision" != "$expected_upgrade_revision" ]; then printf 'Kueue revision cannot be reconciled with the transaction\n' >&2; exit 1; fi
  [ "$live_revision" = "$expected_upgrade_revision" ] && [ "$(helm -n kueue-system status kueue -o json | jq -er '.info.description')" = "$release_description" ] || { printf 'live Kueue revision is not owned by this transaction\n' >&2; exit 1; }
  owned_revision=true
  write_phase upgraded; phase=upgraded
fi
if [ "$phase" = upgraded ]; then
  [ "$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0].revision')" = "$expected_upgrade_revision" ] && [ "$(helm -n kueue-system status kueue -o json | jq -er '.info.description')" = "$release_description" ] || { printf 'upgraded Kueue revision ownership changed\n' >&2; exit 1; }
  owned_revision=true
fi
kubectl wait deployment/kueue-controller-manager -n kueue-system --for=condition=Available --timeout=180s >/dev/null
configured=$(kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}')
printf '%s' "$configured" | grep -F -- '- pod' >/dev/null; printf '%s' "$configured" | grep -F -- '- blazn-poc-sandboxes' >/dev/null
pod_selector='{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["blazn-poc","blazn-poc-sandboxes"]}]}'
kubectl get mutatingwebhookconfiguration kueue-mutating-webhook-configuration -o json | jq -e --argjson selector "$pod_selector" '.webhooks[] | select(.name=="mpod.kb.io") | .failurePolicy=="Fail" and .namespaceSelector==$selector' >/dev/null
kubectl get validatingwebhookconfiguration kueue-validating-webhook-configuration -o json | jq -e --argjson selector "$pod_selector" '.webhooks[] | select(.name=="vpod.kb.io") | .failurePolicy=="Fail" and .namespaceSelector==$selector' >/dev/null
attempt=0
while [ "$attempt" -lt 10 ]; do workload_identities >"$transaction/current-workloads.json"; cmp "$transaction/prior-workloads.json" "$transaction/current-workloads.json" && break; attempt=$((attempt + 1)); sleep 1; done
cmp "$transaction/prior-workloads.json" "$transaction/current-workloads.json" || { printf 'Kueue created or replaced a Workload during integration enablement\n' >&2; exit 1; }
[ -z "$(kubectl get namespace blazn-poc --ignore-not-found -o name)" ] && [ -z "$(kubectl get namespace blazn-poc-sandboxes --ignore-not-found -o name)" ] || { printf 'reviewed Pod namespaces appeared during Kueue integration change\n' >&2; exit 1; }
write_phase complete
trap - EXIT HUP INT TERM
printf 'Kueue Pod integration enabled only for the two reviewed Blazn namespaces\n'
