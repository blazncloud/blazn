#!/bin/sh
set -eu

# Executable proof for phase4c/upgrade-kueue-pod-integration.sh: crash
# injection at every journal boundary, every Helm reconcile branch, fail-closed
# discovery, post-check rollback, and a real-chart render of the sealed patch.
if [ "$(id -u)" -ne 0 ]; then exec sudo -n "$0" "$@"; fi
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/versions.env"
REAL_HELM=$(command -v helm) || { printf 'helm is required\n' >&2; exit 1; }
for required in python3 jq patch tar sha256sum flock; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
python3 -c 'import yaml' 2>/dev/null || { printf 'python3 yaml module is required\n' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-kueue-transaction.XXXXXX")
tx_root=/var/lib/blazn/phase4c
tx_prefix=kueue-pod-disposable-$$
blazn_root_created=0
test_lock_owned=0
cleanup() {
  for owned in "$tx_root/$tx_prefix"-*; do
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
  find "$tmp" -mindepth 1 -xdev -delete
  rmdir "$tmp"
}
trap cleanup EXIT HUP INT TERM
[ ! -e /run/lock/blazn ] || { printf 'live lock path unexpectedly exists; refusing to run the disposable harness here\n' >&2; exit 1; }
test_lock_owned=1
if [ ! -d "$tx_root" ]; then blazn_root_created=1; install -d -o root -g root -m 0700 /var/lib/blazn "$tx_root"; fi

# Pinned upstream chart: the same bytes the live transaction seals.
chart_tgz=$tmp/kueue-$LIVE_KUEUE_CHART_VERSION.tgz
attempt=0
while [ ! -f "$chart_tgz" ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 3 ] || { printf 'could not pull the pinned Kueue chart\n' >&2; exit 1; }
  (cd "$tmp" && "$REAL_HELM" pull "$LIVE_KUEUE_CHART_OCI" --version "$LIVE_KUEUE_CHART_VERSION" >/dev/null 2>&1) || sleep 5
done
[ "$(sha256sum "$chart_tgz" | awk '{print $1}')" = "$LIVE_KUEUE_CHART_SHA256" ] || { printf 'pulled Kueue chart checksum mismatch\n' >&2; exit 1; }

# Render proof: the sealed patch applies to the real chart, defines every
# template variable it references, pins mpod/vpod to the reviewed selector,
# changes nothing else, and embeds the sealed config verbatim.
mkdir -m 0700 "$tmp/render-patched" "$tmp/render-orig"
tar -xzf "$chart_tgz" -C "$tmp/render-patched" --no-same-owner --no-same-permissions
tar -xzf "$chart_tgz" -C "$tmp/render-orig" --no-same-owner --no-same-permissions
patch -s -f -d "$tmp/render-patched" -p1 <"$ROOT/phase4c/kueue-pod-webhook-selector.patch"
"$REAL_HELM" template kueue "$tmp/render-patched/kueue" -n kueue-system --set-file managerConfig.controllerManagerConfigYaml="$ROOT/phase4c/kueue-pod-config.yaml" >"$tmp/rendered-patched.yaml"
"$REAL_HELM" template kueue "$tmp/render-orig/kueue" -n kueue-system --set-file managerConfig.controllerManagerConfigYaml="$ROOT/phase4c/kueue-pod-config.yaml" >"$tmp/rendered-orig.yaml"
python3 - "$tmp/rendered-patched.yaml" "$tmp/rendered-orig.yaml" "$ROOT/phase4c/kueue-pod-config.yaml" "$ROOT/phase4c/kueue-live-config-baseline.yaml" "$tmp/deployed-config.yaml" "$LIVE_KUEUE_CONTROLLER_IMAGE" <<'PY'
import copy, json, sys, yaml
patched_path, orig_path, sealed_path, baseline_path, deployed_path, pinned_image = sys.argv[1:7]
selector = {"matchExpressions": [{"key": "kubernetes.io/metadata.name", "operator": "In",
             "values": ["blazn-poc", "blazn-poc-sandboxes"]}]}
def load(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]
patched, orig = load(patched_path), load(orig_path)
def key(doc):
    return (doc.get("kind"), doc.get("metadata", {}).get("name"))
patched_by, orig_by = {key(d): d for d in patched}, {key(d): d for d in orig}
assert set(patched_by) == set(orig_by), "patched render changed the document set"
pod_hooks = 0
pinned_images = 0
for k in patched_by:
    p, o = copy.deepcopy(patched_by[k]), copy.deepcopy(orig_by[k])
    if k[0] in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
        for pw, ow in zip(p["webhooks"], o["webhooks"]):
            assert pw["name"] == ow["name"], "webhook order changed"
            if pw["name"] in ("mpod.kb.io", "vpod.kb.io"):
                assert pw["namespaceSelector"] == selector, f'{pw["name"]} selector {pw["namespaceSelector"]}'
                assert pw["failurePolicy"] == "Fail", f'{pw["name"]} failurePolicy {pw["failurePolicy"]}'
                pod_hooks += 1
                pw["namespaceSelector"] = ow["namespaceSelector"]
    if k == ("Deployment", "kueue-controller-manager"):
        pc = p["spec"]["template"]["spec"]["containers"][0]
        oc = o["spec"]["template"]["spec"]["containers"][0]
        assert pc["image"] == pinned_image, f'manager image {pc["image"]} is not the reviewed pin'
        assert pinned_image.startswith(oc["image"] + "@sha256:"), "pin must be the upstream reference plus a digest"
        pinned_images += 1
        pc["image"] = oc["image"]
    assert p == o, f"patched render changed more than the pod selectors and manager image in {k}"
assert pod_hooks == 2, f"expected mpod+vpod webhooks, saw {pod_hooks}"
assert pinned_images == 1, "expected exactly one digest-pinned manager image"
config = next(d for d in patched if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "kueue-manager-config")
sealed = open(sealed_path).read()
deployed = config["data"]["controller_manager_config.yaml"]
assert yaml.safe_load(deployed) == yaml.safe_load(sealed), "rendered manager config is not semantically the sealed configuration"
open(deployed_path, "w").write(deployed)
baseline = yaml.safe_load(open(baseline_path))
expected = json.loads(json.dumps(baseline))
expected["integrations"]["frameworks"].append("pod")
expected["integrations"]["podOptions"] = {"namespaceSelector": selector}
assert yaml.safe_load(sealed) == expected, "sealed config is not the reviewed live baseline plus the pod integration"
print("render and baseline equivalence proven")
PY
[ "$(sha256sum "$tmp/deployed-config.yaml" | awk '{print $1}')" = "$LIVE_KUEUE_DEPLOYED_CONFIG_SHA256" ] || { printf 'rendered deployed config digest does not match the versions.env pin\n' >&2; exit 1; }

# Fake cluster: kubectl and helm state machines over $FAKE_STATE.
FAKE_STATE=$tmp/state
mkdir -m 0700 "$tmp/bin" "$FAKE_STATE"
cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
printf 'kubectl %s\n' "$*" >>"$FAKE_STATE/calls.log"
case "$*" in
  'config current-context') printf 'disposable-kueue-test' ;;
  'get namespace kube-system -o jsonpath={.metadata.uid}') printf '99999999-9999-4999-8999-999999999999' ;;
  'get namespace blazn-poc --ignore-not-found -o name')
    [ "${FAKE_NS_ERROR:-0}" = 0 ] || { printf 'fake API discovery error\n' >&2; exit 1; }
    [ ! -e "$FAKE_STATE/ns-blazn-poc" ] || printf 'namespace/blazn-poc\n' ;;
  'get namespace blazn-poc-sandboxes --ignore-not-found -o name')
    [ "${FAKE_NS_ERROR:-0}" = 0 ] || { printf 'fake API discovery error\n' >&2; exit 1; }
    [ ! -e "$FAKE_STATE/ns-blazn-poc-sandboxes" ] || printf 'namespace/blazn-poc-sandboxes\n' ;;
  'get workloads.kueue.x-k8s.io -A -o json') cat "$FAKE_STATE/workloads.json" ;;
  '-n kueue-system get configmap kueue-manager-config -o jsonpath='*) cat "$FAKE_STATE/config" ;;
  'wait deployment/kueue-controller-manager -n kueue-system '*) : ;;
  '-n kueue-system get deployment kueue-controller-manager -o jsonpath='*) cat "$FAKE_STATE/deployment-image" ;;
  'get mutatingwebhookconfiguration kueue-mutating-webhook-configuration -o json')
    jq -n --slurpfile s "$FAKE_STATE/webhooks.json" '{webhooks:[{name:"mpod.kb.io",failurePolicy:"Fail",namespaceSelector:$s[0]}]}' ;;
  'get validatingwebhookconfiguration kueue-validating-webhook-configuration -o json')
    jq -n --slurpfile s "$FAKE_STATE/webhooks.json" '{webhooks:[{name:"vpod.kb.io",failurePolicy:"Fail",namespaceSelector:$s[0]}]}' ;;
  *) printf 'unexpected kubectl invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
