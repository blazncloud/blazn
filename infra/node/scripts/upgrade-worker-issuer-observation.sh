#!/bin/sh
set -eu
umask 077
export LC_ALL=C

die(){ printf 'blazn-worker-issuer-upgrade: %s\n' "$*" >&2; exit 1; }
need(){ command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
[ "$(id -u)" -eq 0 ] || die "upgrade requires root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "upgrade requires the control-plane lock"
for command_name in awk dirname grep jq mkdir mv python3 sha256sum stat sync; do need "$command_name"; done

SOURCE=${BLAZN_ISSUER_BINARY_SOURCE:?set BLAZN_ISSUER_BINARY_SOURCE to the reviewed observation-enforced helper}
SOURCE_DIGEST=${BLAZN_ISSUER_BINARY_SHA256:?set BLAZN_ISSUER_BINARY_SHA256 to the reviewed sha256 digest}
BINARY=${BLAZN_ISSUER_BINARY_PATH:-/usr/libexec/blazn/blazn-microk8s-worker-issuer}
RECEIPT=${BLAZN_ISSUER_RECEIPT_PATH:-/var/lib/blazn/ownership/microk8s-worker-issuer.json}
RECOVERY=${BLAZN_ISSUER_RECOVERY_ROOT:-/var/lib/blazn/ownership/microk8s-worker-issuer-recovery}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
ACTIVE=$RECOVERY/observation-upgrade-active
JOURNAL=$ACTIVE/journal.json
PRIOR_BINARY=$ACTIVE/prior-binary
PRIOR_RECEIPT=$ACTIVE/prior-receipt.json
PRIOR_MAIN=$ACTIVE/prior-main.json
TEST_MODE=${BLAZN_ISSUER_INFRA_TEST_MODE:-0}
SYSTEMCTL=${BLAZN_ISSUER_TEST_SYSTEMCTL:-systemctl}
case "$SOURCE:$BINARY:$RECEIPT:$RECOVERY:$MAIN_RECEIPT:$JOURNAL" in /*:/*:/*:/*:/*:/*) ;; *) die "upgrade paths must be absolute" ;; esac
if [ "$TEST_MODE" != 1 ]; then
  need docker
  [ "$BINARY" = /usr/libexec/blazn/blazn-microk8s-worker-issuer ] || die "issuer binary path differs from reviewed contract"
  [ "$RECEIPT" = /var/lib/blazn/ownership/microk8s-worker-issuer.json ] || die "issuer receipt path differs from reviewed contract"
  [ "$RECOVERY" = /var/lib/blazn/ownership/microk8s-worker-issuer-recovery ] || die "issuer recovery path differs from reviewed contract"
  [ "$MAIN_RECEIPT" = /var/lib/blazn/ownership/control-plane.json ] || die "main receipt path differs from reviewed contract"
  if docker ps -q --filter label=com.docker.compose.project=blazn-m2 --filter label=com.docker.compose.service=node-broker | grep . >/dev/null; then die "Node broker sidecar must be stopped before issuer upgrade"; fi
fi
case "$SOURCE_DIGEST" in sha256:????????????????????????????????????????????????????????????????) ;; *) die "helper digest is invalid" ;; esac
case "${SOURCE_DIGEST#sha256:}" in *[!0-9a-f]*) die "helper digest is invalid" ;; esac
[ -f "$SOURCE" ] && [ ! -L "$SOURCE" ] && [ "sha256:$(sha256sum "$SOURCE"|awk '{print $1}')" = "$SOURCE_DIGEST" ] || die "helper source differs from reviewed digest"
if [ "$TEST_MODE" != 1 ]; then [ "$(stat -c '%u:%a:%h' "$SOURCE")" = 0:755:1 ] || die "helper source metadata is unsafe"; fi

sha(){ sha256sum "$1" | awk '{print $1}'; }
sync_path(){ sync -f "$1"; }
now(){ if [ "$TEST_MODE" = 1 ] && [ -n "${BLAZN_ISSUER_UPGRADE_TEST_NOW:-}" ]; then printf '%s\n' "$BLAZN_ISSUER_UPGRADE_TEST_NOW"; else date -u '+%Y-%m-%dT%H:%M:%SZ'; fi; }
fault(){ [ "$TEST_MODE" = 1 ] || return 0; [ "${BLAZN_ISSUER_UPGRADE_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected issuer upgrade fault after $1"; }
write_receipt(){ filter=$1; shift; tmp=$RECEIPT.tmp.$$; jq "$@" "$filter" "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"; }
# shellcheck disable=SC2016
set_phase(){ value=$1; write_receipt '.phase=$phase|.updatedAt=$at' --arg phase "$value" --arg at "$(now)"; phase=$value; fault "$value"; }
material_digest(){ digest_material=$(jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$1"); printf '%s' "$digest_material" | sha256sum | awk '{print "sha256:"$1}'; }
validate_file(){ path=$1; mode=$2; [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c '%u:%a:%h' "$path")" = "0:$mode:1" ] || die "unsafe receipt-bound file: $path"; }
atomic_install(){
  install_source=$1; install_destination=$2; install_digest=$3; install_mode=$4
  python3 - "$install_source" "$install_destination" "$install_digest" "$install_mode" <<'PY'
import hashlib, os, stat, sys, tempfile
source, destination, expected, output_mode = sys.argv[1:]
source_fd = os.open(source, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
try:
    info = os.fstat(source_fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise SystemExit("helper source is unsafe")
    fd, temporary = tempfile.mkstemp(prefix=".issuer-upgrade.", dir=os.path.dirname(destination))
    digest = hashlib.sha256()
    try:
        with os.fdopen(fd, "wb") as output:
            while True:
                chunk = os.read(source_fd, 1024 * 1024)
                if not chunk: break
                digest.update(chunk); output.write(chunk)
            output.flush(); os.fsync(output.fileno())
        if "sha256:" + digest.hexdigest() != expected:
            raise SystemExit("helper source differs from reviewed digest")
        os.chown(temporary, 0, 0); os.chmod(temporary, int(output_mode, 8)); os.replace(temporary, destination)
        parent = os.open(os.path.dirname(destination), os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try: os.fsync(parent)
        finally: os.close(parent)
    finally:
        try: os.unlink(temporary)
        except FileNotFoundError: pass
finally: os.close(source_fd)
PY
}
install_binary(){ atomic_install "$SOURCE" "$BINARY" "$SOURCE_DIGEST" 0755; }
snapshot_file(){
  snapshot_source=$1; snapshot_destination=$2; snapshot_mode=$3; snapshot_digest=$4
  if [ -e "$snapshot_destination" ]; then
    validate_file "$snapshot_destination" "$snapshot_mode"
    [ "sha256:$(sha "$snapshot_destination")" = "$snapshot_digest" ] || die "upgrade recovery snapshot conflicts: $snapshot_destination"
  else
    atomic_install "$snapshot_source" "$snapshot_destination" "$snapshot_digest" "0$snapshot_mode"
  fi
}
validate_active(){ [ -d "$ACTIVE" ] && [ ! -L "$ACTIVE" ] && [ "$(stat -c '%u:%a:%F' "$ACTIVE")" = 0:700:directory ] || die "issuer upgrade recovery directory is unsafe"; }
archive_attempt(){
  validate_active; validate_file "$JOURNAL" 600
  archive_base=$RECOVERY/observation-upgrade-failed-$(sha "$JOURNAL"); archive=$archive_base; archive_suffix=0
  while [ -e "$archive" ]; do
    archive_suffix=$((archive_suffix + 1)); [ "$archive_suffix" -le 1000 ] || die "too many issuer upgrade failure archives"
    archive=$archive_base-$archive_suffix
  done
  mv -- "$ACTIVE" "$archive"; sync_path "$RECOVERY"
}
restore_prior(){
  case "$phase" in upgrade-rollback-started|upgrade-rollback-binary-restored|upgrade-rollback-main-restored) ;; *) set_phase upgrade-rollback-started ;; esac
  if [ "$phase" = upgrade-rollback-started ]; then
    "$SYSTEMCTL" stop blazn-microk8s-worker-issuer.service >/dev/null 2>&1 || true
    atomic_install "$PRIOR_BINARY" "$BINARY" "$(jq -er .priorBinaryDigest "$JOURNAL")" 0755
    set_phase upgrade-rollback-binary-restored
  fi
  if [ "$phase" = upgrade-rollback-binary-restored ]; then
    atomic_install "$PRIOR_MAIN" "$MAIN_RECEIPT" "$(jq -er .priorMainFileDigest "$JOURNAL")" 0600
    set_phase upgrade-rollback-main-restored
  fi
  atomic_install "$PRIOR_RECEIPT" "$RECEIPT" "$(jq -er .priorReceiptDigest "$JOURNAL")" 0600
  fault upgrade-rollback-receipt-restored
  "$SYSTEMCTL" start blazn-microk8s-worker-issuer.service || die "new issuer failed and the prior binary was restored, but its service did not restart"
  if [ "$TEST_MODE" != 1 ]; then "$SYSTEMCTL" is-active --quiet blazn-microk8s-worker-issuer.service || die "new issuer failed and the prior binary was restored, but its service is not active"; fi
  archive_attempt
  die "upgraded issuer service failed; prior binary and receipt bindings were restored"
}

validate_file "$RECEIPT" 600
validate_file "$MAIN_RECEIPT" 600
[ -d "$RECOVERY" ] && [ ! -L "$RECOVERY" ] && [ "$(stat -c '%u:%a:%F' "$RECOVERY")" = 0:700:directory ] || die "issuer recovery root is unsafe"
validate_file "$RECOVERY/inventory.json" 600
[ -d "$(dirname -- "$BINARY")" ] && [ ! -L "$(dirname -- "$BINARY")" ] || die "issuer binary parent is unsafe"
case "$(stat -c '%u:%a:%F' "$(dirname -- "$BINARY")")" in 0:700:directory|0:750:directory|0:755:directory) ;; *) die "issuer binary parent is unsafe" ;; esac
[ "sha256:$(sha "$RECOVERY/inventory.json")" = "$(jq -er .recovery.inventoryDigest "$RECEIPT")" ] || die "recovery inventory differs from receipt"
jq -e --arg host "$(hostname)" '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .owner=="blazn-poc" and .host==$host' "$RECEIPT" >/dev/null || die "issuer receipt is invalid"
[ "$(jq -er .binary.path "$RECEIPT")" = "$BINARY" ] || die "receipt binary path differs"
for binding in config:400 unit:644 tmpfiles:644; do
  field=${binding%%:*}; mode=${binding#*:}; path=$(jq -er --arg field "$field" '.[$field].path' "$RECEIPT"); validate_file "$path" "$mode"
  [ "sha256:$(sha "$path")" = "$(jq -er --arg field "$field" '.[$field].digest' "$RECEIPT")" ] || die "receipt-bound $field changed"
done
secret=$(jq -er .secret.path "$RECEIPT"); validate_file "$secret" 400; [ "sha256:$(sha "$secret")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "receipt-bound secret changed"
environment=$(jq -er .environment.path "$RECEIPT"); environment_backup=$(jq -er .environment.backupPath "$RECEIPT"); validate_file "$environment" 600; validate_file "$environment_backup" 600
[ "sha256:$(sha "$environment")" = "$(jq -er .environment.digest "$RECEIPT")" ] || die "receipt-bound environment changed"
[ "sha256:$(sha "$environment_backup")" = "$(jq -er .environment.priorDigest "$RECEIPT")" ] || die "environment recovery copy changed"
main_backup=$(jq -er .ownership.backupPath "$RECEIPT"); validate_file "$main_backup" 600; [ "sha256:$(sha "$main_backup")" = "$(jq -er .ownership.priorDigest "$RECEIPT")" ] || die "main receipt recovery copy changed"
recovery_key=$(jq -er .secretRecoveryPath "$RECOVERY/inventory.json"); validate_file "$recovery_key" 400; [ "sha256:$(sha "$recovery_key")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "issuer recovery key changed"

phase=$(jq -er .phase "$RECEIPT")
case "$phase" in complete|upgrade-initialized|upgrade-service-stopped|upgrade-binary-installed|upgrade-receipt-updated|upgrade-main-bound|upgrade-service-started|upgrade-rollback-started|upgrade-rollback-binary-restored|upgrade-rollback-main-restored) ;; *) die "issuer receipt is not observation-upgrade resumable" ;; esac

if [ "$phase" = complete ] && [ "$(jq -er .liveJoinBlocked "$RECEIPT")" = false ]; then
  [ "$(jq -er .binary.digest "$RECEIPT")" = "$SOURCE_DIGEST" ] || die "completed observation upgrade uses another binary"
  validate_file "$JOURNAL" 600
  [ "sha256:$(sha "$JOURNAL")" = "$(jq -er .upgrade.journalDigest "$RECEIPT")" ] || die "upgrade journal differs from receipt"
  [ "sha256:$(sha "$BINARY")" = "$SOURCE_DIGEST" ] || die "upgraded binary differs from receipt"
  expected=$(material_digest "$RECEIPT")
  jq -e --arg digest "$expected" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$MAIN_RECEIPT" >/dev/null || die "main receipt does not bind upgraded issuer"
  printf 'MicroK8s worker issuer observation upgrade is already complete\n'
  exit 0
fi

if [ "$phase" = complete ]; then
  [ "$(jq -er .liveJoinBlocked "$RECEIPT")" = true ] || die "complete issuer receipt has an invalid live-join gate"
  jq -e 'has("upgrade")|not' "$RECEIPT" >/dev/null || die "blocked receipt has unexpected upgrade state"
  old_binary=$(jq -er .binary.digest "$RECEIPT")
  [ "$old_binary" != "$SOURCE_DIGEST" ] || die "blocked receipt cannot claim the observation-enforced binary"
  validate_file "$BINARY" 755
  [ "sha256:$(sha "$BINARY")" = "$old_binary" ] || die "installed helper differs from blocked receipt"
  old_material=$(material_digest "$RECEIPT")
  jq -e --arg digest "$old_material" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$MAIN_RECEIPT" >/dev/null || die "main receipt does not bind blocked issuer"
  if [ -e "$ACTIVE" ]; then
    validate_active
    if [ -e "$JOURNAL" ]; then
      validate_file "$JOURNAL" 600
      [ "sha256:$(sha "$RECEIPT")" = "$(jq -er .priorReceiptDigest "$JOURNAL")" ] || die "unfinished issuer upgrade does not bind the restored receipt"
      [ "sha256:$(sha "$BINARY")" = "$(jq -er .priorBinaryDigest "$JOURNAL")" ] || die "unfinished issuer upgrade does not bind the restored binary"
      [ "sha256:$(sha "$MAIN_RECEIPT")" = "$(jq -er .priorMainFileDigest "$JOURNAL")" ] || die "unfinished issuer upgrade does not bind the restored main receipt"
      "$SYSTEMCTL" start blazn-microk8s-worker-issuer.service || die "restored issuer service did not restart"
      if [ "$TEST_MODE" != 1 ]; then "$SYSTEMCTL" is-active --quiet blazn-microk8s-worker-issuer.service || die "restored issuer service is not active"; fi
      archive_attempt
    fi
  fi
  if [ ! -e "$ACTIVE" ]; then mkdir -m 0700 "$ACTIVE"; sync_path "$RECOVERY"; else validate_active; fi
  new_material_json=$(jq --arg digest "$SOURCE_DIGEST" '.binary.digest=$digest|.liveJoinBlocked=false' "$RECEIPT" | jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}')
  new_material=sha256:$(printf '%s' "$new_material_json" | sha256sum | awk '{print $1}')
  prior_main=sha256:$(sha "$MAIN_RECEIPT")
  prior_receipt=sha256:$(sha "$RECEIPT")
  snapshot_file "$BINARY" "$PRIOR_BINARY" 755 "$old_binary"
  snapshot_file "$RECEIPT" "$PRIOR_RECEIPT" 600 "$prior_receipt"
  snapshot_file "$MAIN_RECEIPT" "$PRIOR_MAIN" 600 "$prior_main"
  result_main=$(jq --arg digest "$new_material" '.microk8sIssuer={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$MAIN_RECEIPT" | sha256sum | awk '{print "sha256:"$1}')
  if [ -e "$JOURNAL" ]; then validate_file "$JOURNAL" 600; else
    tmp=$JOURNAL.tmp.$$
    jq -cn --arg createdAt "$(now)" --arg receipt "$RECEIPT" --arg binary "$BINARY" --arg oldBinary "$old_binary" --arg newBinary "$SOURCE_DIGEST" --arg oldMaterial "$old_material" --arg newMaterial "$new_material" --arg oldMain "$prior_main" --arg newMain "$result_main" --arg priorReceipt "$prior_receipt" --arg priorBinaryPath "$PRIOR_BINARY" --arg priorReceiptPath "$PRIOR_RECEIPT" --arg priorMainPath "$PRIOR_MAIN" --arg inventory "sha256:$(sha "$RECOVERY/inventory.json")" '{schemaVersion:"blazn.dev/microk8s-worker-issuer-observation-upgrade/v1",createdAt:$createdAt,receiptPath:$receipt,binaryPath:$binary,priorBinaryPath:$priorBinaryPath,priorReceiptPath:$priorReceiptPath,priorMainPath:$priorMainPath,priorBinaryDigest:$oldBinary,priorReceiptDigest:$priorReceipt,priorMainFileDigest:$oldMain,resultBinaryDigest:$newBinary,priorMaterialDigest:$oldMaterial,resultMaterialDigest:$newMaterial,priorMainDigest:$oldMain,resultMainDigest:$newMain,recoveryInventoryDigest:$inventory}' >"$tmp"
    chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$JOURNAL"; sync_path "$RECOVERY"
  fi
  fault journal-created
  journal_digest=sha256:$(sha "$JOURNAL")
  jq -e --arg receipt "$RECEIPT" --arg binary "$BINARY" --arg oldBinary "$old_binary" --arg newBinary "$SOURCE_DIGEST" --arg oldMaterial "$old_material" --arg newMaterial "$new_material" --arg oldMain "$prior_main" --arg newMain "$result_main" --arg inventory "sha256:$(sha "$RECOVERY/inventory.json")" '.schemaVersion=="blazn.dev/microk8s-worker-issuer-observation-upgrade/v1" and .receiptPath==$receipt and .binaryPath==$binary and .priorBinaryDigest==$oldBinary and .resultBinaryDigest==$newBinary and .priorMaterialDigest==$oldMaterial and .resultMaterialDigest==$newMaterial and .priorMainDigest==$oldMain and .resultMainDigest==$newMain and .recoveryInventoryDigest==$inventory' "$JOURNAL" >/dev/null || die "upgrade journal conflicts with reviewed transition"
  # shellcheck disable=SC2016
  write_receipt '.upgrade={schemaVersion:"blazn.dev/microk8s-worker-issuer-observation-upgrade/v1",journalPath:$journal,journalDigest:$journalDigest,priorBinaryDigest:$oldBinary,resultBinaryDigest:$newBinary,priorMaterialDigest:$oldMaterial,resultMaterialDigest:$newMaterial,priorMainDigest:$oldMain,resultMainDigest:$newMain}|.phase="upgrade-initialized"|.updatedAt=$at' --arg journal "$JOURNAL" --arg journalDigest "$journal_digest" --arg oldBinary "$old_binary" --arg newBinary "$SOURCE_DIGEST" --arg oldMaterial "$old_material" --arg newMaterial "$new_material" --arg oldMain "$prior_main" --arg newMain "$result_main" --arg at "$(now)"
  phase=upgrade-initialized; fault upgrade-initialized
fi

validate_file "$JOURNAL" 600
[ "sha256:$(sha "$JOURNAL")" = "$(jq -er .upgrade.journalDigest "$RECEIPT")" ] || die "upgrade journal differs from receipt"
[ "$(jq -er .upgrade.resultBinaryDigest "$RECEIPT")" = "$(jq -er .resultBinaryDigest "$JOURNAL")" ] || die "upgrade target differs from journal"
[ "$(jq -er '.recoveryInventoryDigest' "$JOURNAL")" = "sha256:$(sha "$RECOVERY/inventory.json")" ] || die "recovery inventory changed during upgrade"
validate_file "$PRIOR_BINARY" 755; [ "sha256:$(sha "$PRIOR_BINARY")" = "$(jq -er .priorBinaryDigest "$JOURNAL")" ] || die "prior issuer recovery binary changed"
validate_file "$PRIOR_RECEIPT" 600; [ "sha256:$(sha "$PRIOR_RECEIPT")" = "$(jq -er .priorReceiptDigest "$JOURNAL")" ] || die "prior issuer recovery receipt changed"
validate_file "$PRIOR_MAIN" 600; [ "sha256:$(sha "$PRIOR_MAIN")" = "$(jq -er .priorMainFileDigest "$JOURNAL")" ] || die "prior issuer recovery main receipt changed"
case "$phase" in
  upgrade-rollback-started|upgrade-rollback-binary-restored|upgrade-rollback-main-restored) restore_prior ;;
  *) [ "$(jq -er .upgrade.resultBinaryDigest "$RECEIPT")" = "$SOURCE_DIGEST" ] || die "upgrade target differs from reviewed helper" ;;
esac
case "$phase" in upgrade-binary-installed|upgrade-receipt-updated|upgrade-main-bound|upgrade-service-started) [ "sha256:$(sha "$BINARY")" = "$SOURCE_DIGEST" ] || die "observation helper changed during upgrade" ;; esac
case "$phase" in upgrade-receipt-updated|upgrade-main-bound|upgrade-service-started) [ "$(material_digest "$RECEIPT")" = "$(jq -er .upgrade.resultMaterialDigest "$RECEIPT")" ] || die "issuer material changed during upgrade" ;; esac
case "$phase" in upgrade-main-bound|upgrade-service-started) [ "sha256:$(sha "$MAIN_RECEIPT")" = "$(jq -er .upgrade.resultMainDigest "$RECEIPT")" ] || die "main receipt changed during upgrade" ;; esac

if [ "$phase" = upgrade-initialized ]; then "$SYSTEMCTL" stop blazn-microk8s-worker-issuer.service; set_phase upgrade-service-stopped; fi
if [ "$phase" = upgrade-service-stopped ]; then install_binary; [ "sha256:$(sha "$BINARY")" = "$SOURCE_DIGEST" ] || die "installed observation helper differs"; set_phase upgrade-binary-installed; fi
if [ "$phase" = upgrade-binary-installed ]; then
  # shellcheck disable=SC2016
  write_receipt '.binary.digest=$digest|.liveJoinBlocked=false|.phase="upgrade-receipt-updated"|.updatedAt=$at' --arg digest "$SOURCE_DIGEST" --arg at "$(now)"
  phase=upgrade-receipt-updated; fault upgrade-receipt-updated
fi
if [ "$phase" = upgrade-receipt-updated ]; then
  [ "$(material_digest "$RECEIPT")" = "$(jq -er .upgrade.resultMaterialDigest "$RECEIPT")" ] || die "upgraded issuer material differs from journal"
  current_main=sha256:$(sha "$MAIN_RECEIPT"); prior_main=$(jq -er .upgrade.priorMainDigest "$RECEIPT"); result_main=$(jq -er .upgrade.resultMainDigest "$RECEIPT")
  if [ "$current_main" = "$prior_main" ]; then
    tmp=$MAIN_RECEIPT.tmp.$$; jq --arg digest "$(jq -er .upgrade.resultMaterialDigest "$RECEIPT")" '.microk8sIssuer={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$MAIN_RECEIPT" >"$tmp"; chmod 0600 "$tmp"; [ "sha256:$(sha "$tmp")" = "$result_main" ] || die "main receipt upgrade result differs from journal"; sync_path "$tmp"; mv -- "$tmp" "$MAIN_RECEIPT"; sync_path "$(dirname -- "$MAIN_RECEIPT")"
  elif [ "$current_main" != "$result_main" ]; then die "main receipt changed during issuer upgrade"
  fi
  set_phase upgrade-main-bound
fi
if [ "$phase" = upgrade-main-bound ]; then
  "$SYSTEMCTL" start blazn-microk8s-worker-issuer.service || restore_prior
  if [ "$TEST_MODE" != 1 ]; then "$SYSTEMCTL" is-active --quiet blazn-microk8s-worker-issuer.service || restore_prior; fi
  set_phase upgrade-service-started
fi
if [ "$phase" = upgrade-service-started ]; then set_phase complete; fi
[ "$phase" = complete ] && [ "$(jq -er .liveJoinBlocked "$RECEIPT")" = false ] || die "observation upgrade did not complete"
[ "sha256:$(sha "$BINARY")" = "$SOURCE_DIGEST" ] || die "upgraded binary differs from reviewed helper"
[ "$(material_digest "$RECEIPT")" = "$(jq -er .upgrade.resultMaterialDigest "$RECEIPT")" ] || die "completed issuer material differs from journal"
[ "sha256:$(sha "$MAIN_RECEIPT")" = "$(jq -er .upgrade.resultMainDigest "$RECEIPT")" ] || die "completed main receipt differs from journal"
printf 'MicroK8s worker issuer observation upgrade is complete; live canary remains required\n'
