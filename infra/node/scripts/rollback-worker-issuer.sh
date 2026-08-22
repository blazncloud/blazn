#!/bin/sh
set -eu
umask 077
export LC_ALL=C
die(){ printf 'blazn-worker-issuer-rollback: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "rollback requires root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "rollback requires the control-plane lock"
for command_name in cp dirname find grep jq mv sha256sum stat sync; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
RECEIPT=${BLAZN_ISSUER_RECEIPT_PATH:-/var/lib/blazn/ownership/microk8s-worker-issuer.json}
STATE_ROOT=${BLAZN_ISSUER_STATE_ROOT:-/var/lib/blazn-node-root/microk8s-worker-issuer}
SYSTEMCTL=${BLAZN_ISSUER_TEST_SYSTEMCTL:-systemctl}
TEST_MODE=${BLAZN_ISSUER_INFRA_TEST_MODE:-0}
case "$STATE_ROOT:$RECEIPT" in /*:/*) ;; *) die "rollback paths must be absolute" ;; esac
if [ "$TEST_MODE" != 1 ]; then
  if [ "$RECEIPT" != /var/lib/blazn/ownership/microk8s-worker-issuer.json ] || [ "$STATE_ROOT" != /var/lib/blazn-node-root/microk8s-worker-issuer ]; then die "issuer rollback paths differ from reviewed contract"; fi
  if docker ps -q --filter label=com.docker.compose.project=blazn-m2 --filter label=com.docker.compose.service=node-broker | grep . >/dev/null; then die "Node broker sidecar must be stopped before issuer rollback"; fi
fi
if [ ! -f "$RECEIPT" ] || [ -L "$RECEIPT" ] || [ "$(stat -c '%u:%a:%h' "$RECEIPT")" != 0:600:1 ]; then die "issuer receipt is unsafe"; fi
jq -e '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .owner=="blazn-poc"' "$RECEIPT" >/dev/null || die "issuer receipt is invalid"
sha(){ sha256sum "$1" | awk '{print $1}'; }
sync_path(){ sync -f "$1"; }
test_fault(){ [ "$TEST_MODE" = 1 ] || return 0; [ "${BLAZN_ISSUER_ROLLBACK_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected issuer rollback fault after $1"; }
write_phase(){ value=$1; tmp=$RECEIPT.tmp.$$; jq --arg phase "$value" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"; }
advance(){ next=$1; test_fault "$next-before-phase"; write_phase "$next"; phase=$next; test_fault "$next"; }
bound_file(){ field=$1; path=$(jq -er --arg field "$field" '.[$field].path' "$RECEIPT"); expected=$(jq -er --arg field "$field" '.[$field].digest' "$RECEIPT"); if [ -e "$path" ] && { [ ! -f "$path" ] || [ -L "$path" ] || [ "sha256:$(sha "$path")" != "$expected" ]; }; then die "receipt-bound $field changed"; fi; printf '%s\n' "$path"; }
remove_bound(){ field=$1; path=$(bound_file "$field"); if [ -e "$path" ]; then rm -f -- "$path"; sync_path "$(dirname -- "$path")"; fi; }

phase=$(jq -er .phase "$RECEIPT")
case "$phase" in complete|rollback-started|service-stopped|rollback-validated|binary-removed|config-removed|secret-removed|unit-removed|tmpfiles-removed|state-removed|environment-restored|main-removal-intent|main-restored|files-restored|rolled-back) ;; *) die "issuer receipt cannot resume rollback" ;; esac
if [ "$phase" = complete ]; then advance rollback-started; fi
if [ "$phase" = rollback-started ]; then "$SYSTEMCTL" disable --now blazn-microk8s-worker-issuer.service; advance service-stopped; fi
if [ "$phase" = service-stopped ]; then
  for field in binary config unit tmpfiles; do path=$(bound_file "$field"); [ -e "$path" ] || die "receipt-bound $field disappeared before rollback intent"; done
  secret=$(jq -er .secret.path "$RECEIPT"); if [ ! -f "$secret" ] || [ -L "$secret" ] || [ "sha256:$(sha "$secret")" != "$(jq -er .secret.digest "$RECEIPT")" ]; then die "receipt-bound secret changed"; fi
  recovery=$(jq -er .recovery.root "$RECEIPT"); [ "sha256:$(sha "$recovery/inventory.json")" = "$(jq -er .recovery.inventoryDigest "$RECEIPT")" ] || die "recovery inventory changed"; recovery_key=$(jq -er .secretRecoveryPath "$recovery/inventory.json"); if [ "$recovery_key" != "$recovery/issuer-hmac-v1" ] || [ "sha256:$(sha "$recovery_key")" != "$(jq -er .secret.digest "$RECEIPT")" ]; then die "recovery key changed"; fi
  env=$(jq -er .environment.path "$RECEIPT"); env_backup=$(jq -er .environment.backupPath "$RECEIPT"); if [ "sha256:$(sha "$env")" != "$(jq -er .environment.digest "$RECEIPT")" ] || [ "sha256:$(sha "$env_backup")" != "$(jq -er .environment.priorDigest "$RECEIPT")" ]; then die "environment or backup changed"; fi
  main=$(jq -er .ownership.path "$RECEIPT"); main_backup=$(jq -er .ownership.backupPath "$RECEIPT"); material=$(jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$RECEIPT"); material_digest=sha256:$(printf '%s' "$material"|sha256sum|awk '{print $1}'); jq -e --arg digest "$material_digest" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$main" >/dev/null || die "main receipt does not bind issuer"; [ "sha256:$(sha "$main_backup")" = "$(jq -er .ownership.priorDigest "$RECEIPT")" ] || die "main receipt backup changed"
  [ "$(jq -er .state.path "$RECEIPT")" = "$STATE_ROOT" ] || die "issuer state root differs from receipt"
  if [ ! -d "$STATE_ROOT" ] || [ -L "$STATE_ROOT" ] || [ "$(stat -c '%u:%a:%F' "$STATE_ROOT")" != "0:700:directory" ]; then die "issuer state root is unsafe"; fi
  state_parent=$(dirname -- "$STATE_ROOT"); if [ ! -d "$state_parent" ] || [ -L "$state_parent" ] || [ "$(stat -c '%u:%a:%F' "$state_parent")" != "0:700:directory" ]; then die "issuer state parent is unsafe"; fi
  if find "$STATE_ROOT" -mindepth 1 -maxdepth 1 -print | grep . >/dev/null; then die "issuer state contains revocation evidence"; fi
  advance rollback-validated
fi
if [ "$phase" = rollback-validated ]; then remove_bound binary; advance binary-removed; fi
if [ "$phase" = binary-removed ]; then remove_bound config; advance config-removed; fi
if [ "$phase" = config-removed ]; then secret=$(jq -er .secret.path "$RECEIPT"); if [ -e "$secret" ]; then if [ ! -f "$secret" ] || [ -L "$secret" ] || [ "sha256:$(sha "$secret")" != "$(jq -er .secret.digest "$RECEIPT")" ]; then die "receipt-bound secret changed"; fi; rm -f -- "$secret"; sync_path "$(dirname -- "$secret")"; fi; root=$(dirname -- "$secret"); if [ -d "$root" ]; then rmdir -- "$root" || die "issuer config root contains residue"; fi; advance secret-removed; fi
if [ "$phase" = secret-removed ]; then remove_bound unit; "$SYSTEMCTL" daemon-reload; advance unit-removed; fi
if [ "$phase" = unit-removed ]; then remove_bound tmpfiles; advance tmpfiles-removed; fi
if [ "$phase" = tmpfiles-removed ]; then if [ -d "$STATE_ROOT" ]; then rmdir -- "$STATE_ROOT" || die "issuer state root contains residue"; fi; advance state-removed; fi
if [ "$phase" = state-removed ]; then env=$(jq -er .environment.path "$RECEIPT"); backup=$(jq -er .environment.backupPath "$RECEIPT"); prior=$(jq -er .environment.priorDigest "$RECEIPT"); current=$(sha "$env"); if [ "sha256:$current" != "$prior" ]; then [ "sha256:$current" = "$(jq -er .environment.digest "$RECEIPT")" ] || die "environment changed during rollback"; tmp=$env.tmp.$$; cp -- "$backup" "$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$env"; sync_path "$(dirname -- "$env")"; fi; advance environment-restored; fi
if [ "$phase" = environment-restored ]; then
  main=$(jq -er .ownership.path "$RECEIPT")
  material=$(jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$RECEIPT")
  material_digest=sha256:$(printf '%s' "$material" | sha256sum | awk '{print $1}')
  jq -e --arg digest "$material_digest" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$main" >/dev/null || die "main receipt changed during rollback"
  before=sha256:$(sha "$main")
  result=sha256:$(jq 'del(.microk8sIssuer)' "$main" | sha256sum | awk '{print $1}')
  tmp=$RECEIPT.tmp.$$; jq --arg before "$before" --arg result "$result" '.rollbackMain={priorDigest:$before,resultDigest:$result}' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"
  advance main-removal-intent
fi
if [ "$phase" = main-removal-intent ]; then
  main=$(jq -er .ownership.path "$RECEIPT"); before=$(jq -er .rollbackMain.priorDigest "$RECEIPT"); result=$(jq -er .rollbackMain.resultDigest "$RECEIPT"); current=sha256:$(sha "$main")
  if [ "$current" = "$before" ]; then
    tmp=$main.tmp.$$; jq 'del(.microk8sIssuer)' "$main" >"$tmp"; chmod 0600 "$tmp"; [ "sha256:$(sha "$tmp")" = "$result" ] || die "main receipt removal result changed"; sync_path "$tmp"; mv -- "$tmp" "$main"; sync_path "$(dirname -- "$main")"
  elif [ "$current" != "$result" ]; then die "main receipt changed during issuer binding removal"
  fi
  advance main-restored
fi
if [ "$phase" = main-restored ]; then advance files-restored; fi
if [ "$phase" = files-restored ]; then tmp=$RECEIPT.tmp.$$; jq --arg at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.rollback={retainedRecovery:true,groupRetained:true,rolledBackAt:$at}' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; advance rolled-back; fi
[ "$phase" = rolled-back ] || die "rollback did not complete"
printf 'issuer service removed; receipt-bound recovery inventory retained\n'