cat >"$tmp/bin/helm" <<'EOF'
#!/bin/sh
set -eu
printf 'helm %s\n' "$*" >>"$FAKE_STATE/calls.log"
if [ "$1" = package ]; then shift; exec "$REAL_HELM" package "$@"; fi
next_revision() { jq '[.[].revision] | max + 1' "$FAKE_STATE/revisions.json"; }
supersede() {
  jq '[.[] | if .status=="deployed" then .status="superseded" else . end]' "$FAKE_STATE/revisions.json" >"$FAKE_STATE/revisions.json.tmp"
  mv "$FAKE_STATE/revisions.json.tmp" "$FAKE_STATE/revisions.json"
}
append_revision() {
  jq --argjson revision "$1" --arg status "$2" --arg description "$3" '. + [{revision:$revision,status:$status,description:$description}]' "$FAKE_STATE/revisions.json" >"$FAKE_STATE/revisions.json.tmp"
  mv "$FAKE_STATE/revisions.json.tmp" "$FAKE_STATE/revisions.json"
}
case "$*" in
  '-n kueue-system list -f ^kueue$ -o json')
    jq '[.[] | select(.status=="deployed")] | [.[-1] + {chart:"kueue-0.14.3",app_version:"v0.14.3"}]' "$FAKE_STATE/revisions.json" ;;
  '-n kueue-system history kueue -o json') cat "$FAKE_STATE/revisions.json" ;;
  '-n kueue-system status kueue -o json')
    jq '{info:{description:([.[] | select(.status=="deployed")][-1].description)}}' "$FAKE_STATE/revisions.json" ;;
  '-n kueue-system get manifest kueue') cat "$FAKE_STATE/manifest" ;;
  '-n kueue-system rollback kueue '*)
    target=$5
    case "${FAKE_HELM_ROLLBACK_MODE:-success}" in
      fail) printf 'fake helm rollback failure\n' >&2; exit 1 ;;
      kill)
        append_revision "$(next_revision)" pending-rollback "Rollback to $target"
        exit 137 ;;
      success) : ;;
      *) printf 'unknown fake rollback mode\n' >&2; exit 2 ;;
    esac
    revision=$(next_revision)
    supersede
    append_revision "$revision" deployed "Rollback to $target"
    cp "$FAKE_STATE/baseline-manifest" "$FAKE_STATE/manifest"
    cp "$FAKE_STATE/baseline-config" "$FAKE_STATE/config"
    cp "$FAKE_STATE/baseline-webhooks.json" "$FAKE_STATE/webhooks.json"
    [ "${FAKE_ROLLBACK_IMAGE_DRIFT:-0}" = 1 ] || cp "$FAKE_STATE/baseline-deployment-image" "$FAKE_STATE/deployment-image" ;;
  'upgrade kueue '*)
    description=''
    config_file=''
    previous=''
    for argument in "$@"; do
      case "$previous" in
        --description) description=$argument ;;
        --set-file) config_file=${argument#managerConfig.controllerManagerConfigYaml=} ;;
      esac
      previous=$argument
    done
    [ -n "$description" ] && [ -n "$config_file" ] || { printf 'fake helm upgrade missing arguments\n' >&2; exit 2; }
    revision=$(next_revision)
    case "${FAKE_HELM_UPGRADE_MODE:-success}" in
      kill)
        append_revision "$revision" pending-upgrade "$description"
        exit 137 ;;
      atomic-fail)
        append_revision "$revision" failed "$description"
        append_revision "$((revision + 1))" deployed "Rollback to $((revision - 1))"
        printf 'fake atomic upgrade failed and rolled back\n' >&2
        exit 1 ;;
      applied-kill|success)
        [ -f "$config_file" ] || { printf 'fake helm upgrade config file missing\n' >&2; exit 2; }
        supersede
        append_revision "$revision" deployed "$description"
        cat "$FAKE_STATE/deployed-config" >"$FAKE_STATE/config"
        cat "$FAKE_STATE/pinned-image" >"$FAKE_STATE/deployment-image"
        printf 'manifest-for-blazn-pod-integration revision %s\n' "$revision" >"$FAKE_STATE/manifest"
        if [ "${FAKE_WEBHOOK_DRIFT:-0}" = 1 ]; then
          printf '{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["wrong-namespace"]}]}\n' >"$FAKE_STATE/webhooks.json"
        else
          printf '{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["blazn-poc","blazn-poc-sandboxes"]}]}\n' >"$FAKE_STATE/webhooks.json"
        fi
        if [ "${FAKE_WORKLOAD_DRIFT_AFTER_UPGRADE:-0}" = 1 ]; then
          jq '.items += [{"metadata":{"uid":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","namespace":"frontro-agent-runtime","name":"job-drift"}}]' "$FAKE_STATE/workloads.json" >"$FAKE_STATE/workloads.json.tmp"
          mv "$FAKE_STATE/workloads.json.tmp" "$FAKE_STATE/workloads.json"
        fi
        [ "${FAKE_HELM_UPGRADE_MODE:-success}" != applied-kill ] || exit 137 ;;
      *) printf 'unknown fake upgrade mode\n' >&2; exit 2 ;;
    esac ;;
  *) printf 'unexpected helm invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod 0700 "$tmp/bin/kubectl" "$tmp/bin/helm"

