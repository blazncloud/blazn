#!/bin/sh
set -eu
die(){ printf 'blazn-worker-issuer-rollback: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "rollback requires root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "rollback requires the control-plane lock"
for command_name in jq sha256sum stat sync; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
RECEIPT=${BLAZN_ISSUER_RECEIPT_PATH:-/var/lib/blazn/ownership/microk8s-worker-issuer.json}
SYSTEMCTL=${BLAZN_ISSUER_TEST_SYSTEMCTL:-systemctl}
if [ "${BLAZN_ISSUER_INFRA_TEST_MODE:-0}" != 1 ]; then command -v docker >/dev/null 2>&1 || die "docker is required"; if docker ps -q --filter label=com.docker.compose.project=blazn-m2 --filter label=com.docker.compose.service=node-broker | grep . >/dev/null; then die "Node broker sidecar must be stopped before issuer rollback"; fi; fi
[ -f "$RECEIPT" ] && [ ! -L "$RECEIPT" ] && [ "$(stat -c '%u:%a' "$RECEIPT")" = 0:600 ] || die "issuer receipt is unsafe"
jq -e '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .owner=="blazn-poc" and (.phase=="complete" or .phase=="rollback-started" or .phase=="service-stopped" or .phase=="files-restored" or .phase=="rolled-back")' "$RECEIPT" >/dev/null || die "issuer receipt cannot be rolled back"
sha(){ sha256sum "$1" | awk '{print $1}'; }
sync_path(){ sync -f "$1"; }
write_phase(){ value=$1; tmp=$RECEIPT.tmp.$$; jq --arg phase "$value" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"; }
phase=$(jq -er .phase "$RECEIPT")
[ "$phase" != complete ] || { write_phase rollback-started; phase=rollback-started; }
if [ "$phase" = rollback-started ]; then "$SYSTEMCTL" disable --now blazn-microk8s-worker-issuer.service; write_phase service-stopped; phase=service-stopped; fi
if [ "$phase" = service-stopped ]; then
  for field in binary config unit tmpfiles; do path=$(jq -er --arg field "$field" '.[$field].path' "$RECEIPT"); expected=$(jq -er --arg field "$field" '.[$field].digest' "$RECEIPT"); [ -f "$path" ] && [ ! -L "$path" ] && [ "sha256:$(sha "$path")" = "$expected" ] || die "receipt-bound $field changed before rollback"; done
  secret=$(jq -er .secret.path "$RECEIPT"); [ -f "$secret" ] && [ ! -L "$secret" ] && [ "sha256:$(sha "$secret")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "receipt-bound secret changed before rollback"
  env=$(jq -er .environment.path "$RECEIPT"); env_backup=$(jq -er .environment.backupPath "$RECEIPT"); [ -f "$env" ] && [ ! -L "$env" ] && [ "sha256:$(sha "$env")" = "$(jq -er .environment.digest "$RECEIPT")" ] || die "receipt-bound environment changed before rollback"; [ -f "$env_backup" ] && [ ! -L "$env_backup" ] && [ "sha256:$(sha "$env_backup")" = "$(jq -er .environment.priorDigest "$RECEIPT")" ] || die "environment backup changed before rollback"
  binary=$(jq -er .binary.path "$RECEIPT"); config=$(jq -er .config.path "$RECEIPT"); unit=$(jq -er .unit.path "$RECEIPT"); tmpfiles=$(jq -er .tmpfiles.path "$RECEIPT")
  rm -f -- "$binary" "$config" "$secret" "$unit" "$tmpfiles"
  env_tmp=$env.tmp.$$; cp -- "$env_backup" "$env_tmp"; chmod 0600 "$env_tmp"; sync_path "$env_tmp"; mv -- "$env_tmp" "$env"; sync_path "$(dirname -- "$env")"
  root=$(dirname -- "$config"); rmdir -- "$root" 2>/dev/null || die "issuer config root contains unreviewed residue"
  "$SYSTEMCTL" daemon-reload
  write_phase files-restored; phase=files-restored
fi
if [ "$phase" = files-restored ]; then
  tmp=$RECEIPT.tmp.$$; jq --arg at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.rollback={retainedRecovery:true,groupRetained:true,rolledBackAt:$at}' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; write_phase rolled-back; phase=rolled-back
fi
[ "$phase" = rolled-back ] || die "rollback did not complete"
printf 'issuer service removed; receipt-bound recovery key and empty broker group retained\n'
