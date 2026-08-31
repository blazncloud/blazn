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
live_uid() {
  if [ "$3" = - ]; then kubectl get "$1" "$2" -o jsonpath='{.metadata.uid}'
  else kubectl get "$1" "$2" -n "$3" -o jsonpath='{.metadata.uid}'
  fi
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
phase=$(cat "$transaction/phase")
case "$phase" in
  complete) printf 'controller deployment transaction is already complete\n'; exit 0 ;;
  rollback-complete) printf 'controller deployment transaction was rolled back; use a new transaction\n' >&2; exit 1 ;;
  sealed|apply-intent|applied|scaled) ;;
  *) printf 'controller transaction phase is invalid\n' >&2; exit 1 ;;
esac

kubectl get namespace blazn-poc-system >/dev/null
kubectl get deployment agent-sandbox-controller -n agent-sandbox-system >/dev/null 2>&1 || { printf 'the Agent Sandbox controller is not installed\n' >&2; exit 1; }
if ! object_present secret "$BLAZN_DATABASE_URL_SECRET_NAME" blazn-poc-system; then printf 'the controller database Secret is not provisioned\n' >&2; exit 1; fi
if ! object_present secret "$BLAZN_OBJECT_SECRET_NAME" blazn-poc-system; then printf 'the controller object Secret is not provisioned\n' >&2; exit 1; fi
if ! object_present secret "$BLAZN_REGISTRY_PULL_SECRET_NAME" blazn-poc-system; then printf 'the controller registry pull Secret is not provisioned\n' >&2; exit 1; fi
if ! object_present secret "$BLAZN_REGISTRY_PULL_SECRET_NAME" blazn-poc-sandboxes; then printf 'the Sandbox registry pull Secret is not provisioned\n' >&2; exit 1; fi

if [ "$phase" = sealed ]; then
  deployment_state=$(kubectl get deployment blazn-sandbox-controller -n blazn-poc-system --ignore-not-found -o name) || { printf 'controller Deployment discovery failed\n' >&2; exit 1; }
  [ -z "$deployment_state" ] || { printf 'a controller Deployment already exists; use its own transaction or roll it back first\n' >&2; exit 1; }
  write_phase apply-intent; phase=apply-intent
fi
uids=$transaction/owned-uids.json
if [ "$phase" = apply-intent ]; then
  kubectl apply --server-side --field-manager blazn-phase5-controller -f "$sealed" >/dev/null
  # Record every owned identity immediately after apply so a crash before
  # completion still leaves a UID-fenced teardown path.
  {
    printf '{'
    printf '"deployment/blazn-sandbox-controller":"%s",' "$(live_uid deployment blazn-sandbox-controller blazn-poc-system)"
    printf '"role/blazn-sandbox-controller":"%s",' "$(live_uid role blazn-sandbox-controller blazn-poc-sandboxes)"
    printf '"rolebinding/blazn-sandbox-controller":"%s",' "$(live_uid rolebinding blazn-sandbox-controller blazn-poc-sandboxes)"
    printf '"clusterrole/blazn-sandbox-controller-node-observer":"%s",' "$(live_uid clusterrole blazn-sandbox-controller-node-observer -)"
    printf '"clusterrolebinding/blazn-sandbox-controller-node-observer":"%s",' "$(live_uid clusterrolebinding blazn-sandbox-controller-node-observer -)"
    printf '"serviceaccount/blazn-sandbox-controller":"%s",' "$(live_uid serviceaccount blazn-sandbox-controller blazn-poc-system)"
    printf '"service/blazn-sandbox-access":"%s",' "$(live_uid service blazn-sandbox-access blazn-poc-system)"
    printf '"networkpolicy/blazn-sandbox-controller-access-ingress":"%s",' "$(live_uid networkpolicy blazn-sandbox-controller-access-ingress blazn-poc-system)"
    printf '"networkpolicy/blazn-sandbox-controller-egress":"%s",' "$(live_uid networkpolicy blazn-sandbox-controller-egress blazn-poc-system)"
    printf '"networkpolicy/blazn-sandbox-controller-default-deny":"%s"' "$(live_uid networkpolicy blazn-sandbox-controller-default-deny blazn-poc-system)"
    printf '}\n'
  } >"$uids.tmp"
  jq -e 'to_entries | all(.value | test("^[0-9a-f-]{36}$"))' "$uids.tmp" >/dev/null || { printf 'owned controller identities are incomplete\n' >&2; exit 1; }
  mv "$uids.tmp" "$uids"; chmod 0600 "$uids"
  write_phase applied; phase=applied
fi
if [ "$phase" = applied ]; then
  # Idempotent across a crash between the scale and its journal entry: the
  # scale target is 1, so re-running scale is a no-op, and the recorded UID
  # proves the Deployment is still the one this transaction applied.
  [ "$(jq -er '."deployment/blazn-sandbox-controller"' "$uids")" = "$(live_uid deployment blazn-sandbox-controller blazn-poc-system)" ] || { printf 'controller Deployment identity changed since apply\n' >&2; exit 1; }
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