reset_state() {
  find "$FAKE_STATE" -mindepth 1 -xdev -delete
  printf '[{"revision":44,"status":"superseded","description":"Upgrade complete"},{"revision":45,"status":"deployed","description":"Upgrade complete"}]\n' >"$FAKE_STATE/revisions.json"
  printf 'live-kueue-manifest-revision-45\n' >"$FAKE_STATE/manifest"
  cp "$ROOT/phase4c/kueue-live-config-baseline.yaml" "$FAKE_STATE/config"
  printf '{"items":[{"metadata":{"uid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","namespace":"frontro-agent-runtime","name":"job-one"}},{"metadata":{"uid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","namespace":"frontro-agent-runtime","name":"job-two"}}]}\n' >"$FAKE_STATE/workloads.json"
  printf '{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"NotIn","values":["kube-system","kueue-system"]}]}\n' >"$FAKE_STATE/webhooks.json"
  cp "$FAKE_STATE/manifest" "$FAKE_STATE/baseline-manifest"
  cp "$FAKE_STATE/config" "$FAKE_STATE/baseline-config"
  cp "$FAKE_STATE/webhooks.json" "$FAKE_STATE/baseline-webhooks.json"
  cp "$FAKE_STATE/workloads.json" "$FAKE_STATE/baseline-workloads.json"
  cp "$tmp/deployed-config.yaml" "$FAKE_STATE/deployed-config"
  printf '%s\n' "$LIVE_KUEUE_CONTROLLER_IMAGE" >"$FAKE_STATE/pinned-image"
  printf 'registry.k8s.io/kueue/kueue@sha256:2c5b782a2a3954ef72576db22d6bdc752d3604d39f3be734662ab7acfa2f61dc\n' >"$FAKE_STATE/deployment-image"
  cp "$FAKE_STATE/deployment-image" "$FAKE_STATE/baseline-deployment-image"
  : >"$FAKE_STATE/calls.log"
}
baseline_manifest_sha=$(printf 'live-kueue-manifest-revision-45\n' | sha256sum | awk '{print $1}')

