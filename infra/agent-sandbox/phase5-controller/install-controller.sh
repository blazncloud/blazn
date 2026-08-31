#!/bin/sh
set -eu

# Crash-resumable, UID-fenced deployment of the Phase 5 sandbox controller.
# Consumes a sealed transaction directory built from render-install.sh output.
# The controller Secrets must already be provisioned; this applies the sealed
# controller bundle and scales it from zero to one.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the controller deployment must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s RENDERED_CONTROLLER_MANIFEST\n' "$0" >&2; exit 64; }
manifest=$1
phase4c_require_mutation_authority
: "${BLAZN_CONTROLLER_TRANSACTION_DIR:?set a durable transaction directory}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the transaction UUID}"
: "${BLAZN_EXPECTED_CONTROLLER_SHA256:?set the reviewed rendered controller digest}"
: "${BLAZN_DATABASE_URL_SECRET_NAME:?set the database URL Secret name}"
: "${BLAZN_OBJECT_SECRET_NAME:?set the object credential Secret name}"
: "${BLAZN_REGISTRY_PULL_SECRET_NAME:?set the registry pull Secret name}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
transaction=$BLAZN_CONTROLLER_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase5/controller-*) ;; *) printf 'controller transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase5/}
case "$transaction_name" in */*|*..*|'') printf 'controller transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac

write_phase() { phase4c_write_phase "$transaction" "$1"; }
object_present() { discovered=$(kubectl get "$1" "$2" -n "$3" --ignore-not-found -o name) || return 2; [ -n "$discovered" ]; }
object_absent() {
  if [ "$3" = - ]; then discovered=$(kubectl get "$1" "$2" --ignore-not-found -o name) || return 2
  else discovered=$(kubectl get "$1" "$2" -n "$3" --ignore-not-found -o name) || return 2
  fi
  [ -z "$discovered" ]
}
live_uid() {
  if [ "$3" = - ]; then kubectl get "$1" "$2" -o jsonpath='{.metadata.uid}'
  else kubectl get "$1" "$2" -n "$3" -o jsonpath='{.metadata.uid}'
  fi
}
verified_anchor_uid() {
  kubectl get clusterrole "$anchor_name" -o json | jq -er --arg uid "$1" '
    select(.metadata.uid == $uid) | select(.rules == []) |
    select((.aggregationRule // null) == null) | select((.metadata.ownerReferences // []) == []) |
    select((.metadata.finalizers // []) == []) | .metadata.uid'
}
validate_uid_journal() {
  [ -f "$uids" ] && [ ! -L "$uids" ] && [ "$(stat -c '%u:%a:%h' "$uids")" = 0:600:1 ] || { printf 'owned UID journal metadata is unsafe\n' >&2; return 1; }
  jq -e '
    ["serviceaccount/blazn-sandbox-controller","role/blazn-sandbox-controller","clusterrole/blazn-sandbox-controller-node-observer","deployment/blazn-sandbox-controller","service/blazn-sandbox-access","networkpolicy/blazn-sandbox-controller-default-deny","networkpolicy/blazn-sandbox-controller-access-ingress","networkpolicy/blazn-sandbox-controller-egress","rolebinding/blazn-sandbox-controller","clusterrolebinding/blazn-sandbox-controller-node-observer"] as $allowed |
    (to_entries) as $entries | ($entries | length) <= ($allowed | length) and
    all(range(0; ($entries | length)); $entries[.].key == $allowed[.]) and
    all($entries[]; .value | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$uids" >/dev/null || { printf 'owned UID journal schema is invalid\n' >&2; return 1; }
}
validate_anchor_record() {
  [ -f "$anchor_record" ] && [ ! -L "$anchor_record" ] && [ "$(stat -c '%u:%a:%h' "$anchor_record")" = 0:600:1 ] || { printf 'anchor journal metadata is unsafe\n' >&2; return 1; }
  jq -e --arg name "$anchor_name" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" '
    .apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "ClusterRole" and .metadata.name == $name and
    .metadata.annotations == {"blazn.dev/phase5-transaction":$tx} and ((.metadata.labels // {}) == {}) and
    ((.metadata.ownerReferences // []) == []) and ((.metadata.finalizers // []) == []) and
    ((.aggregationRule // null) == null) and .rules == [] and
    (.metadata.uid | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$anchor_record" >/dev/null || { printf 'anchor journal schema is invalid\n' >&2; return 1; }
}

if [ ! -e "$transaction" ]; then
  if ! { [ -f "$manifest" ] && [ ! -L "$manifest" ] && [ "$(stat -c '%h' "$manifest")" = 1 ]; }; then printf 'rendered controller manifest is unsafe\n' >&2; exit 1; fi
  install -d -o root -g root -m 0700 /var/lib/blazn/phase5 2>/dev/null || :
  install -d -o root -g root -m 0700 "$transaction"
  install -o root -g root -m 0400 "$manifest" "$transaction/controller.yaml"
  write_phase sealed
fi
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'controller transaction directory is unsafe\n' >&2; exit 1; fi
sealed=$transaction/controller.yaml
if [ -L "$sealed" ] || [ ! -f "$sealed" ] || [ "$(stat -c '%u:%a:%h' "$sealed")" != 0:400:1 ]; then printf 'sealed controller manifest is unsafe\n' >&2; exit 1; fi
[ "$(sha256sum "$sealed" | awk '{print $1}')" = "$BLAZN_EXPECTED_CONTROLLER_SHA256" ] || { printf 'sealed controller manifest digest mismatch\n' >&2; exit 1; }
grep -Fq 'replicas: 0' "$sealed" || { printf 'the reviewed controller manifest must start scaled to zero\n' >&2; exit 1; }
[ "$(grep -Fxc "    blazn.dev/phase5-transaction: $BLAZN_PHASE5_TRANSACTION_ID" "$sealed")" -eq 10 ] || { printf 'sealed controller manifest transaction identity mismatch\n' >&2; exit 1; }
[ "$(grep -Fxc '    uid: BLAZN_PHASE5_ANCHOR_UID' "$sealed")" -eq 10 ] || { printf 'sealed controller anchor placeholders are invalid\n' >&2; exit 1; }
[ "$(grep -Fxc "    name: blazn-phase5-anchor-$BLAZN_PHASE5_TRANSACTION_ID" "$sealed")" -eq 10 ] || { printf 'sealed controller anchor references are invalid\n' >&2; exit 1; }
if [ "$(grep -Fxc '    controller: false' "$sealed")" -ne 10 ] || [ "$(grep -Fxc '    blockOwnerDeletion: false' "$sealed")" -ne 10 ]; then
  printf 'sealed controller owner references are invalid\n' >&2; exit 1
fi
for object_key in serviceaccount role rolebinding clusterrole clusterrolebinding deployment service deny access-ingress egress; do
  [ "$(grep -Fxc "    blazn.dev/phase5-object: $object_key" "$sealed")" -eq 1 ] || { printf 'sealed controller object selectors are invalid\n' >&2; exit 1; }
done
phase=$(cat "$transaction/phase")
case "$phase" in
  complete) printf 'controller deployment transaction is already complete\n'; exit 0 ;;
  rollback-complete) printf 'controller deployment transaction was rolled back; use a new transaction\n' >&2; exit 1 ;;
  sealed|anchor-intent|anchor-journaled|apply-intent|applied|scaled) ;;
  *) printf 'controller transaction phase is invalid\n' >&2; exit 1 ;;
esac

kubectl get namespace blazn-poc-system >/dev/null
kubectl get deployment agent-sandbox-controller -n agent-sandbox-system >/dev/null 2>&1 || { printf 'the Agent Sandbox controller is not installed\n' >&2; exit 1; }
if ! object_present secret "$BLAZN_DATABASE_URL_SECRET_NAME" blazn-poc-system; then printf 'the controller database Secret is not provisioned\n' >&2; exit 1; fi
if ! object_present secret "$BLAZN_OBJECT_SECRET_NAME" blazn-poc-system; then printf 'the controller object Secret is not provisioned\n' >&2; exit 1; fi
if ! object_present secret "$BLAZN_REGISTRY_PULL_SECRET_NAME" blazn-poc-system; then printf 'the controller registry pull Secret is not provisioned\n' >&2; exit 1; fi
if ! object_present secret "$BLAZN_REGISTRY_PULL_SECRET_NAME" blazn-poc-sandboxes; then printf 'the Sandbox registry pull Secret is not provisioned\n' >&2; exit 1; fi

anchor_name=blazn-phase5-anchor-$BLAZN_PHASE5_TRANSACTION_ID
anchor_record=$transaction/anchor.json
controller_specs='serviceaccount|v1|ServiceAccount|blazn-sandbox-controller|blazn-poc-system|serviceaccount/blazn-sandbox-controller
role|rbac.authorization.k8s.io/v1|Role|blazn-sandbox-controller|blazn-poc-sandboxes|role/blazn-sandbox-controller
clusterrole|rbac.authorization.k8s.io/v1|ClusterRole|blazn-sandbox-controller-node-observer|-|clusterrole/blazn-sandbox-controller-node-observer
deployment|apps/v1|Deployment|blazn-sandbox-controller|blazn-poc-system|deployment/blazn-sandbox-controller
service|v1|Service|blazn-sandbox-access|blazn-poc-system|service/blazn-sandbox-access
deny|networking.k8s.io/v1|NetworkPolicy|blazn-sandbox-controller-default-deny|blazn-poc-system|networkpolicy/blazn-sandbox-controller-default-deny
access-ingress|networking.k8s.io/v1|NetworkPolicy|blazn-sandbox-controller-access-ingress|blazn-poc-system|networkpolicy/blazn-sandbox-controller-access-ingress
egress|networking.k8s.io/v1|NetworkPolicy|blazn-sandbox-controller-egress|blazn-poc-system|networkpolicy/blazn-sandbox-controller-egress
rolebinding|rbac.authorization.k8s.io/v1|RoleBinding|blazn-sandbox-controller|blazn-poc-sandboxes|rolebinding/blazn-sandbox-controller
clusterrolebinding|rbac.authorization.k8s.io/v1|ClusterRoleBinding|blazn-sandbox-controller-node-observer|-|clusterrolebinding/blazn-sandbox-controller-node-observer'
canonicalize_object() {
  jq -S 'del(.metadata.uid, .metadata.resourceVersion, .metadata.generation, .metadata.creationTimestamp, .metadata.managedFields, .metadata.selfLink, .status)'
}
validate_semantics() {
  semantic_key=$1; semantic_actual=$2
  semantic_expected=$transaction/.semantic-expected.json
  semantic_expected_canonical=$transaction/.semantic-expected-canonical.json
  semantic_actual_canonical=$transaction/.semantic-actual-canonical.json
  kubectl apply --server-side --dry-run=server --field-manager blazn-phase5-controller -f "$anchored" -l "blazn.dev/phase5-object=$semantic_key" -o json >"$semantic_expected"
  canonicalize_object <"$semantic_expected" >"$semantic_expected_canonical"
  canonicalize_object <"$semantic_actual" >"$semantic_actual_canonical"
  cmp -s "$semantic_expected_canonical" "$semantic_actual_canonical" || { printf 'controller object semantics differ from sealed manifest: %s\n' "$semantic_key" >&2; return 1; }
  rm -f "$semantic_expected" "$semantic_expected_canonical" "$semantic_actual_canonical"
}
if [ "$phase" = sealed ]; then
  for object in deployment/blazn-sandbox-controller:blazn-poc-system service/blazn-sandbox-access:blazn-poc-system serviceaccount/blazn-sandbox-controller:blazn-poc-system role/blazn-sandbox-controller:blazn-poc-sandboxes rolebinding/blazn-sandbox-controller:blazn-poc-sandboxes clusterrole/blazn-sandbox-controller-node-observer:- clusterrolebinding/blazn-sandbox-controller-node-observer:- networkpolicy/blazn-sandbox-controller-access-ingress:blazn-poc-system networkpolicy/blazn-sandbox-controller-egress:blazn-poc-system networkpolicy/blazn-sandbox-controller-default-deny:blazn-poc-system; do
    ref=${object%%:*}; ns=${object#*:}; kind=${ref%%/*}; name=${ref#*/}
    object_absent "$kind" "$name" "$ns" || { printf 'controller object already exists before transaction: %s\n' "$ref" >&2; exit 1; }
  done
  object_absent clusterrole "$anchor_name" - || { printf 'transaction anchor already exists before transaction\n' >&2; exit 1; }
  write_phase anchor-intent; phase=anchor-intent
