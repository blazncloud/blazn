#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
# shellcheck disable=SC1091
. "$ROOT/../versions.env"
[ "$(id -u)" -eq 0 ] || { printf 'Kueue integration upgrade must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s KUEUE_CHART_TGZ\n' "$0" >&2; exit 64; }
chart=$1
phase4c_require_mutation_authority
: "${LIVE_KUEUE_CHART_SHA256:?versions.env must pin the reviewed Kueue chart digest}"
: "${LIVE_KUEUE_POD_CONFIG_SHA256:?versions.env must pin the reviewed Kueue Pod config digest}"
: "${LIVE_KUEUE_WEBHOOK_PATCH_SHA256:?versions.env must pin the reviewed webhook patch digest}"
: "${BLAZN_KUEUE_TRANSACTION_DIR:?set a durable transaction directory}"
: "${BLAZN_EXPECTED_KUEUE_REVISION:?set reviewed Helm revision}"
: "${BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256:?set reviewed Helm manifest digest}"
: "${BLAZN_EXPECTED_KUEUE_CONFIG_SHA256:?set reviewed Kueue config digest}"
: "${BLAZN_EXPECTED_WORKLOADS:?set reviewed total Workload count}"
case "$BLAZN_EXPECTED_KUEUE_REVISION:$BLAZN_EXPECTED_WORKLOADS" in *[!0-9:]*) printf 'reviewed Kueue counters must be numeric\n' >&2; exit 1 ;; esac
for required in helm jq patch; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
transaction=$BLAZN_KUEUE_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase4c/kueue-pod-*) ;; *) printf 'Kueue transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase4c/}
case "$transaction_name" in */*|*..*|'') printf 'Kueue transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac
release_description="blazn-phase4c:$transaction_name"

write_phase() { phase4c_write_phase "$transaction" "$1"; }
workload_identities() { kubectl get workloads.kueue.x-k8s.io -A -o json | jq -S '[.items[] | {uid:.metadata.uid,namespace:.metadata.namespace,name:.metadata.name}] | sort_by(.uid)'; }
namespace_absent() { discovered=$(kubectl get namespace "$1" --ignore-not-found -o name) || return 2; [ -z "$discovered" ]; }
both_pod_namespaces_absent() { namespace_absent blazn-poc && namespace_absent blazn-poc-sandboxes; }
live_config_sha() { kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}' | sha256sum | awk '{print $1}'; }
live_release_revision() { helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0].revision'; }
live_release_description() { helm -n kueue-system status kueue -o json | jq -er '.info.description'; }

if [ ! -e "$transaction" ]; then
  if ! { [ -f "$chart" ] && [ ! -L "$chart" ] && [ "$(stat -c '%h' "$chart")" = 1 ]; }; then printf 'Kueue chart file is unsafe\n' >&2; exit 1; fi
  install -d -o root -g root -m 0700 "$transaction"
  install -o root -g root -m 0400 "$chart" "$transaction/upstream-kueue-0.14.3.tgz"
  install -o root -g root -m 0400 "$ROOT/kueue-pod-config.yaml" "$transaction/controller-manager.yaml"
  install -o root -g root -m 0400 "$ROOT/kueue-pod-webhook-selector.patch" "$transaction/webhook-selector.patch"
  write_phase sealed
fi
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'Kueue transaction directory is unsafe\n' >&2; exit 1; fi
sealed_chart=$transaction/upstream-kueue-0.14.3.tgz; sealed_config=$transaction/controller-manager.yaml; sealed_patch=$transaction/webhook-selector.patch
for sealed_input in "$sealed_chart" "$sealed_config" "$sealed_patch"; do [ "$(stat -c '%u:%a:%h' "$sealed_input")" = 0:400:1 ] || { printf 'sealed Kueue input is unsafe\n' >&2; exit 1; }; done
[ "$(sha256sum "$sealed_chart" | awk '{print $1}')" = "$LIVE_KUEUE_CHART_SHA256" ] || { printf 'Kueue chart checksum mismatch\n' >&2; exit 1; }
[ "$(sha256sum "$sealed_config" | awk '{print $1}')" = "$LIVE_KUEUE_POD_CONFIG_SHA256" ] || { printf 'Kueue configuration checksum mismatch\n' >&2; exit 1; }
[ "$(sha256sum "$sealed_patch" | awk '{print $1}')" = "$LIVE_KUEUE_WEBHOOK_PATCH_SHA256" ] || { printf 'Kueue chart patch checksum mismatch\n' >&2; exit 1; }
phase=$(cat "$transaction/phase")
case "$phase" in complete) printf 'Kueue Pod integration transaction is already complete\n'; exit 0 ;; rollback-complete) printf 'Kueue Pod integration transaction was rolled back; use a new transaction\n' >&2; exit 1 ;; sealed|prepared|upgrade-intent|upgraded) ;; *) printf 'Kueue transaction phase is invalid\n' >&2; exit 1 ;; esac

if [ "$phase" = sealed ]; then
  if ! both_pod_namespaces_absent; then printf 'reviewed Pod namespaces are present or could not be verified before Kueue integration changes\n' >&2; exit 1; fi
  current_revision=$(helm -n kueue-system list -f '^kueue$' -o json | jq -er '.[0] | select(.chart=="kueue-0.14.3" and .app_version=="v0.14.3" and .status=="deployed") | .revision')
  [ "$current_revision" = "$BLAZN_EXPECTED_KUEUE_REVISION" ] || { printf 'Kueue Helm revision changed\n' >&2; exit 1; }
  helm -n kueue-system get manifest kueue >"$transaction/prior-manifest.yaml"; chmod 0400 "$transaction/prior-manifest.yaml"
  [ "$(sha256sum "$transaction/prior-manifest.yaml" | awk '{print $1}')" = "$BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256" ] || { printf 'Kueue manifest digest changed\n' >&2; exit 1; }
  [ "$(live_config_sha)" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] || { printf 'Kueue manager config digest changed\n' >&2; exit 1; }
  workload_identities >"$transaction/prior-workloads.json"; chmod 0400 "$transaction/prior-workloads.json"
  [ "$(jq 'length' "$transaction/prior-workloads.json")" = "$BLAZN_EXPECTED_WORKLOADS" ] || { printf 'Workload baseline changed\n' >&2; exit 1; }
  printf '%s\n' "$current_revision" >"$transaction/prior-revision"; chmod 0400 "$transaction/prior-revision"
  [ ! -L "$transaction/chart-source" ] || { printf 'Kueue chart-source location is unsafe\n' >&2; exit 1; }
  if [ -d "$transaction/chart-source" ]; then find "$transaction/chart-source" -mindepth 1 -xdev -delete; else mkdir -m 0700 "$transaction/chart-source"; fi
  chmod 0700 "$transaction/chart-source"
  [ ! -e "$transaction/kueue-0.14.3.tgz" ] || find "$transaction/kueue-0.14.3.tgz" -xdev -maxdepth 0 -delete
  tar -xzf "$sealed_chart" -C "$transaction/chart-source" --no-same-owner --no-same-permissions
  patch -s -f -d "$transaction/chart-source" -p1 <"$sealed_patch"
  helm package "$transaction/chart-source/kueue" --destination "$transaction" >/dev/null
  sha256sum "$transaction/kueue-0.14.3.tgz" | awk '{print $1}' >"$transaction/derived-chart.sha256"
  chmod 0400 "$transaction/kueue-0.14.3.tgz" "$transaction/derived-chart.sha256"
  write_phase prepared; phase=prepared
fi

derived_chart=$transaction/kueue-0.14.3.tgz
for derived_artifact in "$derived_chart" "$transaction/derived-chart.sha256" "$transaction/prior-manifest.yaml" "$transaction/prior-workloads.json" "$transaction/prior-revision"; do
  if [ -L "$derived_artifact" ] || [ ! -f "$derived_artifact" ] || [ "$(stat -c '%u:%a:%h' "$derived_artifact")" != 0:400:1 ]; then printf 'derived Kueue transaction artifact is unsafe: %s\n' "$derived_artifact" >&2; exit 1; fi
done
[ "$(sha256sum "$derived_chart" | awk '{print $1}')" = "$(cat "$transaction/derived-chart.sha256")" ] || { printf 'derived Kueue chart changed since preparation\n' >&2; exit 1; }

prior_revision=$(cat "$transaction/prior-revision"); expected_upgrade_revision=$((prior_revision + 1)); owned_revision=false
verify_prior_state() {
  helm -n kueue-system get manifest kueue >"$transaction/verified-rollback-manifest.yaml"
  cmp "$transaction/prior-manifest.yaml" "$transaction/verified-rollback-manifest.yaml" || return 1
  [ "$(live_config_sha)" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] || return 1
  workload_identities >"$transaction/verified-rollback-workloads.json"
  cmp "$transaction/prior-workloads.json" "$transaction/verified-rollback-workloads.json" || return 1
  both_pod_namespaces_absent
}
revalidate_baseline() {
  if ! both_pod_namespaces_absent; then printf 'reviewed Pod namespaces appeared or could not be verified before Kueue mutation\n' >&2; return 1; fi
  workload_identities >"$transaction/current-workloads.json"
  cmp "$transaction/prior-workloads.json" "$transaction/current-workloads.json" || { printf 'Workload identities changed before Kueue mutation\n' >&2; return 1; }
  helm -n kueue-system get manifest kueue >"$transaction/current-manifest.yaml"
  cmp "$transaction/prior-manifest.yaml" "$transaction/current-manifest.yaml" || { printf 'Kueue manifest changed before Kueue mutation\n' >&2; return 1; }
  [ "$(live_config_sha)" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] || { printf 'Kueue config changed before Kueue mutation\n' >&2; return 1; }
}
rollback_on_failure() {
  code=$?
  trap - EXIT
  trap '' HUP INT TERM
  if [ "$code" -ne 0 ] && [ "$owned_revision" = true ]; then
    if helm -n kueue-system rollback kueue "$prior_revision" --wait --timeout 300s >/dev/null && verify_prior_state; then write_phase rollback-complete; else printf 'automatic Kueue rollback or verification failed\n' >&2; fi
  fi
  exit "$code"
}
trap rollback_on_failure EXIT
trap 'exit 130' HUP INT TERM
if [ "$phase" = prepared ]; then
  revalidate_baseline || exit 1
  live_revision=$(live_release_revision); [ "$live_revision" = "$prior_revision" ] || { printf 'Kueue revision changed after preparation\n' >&2; exit 1; }
  write_phase upgrade-intent; phase=upgrade-intent
fi
if [ "$phase" = upgrade-intent ]; then
  pending_owned=$(helm -n kueue-system history kueue -o json | jq -er --argjson revision "$expected_upgrade_revision" --arg description "$release_description" '[.[] | select(.revision==$revision and .status=="pending-upgrade" and .description==$description)] | length')
  if [ "$pending_owned" = 1 ]; then
    owned_revision=true
    if helm -n kueue-system rollback kueue "$prior_revision" --wait --timeout 300s >/dev/null && verify_prior_state; then trap - EXIT HUP INT TERM; write_phase rollback-complete; printf 'owned pending Kueue upgrade rolled back; use a new transaction\n' >&2; exit 1; fi
    printf 'owned pending Kueue upgrade could not be reconciled\n' >&2; exit 1
  fi
  pending_any=$(helm -n kueue-system history kueue -o json | jq -er --argjson revision "$expected_upgrade_revision" '[.[] | select(.revision==$revision and .status=="pending-upgrade")] | length')
  [ "$pending_any" = 0 ] || { printf 'an unowned Kueue upgrade is pending\n' >&2; exit 1; }
  live_revision=$(live_release_revision)
  if [ "$live_revision" = "$prior_revision" ]; then
    revalidate_baseline || exit 1
    helm upgrade kueue "$derived_chart" -n kueue-system --reuse-values --set-file managerConfig.controllerManagerConfigYaml="$sealed_config" --description "$release_description" --atomic --wait --timeout 300s >/dev/null
    live_revision=$(live_release_revision)
  elif [ "$live_revision" -gt "$expected_upgrade_revision" ]; then
    helm -n kueue-system get manifest kueue >"$transaction/rollback-manifest.yaml"
    rollback_description=$(live_release_description)
    workload_identities >"$transaction/rollback-workloads.json"
    if [ "$rollback_description" = "Rollback to $prior_revision" ] && cmp "$transaction/prior-manifest.yaml" "$transaction/rollback-manifest.yaml" && [ "$(live_config_sha)" = "$BLAZN_EXPECTED_KUEUE_CONFIG_SHA256" ] && cmp "$transaction/prior-workloads.json" "$transaction/rollback-workloads.json" && verify_prior_state; then trap - EXIT HUP INT TERM; write_phase rollback-complete; printf 'Kueue atomic rollback reconciled; use a new transaction\n' >&2; exit 1; fi
    printf 'Kueue revision cannot be reconciled with the transaction\n' >&2; exit 1
  elif [ "$live_revision" != "$expected_upgrade_revision" ]; then printf 'Kueue revision cannot be reconciled with the transaction\n' >&2; exit 1; fi
  if ! { [ "$live_revision" = "$expected_upgrade_revision" ] && [ "$(live_release_description)" = "$release_description" ]; }; then printf 'live Kueue revision is not owned by this transaction\n' >&2; exit 1; fi
  owned_revision=true
  write_phase upgraded; phase=upgraded; upgraded_verified=true
fi
if [ "$phase" = upgraded ] && [ "${upgraded_verified:-false}" != true ]; then
  if ! { [ "$(live_release_revision)" = "$expected_upgrade_revision" ] && [ "$(live_release_description)" = "$release_description" ]; }; then printf 'upgraded Kueue revision ownership changed\n' >&2; exit 1; fi
  owned_revision=true
fi
kubectl wait deployment/kueue-controller-manager -n kueue-system --for=condition=Available --timeout=180s >/dev/null
configured=$(kubectl -n kueue-system get configmap kueue-manager-config -o jsonpath='{.data.controller_manager_config\.yaml}')
printf '%s\n' "$configured" | grep -Eq -- '^[[:space:]]*- pod$'
printf '%s\n' "$configured" | grep -Eq -- '^[[:space:]]*- blazn-poc$'
printf '%s\n' "$configured" | grep -Eq -- '^[[:space:]]*- blazn-poc-sandboxes$'
printf '%s\n' "$configured" | grep -Fq 'podOptions:'
[ "$(printf '%s\n' "$configured" | grep -cE -- '^[[:space:]]*- blazn-poc(-sandboxes)?$')" = 2 ] || { printf 'deployed Kueue selector namespaces differ from the reviewed pair\n' >&2; exit 1; }
pod_selector='{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["blazn-poc","blazn-poc-sandboxes"]}]}'
kubectl get mutatingwebhookconfiguration kueue-mutating-webhook-configuration -o json | jq -e --argjson selector "$pod_selector" '.webhooks[] | select(.name=="mpod.kb.io") | .failurePolicy=="Fail" and .namespaceSelector==$selector' >/dev/null
kubectl get validatingwebhookconfiguration kueue-validating-webhook-configuration -o json | jq -e --argjson selector "$pod_selector" '.webhooks[] | select(.name=="vpod.kb.io") | .failurePolicy=="Fail" and .namespaceSelector==$selector' >/dev/null
attempt=0
while [ "$attempt" -lt 10 ]; do workload_identities >"$transaction/current-workloads.json"; cmp "$transaction/prior-workloads.json" "$transaction/current-workloads.json" && break; attempt=$((attempt + 1)); sleep 1; done
cmp "$transaction/prior-workloads.json" "$transaction/current-workloads.json" || { printf 'Kueue created or replaced a Workload during integration enablement\n' >&2; exit 1; }
if ! both_pod_namespaces_absent; then printf 'reviewed Pod namespaces appeared or could not be verified during Kueue integration change\n' >&2; exit 1; fi
trap - EXIT HUP INT TERM
write_phase complete
printf 'Kueue Pod integration enabled only for the two reviewed Blazn namespaces\n'