transaction_counter=0
new_transaction() {
  transaction_counter=$((transaction_counter + 1))
  transaction=$tx_root/$tx_prefix-$transaction_counter
}
run_upgrade() {
  run_transaction=$1
  shift
  set +e
  "$ROOT/phase4c/with-live-lock.sh" env PATH="$tmp/bin:$PATH" \
    FAKE_STATE="$FAKE_STATE" REAL_HELM="$REAL_HELM" \
    BLAZN_EXPECTED_CONTEXT=disposable-kueue-test \
    BLAZN_EXPECTED_KUBE_SYSTEM_UID=99999999-9999-4999-8999-999999999999 \
    BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
    BLAZN_KUEUE_TRANSACTION_DIR="$run_transaction" \
    BLAZN_EXPECTED_KUEUE_REVISION=45 \
    BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256="$baseline_manifest_sha" \
    BLAZN_EXPECTED_KUEUE_CONFIG_SHA256="$LIVE_KUEUE_PRIOR_CONFIG_SHA256" \
    BLAZN_EXPECTED_WORKLOADS=2 \
    "$@" \
    "$ROOT/phase4c/upgrade-kueue-pod-integration.sh" "$chart_tgz" >"$tmp/last-out" 2>"$tmp/last-err"
  last_code=$?
  set -e
}
expect_phase() { [ "$(cat "$1/phase")" = "$2" ] || { printf 'expected phase %s, got %s\n' "$2" "$(cat "$1/phase")" >&2; exit 1; }; }
expect_message() { grep -Fq -- "$1" "$tmp/last-err" || { printf 'missing expected message: %s\n' "$1" >&2; cat "$tmp/last-err" >&2; exit 1; }; }