fi
if [ "$phase" = anchor-intent ]; then
  if object_absent clusterrole "$anchor_name" -; then
    anchor_request=$transaction/.anchor-request.json
    printf '{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"%s","annotations":{"blazn.dev/phase5-transaction":"%s"}},"rules":[]}\n' "$anchor_name" "$BLAZN_PHASE5_TRANSACTION_ID" >"$anchor_request"
    kubectl create -f "$anchor_request" -o json >"$anchor_record.tmp"
    rm -f "$anchor_request"
    if [ "${BLAZN_PHASE4C_DISPOSABLE_TEST:-}" = true ] && [ "${BLAZN_PHASE4C_FAIL_AFTER:-}" = anchor-created ]; then exit 86; fi
  else
    kubectl get clusterrole "$anchor_name" -o json >"$anchor_record.tmp"
  fi
  jq -e --arg name "$anchor_name" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" '
    .apiVersion == "rbac.authorization.k8s.io/v1" and .kind == "ClusterRole" and
    .metadata.name == $name and .metadata.annotations == {"blazn.dev/phase5-transaction":$tx} and
    ((.metadata.labels // {}) == {}) and ((.metadata.ownerReferences // []) == []) and
    ((.metadata.finalizers // []) == []) and ((.aggregationRule // null) == null) and
    .rules == [] and (.metadata.uid | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))' "$anchor_record.tmp" >/dev/null || { printf 'transaction anchor response is invalid\n' >&2; exit 1; }
  chmod 0600 "$anchor_record.tmp"; sync -f "$anchor_record.tmp"; mv "$anchor_record.tmp" "$anchor_record"; sync -f "$transaction"
  write_phase anchor-journaled; phase=anchor-journaled
fi
if [ "$phase" = anchor-journaled ]; then
  validate_anchor_record
  anchor_uid=$(jq -er '.metadata.uid' "$anchor_record")
  [ "$anchor_uid" = "$(verified_anchor_uid "$anchor_uid")" ] || { printf 'transaction anchor identity or inert rules changed; recovery is required\n' >&2; exit 1; }
  anchored=$transaction/controller-anchored.yaml
  sed "s/BLAZN_PHASE5_ANCHOR_UID/$anchor_uid/g" "$sealed" >"$anchored.tmp"
  [ "$(grep -Fxc "    uid: $anchor_uid" "$anchored.tmp")" -eq 10 ] || { printf 'anchored controller manifest is incomplete\n' >&2; exit 1; }
  ! grep -Fq BLAZN_PHASE5_ANCHOR_UID "$anchored.tmp" || { printf 'anchored controller manifest retains a placeholder\n' >&2; exit 1; }
  mv "$anchored.tmp" "$anchored"; chmod 0400 "$anchored"
  sync -f "$anchored"; sync -f "$transaction"
  write_phase apply-intent; phase=apply-intent
fi
uids=$transaction/owned-uids.json
if [ "$phase" = apply-intent ]; then
  validate_anchor_record
  anchor_uid=$(jq -er '.metadata.uid' "$anchor_record")
  anchored=$transaction/controller-anchored.yaml
  [ "$anchor_uid" = "$(verified_anchor_uid "$anchor_uid")" ] || { printf 'transaction anchor identity or inert rules changed; recovery is required\n' >&2; exit 1; }
  sed "s/BLAZN_PHASE5_ANCHOR_UID/$anchor_uid/g" "$sealed" >"$anchored.tmp"
  if [ "$(grep -Fxc "    uid: $anchor_uid" "$anchored.tmp")" -ne 10 ] || grep -Fq BLAZN_PHASE5_ANCHOR_UID "$anchored.tmp"; then
    printf 'rebuilt anchored controller manifest is invalid\n' >&2; exit 1
  fi
  chmod 0400 "$anchored.tmp"; sync -f "$anchored.tmp"; mv "$anchored.tmp" "$anchored"; sync -f "$transaction"
  if [ ! -f "$uids" ]; then printf '{}\n' >"$uids.tmp"; chmod 0600 "$uids.tmp"; sync -f "$uids.tmp"; mv "$uids.tmp" "$uids"; sync -f "$transaction"; fi
  validate_uid_journal
  # Apply one sealed document at a time. The UID is accepted only from that
  # apply response and is durably journaled before the next object is touched.
  for spec in $controller_specs; do
    IFS='|' read -r key api object_kind name ns ref <<EOF
$spec
EOF
    journaled_uid=$(jq -r --arg ref "$ref" '.[$ref] // empty' "$uids")
    if [ -n "$journaled_uid" ]; then
      response=$transaction/.apply-response.json
      if [ "$ns" = - ]; then kubectl get "${ref%%/*}" "$name" -o json >"$response"; else kubectl get "${ref%%/*}" "$name" -n "$ns" -o json >"$response"; fi
      validate_semantics "$key" "$response"
      [ "$journaled_uid" = "$(jq -er '.metadata.uid' "$response")" ] || { printf 'journaled controller object identity changed: %s\n' "$ref" >&2; exit 1; }
      rm -f "$response"
      continue
    fi
    object_absent "${ref%%/*}" "$name" "$ns" || { printf 'dependent controller object exists without a completed UID journal; recovery is required: %s\n' "$ref" >&2; exit 1; }
    response=$transaction/.apply-response.json
    kubectl apply --server-side --field-manager blazn-phase5-controller -f "$anchored" -l "blazn.dev/phase5-object=$key" -o json >"$response"
    if [ "${BLAZN_PHASE4C_DISPOSABLE_TEST:-}" = true ] && { [ "${BLAZN_PHASE4C_FAIL_AFTER:-}" = apply-executed ] || [ "${BLAZN_PHASE4C_FAIL_AFTER:-}" = "apply-executed-$key" ]; }; then exit 86; fi
    validate_semantics "$key" "$response"
    jq -e --arg api "$api" --arg kind "$object_kind" --arg name "$name" --arg ns "$ns" --arg key "$key" --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" --arg anchor "$anchor_name" --arg anchor_uid "$anchor_uid" '
      .apiVersion == $api and .kind == $kind and .metadata.name == $name and
      (if $ns == "-" then ((.metadata.namespace // "") == "") else .metadata.namespace == $ns end) and
      .metadata.labels["blazn.dev/phase5-object"] == $key and .metadata.annotations["blazn.dev/phase5-transaction"] == $tx and
      any(.metadata.ownerReferences[]?; .kind == "ClusterRole" and .name == $anchor and .uid == $anchor_uid) and
      (.metadata.uid | test("^[0-9a-f-]{36}$"))' "$response" >/dev/null || { printf 'controller apply response is invalid: %s\n' "$ref" >&2; exit 1; }
    applied_uid=$(jq -er '.metadata.uid' "$response")
    jq --arg ref "$ref" --arg uid "$applied_uid" '. + {($ref):$uid}' "$uids" >"$uids.tmp"
    chmod 0600 "$uids.tmp"; sync -f "$uids.tmp"; mv "$uids.tmp" "$uids"; sync -f "$transaction"
    validate_uid_journal
    rm -f "$response"
    if [ "${BLAZN_PHASE4C_DISPOSABLE_TEST:-}" = true ] && [ "${BLAZN_PHASE4C_FAIL_AFTER:-}" = "journal-$key" ]; then exit 86; fi
  done
  write_phase applied; phase=applied
fi
if [ "$phase" = applied ] || [ "$phase" = scaled ]; then
  validate_anchor_record
  anchor_uid=$(jq -er '.metadata.uid' "$anchor_record")
  [ "$anchor_uid" = "$(verified_anchor_uid "$anchor_uid")" ] || { printf 'transaction anchor identity or inert rules changed; recovery is required\n' >&2; exit 1; }
  validate_uid_journal
fi
if [ "$phase" = applied ]; then
  # Idempotent across a crash between the scale and its journal entry: the
  # scale target is 1, so re-running scale is a no-op, and the recorded UID
  # proves the Deployment is still the one this transaction applied.
  for spec in $controller_specs; do
    IFS='|' read -r key _api _object_kind name ns ref <<EOF
$spec
EOF
    response=$transaction/.pre-scale-response.json
    if [ "$ns" = - ]; then kubectl get "${ref%%/*}" "$name" -o json >"$response"; else kubectl get "${ref%%/*}" "$name" -n "$ns" -o json >"$response"; fi
    validate_semantics "$key" "$response"
    [ "$(jq -er --arg ref "$ref" '.[$ref]' "$uids")" = "$(jq -er '.metadata.uid' "$response")" ] || { printf 'controller object identity changed before scale: %s\n' "$ref" >&2; exit 1; }
    rm -f "$response"
  done
  kubectl scale deployment blazn-sandbox-controller -n blazn-poc-system --replicas=1 >/dev/null
  write_phase scaled; phase=scaled
fi
if [ "$phase" = scaled ]; then
  available_attempts=${BLAZN_CONTROLLER_AVAILABLE_ATTEMPTS:-60}
  case "$available_attempts" in ''|*[!0-9]*) available_attempts=60 ;; esac
  attempt=0
  until [ "$(kubectl get deployment blazn-sandbox-controller -n blazn-poc-system -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)" = True ]; do
    attempt=$((attempt + 1))
    [ "$attempt" -le "$available_attempts" ] || { printf 'the controller never became Available\n' >&2; kubectl get pods -n blazn-poc-system -o wide >&2 || :; exit 1; }
    sleep 3
  done
  write_phase complete
fi
printf 'Phase 5 sandbox controller deployed and Available\n'
