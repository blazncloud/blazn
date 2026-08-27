#!/bin/sh
set -eu

# Crash-resumable, UID-fenced in-place upgrade of an installed Phase 5
# boundary. Unlike install/rollback, this preserves the namespaces and their
# Secrets while transferring journal authority to a new sealed transaction.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the boundary upgrade must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s RENDERED_MANIFEST\n' "$0" >&2; exit 64; }
manifest=$1
phase4c_require_mutation_authority
: "${BLAZN_PHASE5_TRANSACTION_DIR:?set the new boundary transaction directory}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the new transaction UUID used at render time}"
: "${BLAZN_EXPECTED_BOUNDARY_SHA256:?set the reviewed rendered manifest digest}"
: "${BLAZN_PREVIOUS_BOUNDARY_TRANSACTION_DIR:?set the completed boundary transaction being superseded}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }

transaction=$BLAZN_PHASE5_TRANSACTION_DIR
previous=$BLAZN_PREVIOUS_BOUNDARY_TRANSACTION_DIR
for candidate in "$transaction" "$previous"; do
  case "$candidate" in /var/lib/blazn/phase5/boundary-*) ;; *) printf 'boundary transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
  name=${candidate#/var/lib/blazn/phase5/}
  case "$name" in */*|*..*|'') printf 'boundary transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac
done
[ "$transaction" != "$previous" ] || { printf 'boundary upgrade requires a new transaction\n' >&2; exit 1; }
if ! { [ -d "$previous" ] && [ ! -L "$previous" ] && [ "$(stat -c '%u:%a' "$previous")" = 0:700 ]; }; then
  printf 'previous boundary transaction directory is unsafe\n' >&2; exit 1
fi
previous_phase=$(cat "$previous/phase")
case "$previous_phase" in complete|superseded) ;; *) printf 'previous boundary transaction is not complete\n' >&2; exit 1 ;; esac
previous_uids=$previous/owned-uids.json
[ -f "$previous_uids" ] || { printf 'previous owned identities are missing\n' >&2; exit 1; }

write_phase() { phase4c_write_phase "$transaction" "$1"; }
live_uid() {
  if [ "$#" -eq 3 ]; then kubectl get "$1" "$2" -n "$3" -o jsonpath='{.metadata.uid}'; else kubectl get "$1" "$2" -o jsonpath='{.metadata.uid}'; fi
}
verify_previous_uid() {
  key=$1; shift
  expected=$(jq -er --arg key "$key" '.[$key]' "$previous_uids")
  [ "$expected" = "$(live_uid "$@")" ] || { printf 'live boundary identity changed: %s\n' "$key" >&2; exit 1; }
}
verify_previous_objects() {
  verify_previous_uid namespace/blazn-poc-system namespace blazn-poc-system
  verify_previous_uid namespace/blazn-poc-sandboxes namespace blazn-poc-sandboxes
  verify_previous_uid serviceaccount/blazn-sandbox-runner serviceaccount blazn-sandbox-runner blazn-poc-sandboxes
  verify_previous_uid localqueue/blazn-poc localqueue.kueue.x-k8s.io blazn-poc blazn-poc-sandboxes
  verify_previous_uid role/blazn-agent-sandbox-controller role blazn-agent-sandbox-controller blazn-poc-sandboxes
  verify_previous_uid rolebinding/blazn-agent-sandbox-controller rolebinding blazn-agent-sandbox-controller blazn-poc-sandboxes
  verify_previous_uid validatingadmissionpolicy/blazn-sandbox-boundary validatingadmissionpolicy blazn-sandbox-boundary
  verify_previous_uid validatingadmissionpolicybinding/blazn-sandbox-boundary validatingadmissionpolicybinding blazn-sandbox-boundary
}
record_uids() {
  output=$transaction/owned-uids.json.tmp
  {
    printf '{'
    printf '"namespace/blazn-poc-system":"%s",' "$(live_uid namespace blazn-poc-system)"
    printf '"namespace/blazn-poc-sandboxes":"%s",' "$(live_uid namespace blazn-poc-sandboxes)"
    printf '"serviceaccount/blazn-sandbox-runner":"%s",' "$(live_uid serviceaccount blazn-sandbox-runner blazn-poc-sandboxes)"
    printf '"localqueue/blazn-poc":"%s",' "$(live_uid localqueue.kueue.x-k8s.io blazn-poc blazn-poc-sandboxes)"
    printf '"role/blazn-agent-sandbox-controller":"%s",' "$(live_uid role blazn-agent-sandbox-controller blazn-poc-sandboxes)"
    printf '"rolebinding/blazn-agent-sandbox-controller":"%s",' "$(live_uid rolebinding blazn-agent-sandbox-controller blazn-poc-sandboxes)"
    printf '"validatingadmissionpolicy/blazn-sandbox-boundary":"%s",' "$(live_uid validatingadmissionpolicy blazn-sandbox-boundary)"
    printf '"validatingadmissionpolicybinding/blazn-sandbox-boundary":"%s"' "$(live_uid validatingadmissionpolicybinding blazn-sandbox-boundary)"
    printf '}\n'
  } >"$output"
  jq -e 'length == 8 and to_entries | all(.value | test("^[0-9a-f-]{36}$"))' "$output" >/dev/null || { printf 'upgraded owned identities are incomplete\n' >&2; exit 1; }
  mv "$output" "$transaction/owned-uids.json"
  chmod 0600 "$transaction/owned-uids.json"
}
supersede_previous() {
  successor=$previous/successor
  if [ ! -e "$successor" ]; then
    tmp=$(mktemp "$previous/.successor.XXXXXX")
    printf '%s\n' "$transaction" >"$tmp"
    chmod 0600 "$tmp"
    mv "$tmp" "$successor"
  fi
  [ "$(cat "$successor")" = "$transaction" ] || { printf 'previous boundary transaction has a different successor\n' >&2; exit 1; }
  phase4c_write_phase "$previous" superseded
}

if [ ! -e "$transaction" ]; then
  if ! { [ -f "$manifest" ] && [ ! -L "$manifest" ] && [ "$(stat -c '%h' "$manifest")" = 1 ]; }; then printf 'rendered boundary manifest is unsafe\n' >&2; exit 1; fi
  install -d -o root -g root -m 0700 /var/lib/blazn/phase5 2>/dev/null || :
  install -d -o root -g root -m 0700 "$transaction"
  install -o root -g root -m 0400 "$manifest" "$transaction/boundary.yaml"
  write_phase sealed
fi
sealed=$transaction/boundary.yaml
if [ -L "$sealed" ] || [ ! -f "$sealed" ] || [ "$(stat -c '%u:%a:%h' "$sealed")" != 0:400:1 ]; then printf 'sealed boundary manifest is unsafe\n' >&2; exit 1; fi
[ "$(sha256sum "$sealed" | awk '{print $1}')" = "$BLAZN_EXPECTED_BOUNDARY_SHA256" ] || { printf 'sealed boundary manifest digest mismatch\n' >&2; exit 1; }
[ "$(grep -c "blazn.dev/phase5-transaction: $BLAZN_PHASE5_TRANSACTION_ID" "$sealed")" -ge 8 ] || { printf 'sealed manifest does not carry this transaction identity\n' >&2; exit 1; }
phase=$(cat "$transaction/phase")
case "$phase" in
  complete) supersede_previous; printf 'Phase 5 boundary upgrade transaction is already complete\n'; exit 0 ;;
  rollback-complete|superseded) printf 'boundary upgrade transaction cannot be resumed from %s\n' "$phase" >&2; exit 1 ;;
  sealed|apply-intent|applied) ;;
  *) printf 'boundary upgrade transaction phase is invalid\n' >&2; exit 1 ;;
esac

verify_previous_objects
if [ "$phase" = sealed ]; then write_phase apply-intent; phase=apply-intent; fi
if [ "$phase" = apply-intent ]; then
  kubectl apply --server-side --field-manager blazn-phase5-boundary -f "$sealed" >/dev/null
  write_phase applied; phase=applied
fi
if [ "$phase" = applied ]; then
  record_uids
  [ "$(kubectl get validatingadmissionpolicy blazn-sandbox-boundary -o jsonpath='{.metadata.annotations.blazn\.dev/phase5-transaction}')" = "$BLAZN_PHASE5_TRANSACTION_ID" ] || { printf 'boundary policy did not adopt the successor transaction\n' >&2; exit 1; }
  [ "$(kubectl get validatingadmissionpolicy blazn-sandbox-boundary -o jsonpath='{.spec.failurePolicy}')" = Fail ] || { printf 'boundary policy is not fail-closed\n' >&2; exit 1; }
  [ "$(kubectl get validatingadmissionpolicybinding blazn-sandbox-boundary -o jsonpath='{.spec.validationActions[0]}')" = Deny ] || { printf 'boundary binding does not deny\n' >&2; exit 1; }
  write_phase complete
fi
supersede_previous
printf 'Phase 5 boundary upgraded in place and verified\n'