# S1: happy path, then idempotent re-run.
reset_state
new_transaction
run_upgrade "$transaction"
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase "$transaction" complete
cmp -s "$FAKE_STATE/config" "$FAKE_STATE/deployed-config" || { printf 'deployed config is not the reviewed rendered bytes\n' >&2; exit 1; }
jq -e '.matchExpressions[0].values == ["blazn-poc","blazn-poc-sandboxes"]' "$FAKE_STATE/webhooks.json" >/dev/null
jq -e '([.[] | select(.status=="deployed")][-1]) | .revision == 46' "$FAKE_STATE/revisions.json" >/dev/null
cmp -s "$FAKE_STATE/workloads.json" "$FAKE_STATE/baseline-workloads.json" || { printf 'happy path must not change Workload identities\n' >&2; exit 1; }
run_upgrade "$transaction"
[ "$last_code" -eq 0 ]
grep -Fq 'already complete' "$tmp/last-out"

# S2: crash at each pre-mutation journal boundary, then resume to completion.
for boundary in sealed prepared upgrade-intent; do
  reset_state
  new_transaction
  run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER="$boundary" BLAZN_PHASE4C_DISPOSABLE_TEST=true
  [ "$last_code" -eq 86 ] || { printf 'boundary %s: expected 86, got %s\n' "$boundary" "$last_code" >&2; exit 1; }
  expect_phase "$transaction" "$boundary"
  run_upgrade "$transaction"
  [ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
  expect_phase "$transaction" complete
done

# S2b: crash right after the upgraded journal entry triggers a verified
# automatic rollback, and the rolled-back transaction refuses reuse.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=upgraded BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase "$transaction" rollback-complete
cmp -s "$FAKE_STATE/config" "$FAKE_STATE/baseline-config"
cmp -s "$FAKE_STATE/deployment-image" "$FAKE_STATE/baseline-deployment-image" || { printf 'rollback must restore the controller image\n' >&2; exit 1; }
grep -Fq 'helm -n kueue-system rollback kueue 45' "$FAKE_STATE/calls.log"
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'was rolled back; use a new transaction'

# S2c: crash after the complete journal entry keeps the release upgraded.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=complete BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase "$transaction" complete
cmp -s "$FAKE_STATE/config" "$FAKE_STATE/deployed-config"
if grep -Fq 'rollback kueue' "$FAKE_STATE/calls.log"; then printf 'completed transaction must never roll back\n' >&2; exit 1; fi

# S3: helm process killed mid-upgrade leaves an owned pending revision that a
# resume reconciles into a verified rollback.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_HELM_UPGRADE_MODE=kill
[ "$last_code" -eq 137 ]
expect_phase "$transaction" upgrade-intent
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'owned pending Kueue upgrade rolled back'
expect_phase "$transaction" rollback-complete
cmp -s "$FAKE_STATE/config" "$FAKE_STATE/baseline-config"
cmp -s "$FAKE_STATE/deployment-image" "$FAKE_STATE/baseline-deployment-image"

# S4: crash after Helm applied but before the upgraded journal entry; resume
# adopts the owned deployed revision and completes.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_HELM_UPGRADE_MODE=applied-kill
[ "$last_code" -eq 137 ]
expect_phase "$transaction" upgrade-intent
run_upgrade "$transaction"
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase "$transaction" complete

# S5: Helm atomic rollback is recognized, verified, and journaled.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_HELM_UPGRADE_MODE=atomic-fail
[ "$last_code" -eq 1 ]
expect_phase "$transaction" upgrade-intent
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'Kueue atomic rollback reconciled'
expect_phase "$transaction" rollback-complete

# S6: an unowned pending revision blocks without any mutation.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=upgrade-intent BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
jq '. + [{"revision":46,"status":"pending-upgrade","description":"someone-else"}]' "$FAKE_STATE/revisions.json" >"$FAKE_STATE/revisions.json.tmp"
mv "$FAKE_STATE/revisions.json.tmp" "$FAKE_STATE/revisions.json"
: >"$FAKE_STATE/calls.log"
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'an unowned Kueue upgrade is pending'
expect_phase "$transaction" upgrade-intent
if grep -Eq 'helm (upgrade|-n kueue-system rollback)' "$FAKE_STATE/calls.log"; then printf 'unowned pending state must not be mutated\n' >&2; exit 1; fi

# S7: an unrelated newer deployed revision blocks without any mutation.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=upgrade-intent BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
jq '[.[] | if .status=="deployed" then .status="superseded" else . end] + [{"revision":47,"status":"deployed","description":"unrelated admin change"}]' "$FAKE_STATE/revisions.json" >"$FAKE_STATE/revisions.json.tmp"
mv "$FAKE_STATE/revisions.json.tmp" "$FAKE_STATE/revisions.json"
: >"$FAKE_STATE/calls.log"
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'cannot be reconciled with the transaction'
expect_phase "$transaction" upgrade-intent
if grep -Eq 'helm (upgrade|-n kueue-system rollback)' "$FAKE_STATE/calls.log"; then printf 'unowned revision state must not be mutated\n' >&2; exit 1; fi

# S8: namespace discovery API errors fail closed before any mutation.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_NS_ERROR=1
[ "$last_code" -eq 1 ]
expect_message 'could not be verified'
expect_phase "$transaction" sealed
if grep -Fq 'helm upgrade' "$FAKE_STATE/calls.log"; then printf 'discovery failure must block mutation\n' >&2; exit 1; fi

# S9: a namespace appearing between journal boundaries blocks the resumed
# mutation.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=upgrade-intent BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
touch "$FAKE_STATE/ns-blazn-poc"
: >"$FAKE_STATE/calls.log"
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'appeared or could not be verified before Kueue mutation'
if grep -Fq 'helm upgrade' "$FAKE_STATE/calls.log"; then printf 'namespace appearance must block mutation\n' >&2; exit 1; fi

# S10: a Workload created during enablement rolls back, and rollback is never
# declared complete while the identity set still differs.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_WORKLOAD_DRIFT_AFTER_UPGRADE=1
[ "$last_code" -ne 0 ]
expect_message 'automatic Kueue rollback or verification failed'
expect_phase "$transaction" upgraded

# S11: a tampered derived chart package is rejected on resume.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=prepared BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
chmod 0600 "$transaction/kueue-0.14.3.tgz"
printf 'tamper' >>"$transaction/kueue-0.14.3.tgz"
chmod 0400 "$transaction/kueue-0.14.3.tgz"
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'derived Kueue chart changed since preparation'

# S12: path traversal outside the reviewed transaction root is rejected.
reset_state
run_upgrade "$tx_root/kueue-pod-x/../$tx_prefix-evil"
[ "$last_code" -eq 1 ]
expect_message 'one clean segment'

# S13: a wrong deployed webhook selector rolls back with verification.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_WEBHOOK_DRIFT=1
[ "$last_code" -ne 0 ]
expect_phase "$transaction" rollback-complete
cmp -s "$FAKE_STATE/config" "$FAKE_STATE/baseline-config"
cmp -s "$FAKE_STATE/deployment-image" "$FAKE_STATE/baseline-deployment-image"

# S13b: a rollback that fails to restore the controller image is never
# journaled rollback-complete.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_WEBHOOK_DRIFT=1 FAKE_ROLLBACK_IMAGE_DRIFT=1
[ "$last_code" -ne 0 ]
expect_message 'automatic Kueue rollback or verification failed'
expect_phase "$transaction" upgraded

# S14: a failing automatic rollback never journals rollback-complete.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_WEBHOOK_DRIFT=1 FAKE_HELM_ROLLBACK_MODE=fail
[ "$last_code" -ne 0 ]
expect_message 'automatic Kueue rollback or verification failed'
expect_phase "$transaction" upgraded

# S15: a rollback killed mid-flight leaves pending-rollback, and a resume
# reconciles the owned pending rollback into a verified rollback-complete.
reset_state
new_transaction
run_upgrade "$transaction" FAKE_WEBHOOK_DRIFT=1 FAKE_HELM_ROLLBACK_MODE=kill
[ "$last_code" -ne 0 ]
expect_phase "$transaction" upgraded
jq -e '[.[] | select(.status=="pending-rollback")] | length == 1' "$FAKE_STATE/revisions.json" >/dev/null
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'interrupted Kueue rollback reconciled'
expect_phase "$transaction" rollback-complete
cmp -s "$FAKE_STATE/config" "$FAKE_STATE/baseline-config"
cmp -s "$FAKE_STATE/deployment-image" "$FAKE_STATE/baseline-deployment-image"

# S16: an unowned pending rollback blocks without any mutation.
reset_state
new_transaction
run_upgrade "$transaction" BLAZN_PHASE4C_FAIL_AFTER=upgrade-intent BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
jq '. + [{"revision":46,"status":"pending-rollback","description":"Rollback to 12"}]' "$FAKE_STATE/revisions.json" >"$FAKE_STATE/revisions.json.tmp"
mv "$FAKE_STATE/revisions.json.tmp" "$FAKE_STATE/revisions.json"
: >"$FAKE_STATE/calls.log"
run_upgrade "$transaction"
[ "$last_code" -eq 1 ]
expect_message 'an unowned Kueue rollback is pending'
expect_phase "$transaction" upgrade-intent
if grep -Eq 'helm (upgrade|-n kueue-system rollback)' "$FAKE_STATE/calls.log"; then printf 'unowned pending rollback must not be mutated\n' >&2; exit 1; fi

printf 'Phase 4C Kueue Pod integration transaction proofs passed\n'
