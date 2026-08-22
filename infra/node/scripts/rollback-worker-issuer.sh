#!/bin/sh
set -eu
die(){ printf 'blazn-worker-issuer-rollback: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "rollback requires root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "rollback requires the control-plane lock"
for command_name in jq sha256sum stat sync; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
RECEIPT=${BLAZN_ISSUER_RECEIPT_PATH:-/var/lib/blazn/ownership/microk8s-worker-issuer.json}
SYSTEMCTL=${BLAZN_ISSUER_TEST_SYSTEMCTL:-systemctl}
STATE_ROOT=${BLAZN_ISSUER_STATE_ROOT:-/var/lib/blazn/microk8s-worker-issuer}
case "$STATE_ROOT" in /*) ;; *) die "issuer state root must be absolute" ;; esac
if [ "${BLAZN_ISSUER_INFRA_TEST_MODE:-0}" != 1 ]; then command -v docker >/dev/null 2>&1 || die "docker is required"; if docker ps -q --filter label=com.docker.compose.project=blazn-m2 --filter label=com.docker.compose.service=node-broker | grep . >/dev/null; then die "Node broker sidecar must be stopped before issuer rollback"; fi; fi
if [ "${BLAZN_ISSUER_INFRA_TEST_MODE:-0}" != 1 ] && { [ "$RECEIPT" != /var/lib/blazn/ownership/microk8s-worker-issuer.json ] || [ "$STATE_ROOT" != /var/lib/blazn/microk8s-worker-issuer ]; }; then die "issuer rollback paths differ from reviewed contract"; fi
[ -f "$RECEIPT" ] && [ ! -L "$RECEIPT" ] && [ "$(stat -c '%u:%a' "$RECEIPT")" = 0:600 ] || die "issuer receipt is unsafe"
jq -e '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .owner=="blazn-poc" and (.phase=="complete" or .phase=="rollback-started" or .phase=="service-stopped" or .phase=="files-restored" or .phase=="rolled-back")' "$RECEIPT" >/dev/null || die "issuer receipt cannot be rolled back"
sha(){ sha256sum "$1" | awk '{print $1}'; }
sync_path(){ sync -f "$1"; }
write_phase(){ value=$1; tmp=$RECEIPT.tmp.$$; jq --arg phase "$value" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"; }
phase=$(jq -er .phase "$RECEIPT")
if [ "$phase" = complete ]; then main=$(jq -er .ownership.path "$RECEIPT"); main_backup=$(jq -er .ownership.backupPath "$RECEIPT"); material=$(jq -cS '{binary,config,unit,tmpfiles,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$RECEIPT"); material_digest=sha256:$(printf '%s' "$material" | sha256sum | awk '{print $1}'); jq -e --arg digest "$material_digest" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$main" >/dev/null || die "main ownership receipt does not bind issuer material"; [ "sha256:$(sha "$main_backup")" = "$(jq -er .ownership.priorDigest "$RECEIPT")" ] || die "main ownership backup changed"; write_phase rollback-started; phase=rollback-started; fi
if [ "$phase" = rollback-started ]; then "$SYSTEMCTL" disable --now blazn-microk8s-worker-issuer.service; write_phase service-stopped; phase=service-stopped; fi
if [ "$phase" = service-stopped ]; then
  main=$(jq -er .ownership.path "$RECEIPT"); main_backup=$(jq -er .ownership.backupPath "$RECEIPT"); material=$(jq -cS '{binary,config,unit,tmpfiles,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$RECEIPT"); material_digest=sha256:$(printf '%s' "$material" | sha256sum | awk '{print $1}'); jq -e --arg digest "$material_digest" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$main" >/dev/null || die "main ownership receipt changed during rollback"; [ "sha256:$(sha "$main_backup")" = "$(jq -er .ownership.priorDigest "$RECEIPT")" ] || die "main ownership backup changed during rollback"
  if [ -e "$STATE_ROOT" ]; then [ -d "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] || die "issuer state root is unsafe"; if find "$STATE_ROOT" -mindepth 1 -maxdepth 1 -print | grep . >/dev/null; then die "issuer state contains revocation evidence and cannot be removed"; fi; rmdir -- "$STATE_ROOT" || die "empty issuer state root could not be removed"; fi
  for field in binary config unit tmpfiles; do path=$(jq -er --arg field "$field" '.[$field].path' "$RECEIPT"); expected=$(jq -er --arg field "$field" '.[$field].digest' "$RECEIPT"); [ -f "$path" ] && [ ! -L "$path" ] && [ "sha256:$(sha "$path")" = "$expected" ] || die "receipt-bound $field changed before rollback"; done
  secret=$(jq -er .secret.path "$RECEIPT"); [ -f "$secret" ] && [ ! -L "$secret" ] && [ "sha256:$(sha "$secret")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "receipt-bound secret changed before rollback"
  recovery=$(jq -er .recovery.root "$RECEIPT"); [ "sha256:$(sha "$recovery/inventory.json")" = "$(jq -er .recovery.inventoryDigest "$RECEIPT")" ] || die "receipt-bound recovery inventory changed"; recovery_key=$(jq -er .secretRecoveryPath "$recovery/inventory.json"); [ "$recovery_key" = "$recovery/issuer-hmac-v1" ] && [ -f "$recovery_key" ] && [ ! -L "$recovery_key" ] && [ "$(stat -c '%u:%a' "$recovery_key")" = 0:400 ] && [ "sha256:$(sha "$recovery_key")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "receipt-bound recovery key changed before rollback"
  env=$(jq -er .environment.path "$RECEIPT"); env_backup=$(jq -er .environment.backupPath "$RECEIPT"); [ -f "$env" ] && [ ! -L "$env" ] && [ "sha256:$(sha "$env")" = "$(jq -er .environment.digest "$RECEIPT")" ] || die "receipt-bound environment changed before rollback"; [ -f "$env_backup" ] && [ ! -L "$env_backup" ] && [ "sha256:$(sha "$env_backup")" = "$(jq -er .environment.priorDigest "$RECEIPT")" ] || die "environment backup changed before rollback"
  binary=$(jq -er .binary.path "$RECEIPT"); config=$(jq -er .config.path "$RECEIPT"); unit=$(jq -er .unit.path "$RECEIPT"); tmpfiles=$(jq -er .tmpfiles.path "$RECEIPT")
  rm -f -- "$binary" "$config" "$secret" "$unit" "$tmpfiles"
  env_tmp=$env.tmp.$$; cp -- "$env_backup" "$env_tmp"; chmod 0600 "$env_tmp"; sync_path "$env_tmp"; mv -- "$env_tmp" "$env"; sync_path "$(dirname -- "$env")"
  main=$(jq -er .ownership.path "$RECEIPT"); main_backup=$(jq -er .ownership.backupPath "$RECEIPT"); main_tmp=$main.tmp.$$; cp -- "$main_backup" "$main_tmp"; chmod 0600 "$main_tmp"; sync_path "$main_tmp"; mv -- "$main_tmp" "$main"; sync_path "$(dirname -- "$main")"
  root=$(dirname -- "$config"); rmdir -- "$root" 2>/dev/null || die "issuer config root contains unreviewed residue"
  "$SYSTEMCTL" daemon-reload
  write_phase files-restored; phase=files-restored
fi
if [ "$phase" = files-restored ]; then
  tmp=$RECEIPT.tmp.$$; jq --arg at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.rollback={retainedRecovery:true,groupRetained:true,rolledBackAt:$at}' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; write_phase rolled-back; phase=rolled-back
fi
[ "$phase" = rolled-back ] || die "rollback did not complete"
printf 'issuer service removed; receipt-bound recovery key and empty broker group retained\n'
