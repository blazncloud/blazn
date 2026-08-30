#!/bin/sh
set -eu
umask 077
export LC_ALL=C

die(){ printf 'blazn-worker-issuer-infra: %s\n' "$*" >&2; exit 1; }
need(){ command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
[ "$(id -u)" -eq 0 ] || die "installation requires root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "installation requires the control-plane lock"
for command_name in awk cmp cp dirname find getent grep jq mv openssl python3 rm sha256sum stat sync wc xxd; do need "$command_name"; done

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SOURCE=${BLAZN_ISSUER_BINARY_SOURCE:?set BLAZN_ISSUER_BINARY_SOURCE to the reviewed helper binary}
SOURCE_DIGEST=${BLAZN_ISSUER_BINARY_SHA256:?set BLAZN_ISSUER_BINARY_SHA256 to the reviewed sha256 digest}
UNIT_SOURCE=$SCRIPT_DIR/../systemd/blazn-microk8s-worker-issuer.service
UNIT_SOURCE_DIGEST=sha256:1051d5a13db6cadc5ef4aa9d4f8e27b334feaa1a740c89607d33b9b5a489b508
TMPFILES_SOURCE=$SCRIPT_DIR/../systemd/blazn-microk8s-worker-issuer.tmpfiles
TMPFILES_SOURCE_DIGEST=sha256:ab1492fc38be84b26bb6acc91a933560b304c3bc5412196f70c608cb5797ae39
ROOT=${BLAZN_ISSUER_CONFIG_ROOT:-/etc/blazn/microk8s-worker-issuer}
BINARY=${BLAZN_ISSUER_BINARY_PATH:-/usr/libexec/blazn/blazn-microk8s-worker-issuer}
UNIT=${BLAZN_ISSUER_UNIT_PATH:-/etc/systemd/system/blazn-microk8s-worker-issuer.service}
TMPFILES=${BLAZN_ISSUER_TMPFILES_PATH:-/etc/tmpfiles.d/blazn-microk8s-worker-issuer.conf}
STATE_ROOT=${BLAZN_ISSUER_STATE_ROOT:-/var/lib/blazn-node-root/microk8s-worker-issuer}
RECEIPT=${BLAZN_ISSUER_RECEIPT_PATH:-/var/lib/blazn/ownership/microk8s-worker-issuer.json}
RECOVERY=${BLAZN_ISSUER_RECOVERY_ROOT:-/var/lib/blazn/ownership/microk8s-worker-issuer-recovery}
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
BROKER_UID=${BLAZN_NODE_BROKER_UID:?set the receipt-bound node broker UID}
TEST_MODE=${BLAZN_ISSUER_INFRA_TEST_MODE:-0}
case "$BROKER_UID" in ''|*[!0-9]*|0) die "broker UID must be a positive integer" ;; esac
for path in "$SOURCE" "$ROOT" "$BINARY" "$UNIT" "$TMPFILES" "$STATE_ROOT" "$RECEIPT" "$RECOVERY" "$ENV_FILE" "$MAIN_RECEIPT"; do case "$path" in /*) ;; *) die "all issuer paths must be absolute" ;; esac; done
if [ ! -f "$SOURCE" ] || [ -L "$SOURCE" ]; then die "helper binary source is unavailable or linked"; fi
case "$SOURCE_DIGEST" in sha256:*) digest_value=${SOURCE_DIGEST#sha256:}; case "$digest_value" in ''|*[!0-9a-f]*) die "helper digest is invalid" ;; esac; [ "${#digest_value}" -eq 64 ] || die "helper digest length is invalid" ;; *) die "helper digest is invalid" ;; esac
[ "sha256:$(sha256sum "$SOURCE" | awk '{print $1}')" = "$SOURCE_DIGEST" ] || die "helper binary source differs from reviewed digest"
if [ ! -f "$ENV_FILE" ] || [ -L "$ENV_FILE" ] || [ "$(stat -c '%u:%a:%h' "$ENV_FILE")" != 0:600:1 ]; then die "control-plane environment is unsafe"; fi
if [ ! -f "$MAIN_RECEIPT" ] || [ -L "$MAIN_RECEIPT" ] || [ "$(stat -c '%u:%a:%h' "$MAIN_RECEIPT")" != 0:600:1 ]; then die "main ownership receipt is unsafe"; fi

sha(){ sha256sum "$1" | awk '{print $1}'; }
sync_path(){ sync -f "$1"; }
install_artifact(){
  source_path=$1; destination_path=$2; expected_digest=$3; source_mode=$4; destination_mode=$5
  python3 - "$source_path" "$destination_path" "$expected_digest" "$source_mode" "$destination_mode" "$TEST_MODE" <<'PY'
import hashlib
import os
import stat
import sys
import tempfile

source, destination, expected, source_mode, destination_mode, test_mode = sys.argv[1:]
if not os.path.isabs(source) or not os.path.isabs(destination):
    raise SystemExit("issuer artifact paths must be absolute")
if len(expected) != 71 or not expected.startswith("sha256:"):
    raise SystemExit("issuer artifact digest is invalid")
flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
source_fd = os.open(source, flags)
try:
    source_info = os.fstat(source_fd)
    if not stat.S_ISREG(source_info.st_mode) or source_info.st_nlink != 1:
        raise SystemExit("issuer artifact source type or link count is unsafe")
    if test_mode != "1" and (source_info.st_uid != 0 or stat.S_IMODE(source_info.st_mode) != int(source_mode, 8)):
        raise SystemExit("issuer artifact source owner or mode is unsafe")
    parent = os.path.dirname(destination)
    temporary_fd, temporary = tempfile.mkstemp(prefix=".issuer-artifact.", dir=parent)
    digest = hashlib.sha256()
    try:
        with os.fdopen(temporary_fd, "wb", closefd=True) as output:
            while True:
                chunk = os.read(source_fd, 1024 * 1024)
                if not chunk:
                    break
                digest.update(chunk)
                output.write(chunk)
            output.flush()
            os.fsync(output.fileno())
        if "sha256:" + digest.hexdigest() != expected:
            raise SystemExit("issuer artifact source differs from reviewed digest")
        os.chown(temporary, 0, 0)
        os.chmod(temporary, int(destination_mode, 8))
        os.replace(temporary, destination)
        parent_fd = os.open(parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
finally:
    os.close(source_fd)
PY
}
validate_key(){
  key=$1; if [ ! -f "$key" ] || [ -L "$key" ] || [ "$(stat -c '%u:%a:%h' "$key")" != 0:400:1 ]; then die "issuer key is unsafe"; fi
  if [ "$(wc -c <"$key" | tr -d ' ')" != 43 ] || ! LC_ALL=C grep -Eq '^[A-Za-z0-9_-]{43}$' "$key"; then die "issuer key encoding is invalid"; fi
  decoded_bytes=$({ tr '_-' '/+' <"$key"; printf '='; } | openssl base64 -d -A 2>/dev/null | wc -c | tr -d ' ')
  [ "$decoded_bytes" = 32 ] || die "issuer key entropy length is invalid"
}
bind_environment(){
  for binding in "BLAZN_NODE_BROKER_LOOPBACK=enabled" "BLAZN_NODE_BROKER_UID=$BROKER_UID" "BLAZN_NODE_BROKER_GID=$BROKER_GID"; do
    name=${binding%%=*}; count=$(grep -c "^$name=" "$ENV_FILE" || true)
    case "$count" in
      0) tmp=$ENV_FILE.tmp.$$; awk -v binding="$binding" '{print} END{print binding}' "$ENV_FILE" >"$tmp" ;;
      1) if grep -Fx "$binding" "$ENV_FILE" >/dev/null; then continue; elif [ "$name" = BLAZN_NODE_BROKER_LOOPBACK ] && grep -Fx 'BLAZN_NODE_BROKER_LOOPBACK=disabled' "$ENV_FILE" >/dev/null; then tmp=$ENV_FILE.tmp.$$; awk -v name="$name" -v binding="$binding" 'index($0,name"=")==1{print binding;next}{print}' "$ENV_FILE" >"$tmp"; else die "existing $name environment binding conflicts"; fi ;;
      *) die "duplicate $name environment bindings" ;;
    esac
    chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$ENV_FILE"; sync_path "$(dirname -- "$ENV_FILE")"
  done
}
bind_main_receipt(){
  material=$(jq -cS '{binary,config,unit,tmpfiles,state,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$RECEIPT"); material_digest=sha256:$(printf '%s' "$material" | sha256sum | awk '{print $1}')
  if jq -e --arg digest "$material_digest" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$MAIN_RECEIPT" >/dev/null; then return; fi
  jq -e 'has("microk8sIssuer")|not' "$MAIN_RECEIPT" >/dev/null || die "main ownership receipt has a conflicting issuer binding"
  tmp=$MAIN_RECEIPT.tmp.$$; jq --arg digest "$material_digest" '.microk8sIssuer={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$MAIN_RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$MAIN_RECEIPT"; sync_path "$(dirname -- "$MAIN_RECEIPT")"
}
phase(){ value=$1; tmp=$RECEIPT.tmp.$$; jq --arg phase "$value" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"; }
fault(){ [ "$TEST_MODE" = 1 ] || return 0; [ "${BLAZN_ISSUER_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected issuer fault after $1"; }
safe_parent(){ candidate=$1; while [ "$candidate" != / ]; do [ ! -L "$candidate" ] || die "issuer path contains a symlink: $candidate"; candidate=$(dirname -- "$candidate"); done; }
validate_managed_parent(){
  parent=$1
  if [ ! -d "$parent" ] || [ -L "$parent" ]; then die "managed issuer parent is unavailable or linked: $parent"; fi
  case "$(stat -c '%u:%a:%F' "$parent")" in
    0:700:directory|0:750:directory|0:755:directory) ;;
    *) die "managed issuer parent owner or mode is unsafe: $parent" ;;
  esac
}
for path in "$ROOT" "$BINARY" "$UNIT" "$TMPFILES" "$STATE_ROOT" "$RECEIPT" "$RECOVERY" "$ENV_FILE" "$MAIN_RECEIPT"; do safe_parent "$path"; done

if [ "$TEST_MODE" = 1 ]; then
  BROKER_GID=${BLAZN_ISSUER_TEST_BROKER_GID:?}
  MICROK8S_GID=${BLAZN_ISSUER_TEST_MICROK8S_GID:?}
  MICROK8S_REVISION=${BLAZN_ISSUER_TEST_REVISION:-9072}
  SYSTEMCTL=${BLAZN_ISSUER_TEST_SYSTEMCTL:?}
  TMPFILES_CMD=${BLAZN_ISSUER_TEST_TMPFILES:?}
  group_created=false
else
  if [ "$ROOT" != /etc/blazn/microk8s-worker-issuer ] || [ "$BINARY" != /usr/libexec/blazn/blazn-microk8s-worker-issuer ] || [ "$UNIT" != /etc/systemd/system/blazn-microk8s-worker-issuer.service ] || [ "$TMPFILES" != /etc/tmpfiles.d/blazn-microk8s-worker-issuer.conf ] || [ "$STATE_ROOT" != /var/lib/blazn-node-root/microk8s-worker-issuer ] || [ "$RECEIPT" != /var/lib/blazn/ownership/microk8s-worker-issuer.json ] || [ "$RECOVERY" != /var/lib/blazn/ownership/microk8s-worker-issuer-recovery ] || [ "$ENV_FILE" != /etc/blazn/control-plane/control-plane.env ] || [ "$MAIN_RECEIPT" != /var/lib/blazn/ownership/control-plane.json ]; then die "issuer production paths differ from the reviewed contract"; fi
  [ "$(stat -c '%u:%a:%h' "$SOURCE")" = 0:755:1 ] || die "helper binary source ownership, mode, or link count is unsafe"
  getent group blazn-node-broker >/dev/null || die "dedicated blazn-node-broker group must be provisioned before this transaction"
  group_created=false
  BROKER_GID=$(getent group blazn-node-broker | awk -F: '{print $3}')
  broker_passwd=$(getent passwd blazn-node-broker) || die "dedicated broker account is unavailable"
  IFS=: read -r broker_name _ broker_uid broker_primary_gid _ broker_home broker_shell <<EOF
$broker_passwd
EOF
  if [ "$broker_name" != blazn-node-broker ] || [ "$broker_uid" != "$BROKER_UID" ] || [ "$broker_primary_gid" != "$BROKER_GID" ] || [ "$broker_home" != /nonexistent ] || [ "$broker_shell" != /usr/sbin/nologin ]; then die "dedicated broker account attributes differ from contract"; fi
  [ "$(getent passwd "$BROKER_UID" | awk -F: '{print $1}')" = blazn-node-broker ] || die "broker UID collides with another account"
  [ "$(getent group "$BROKER_GID" | awk -F: '{print $1}')" = blazn-node-broker ] || die "broker GID collides with another group"
  [ -z "$(getent group blazn-node-broker | awk -F: '{print $4}')" ] || die "broker group has unexpected supplementary members"
  [ "$(id -G blazn-node-broker)" = "$BROKER_GID" ] || die "broker account has unexpected supplementary groups"
  MICROK8S_GID=$(getent group microk8s | awk -F: '{print $3}')
  [ -n "$MICROK8S_GID" ] || die "MicroK8s group is unavailable"
  current=$(readlink /snap/microk8s/current) || die "MicroK8s current revision is unavailable"
  MICROK8S_REVISION=${current##*/}
  grep -Fx 'version: v1.35.6' /snap/microk8s/current/meta/snap.yaml >/dev/null || die "MicroK8s version is unsupported"
  SYSTEMCTL=systemctl
  TMPFILES_CMD=systemd-tmpfiles
fi
case "$BROKER_GID:$MICROK8S_GID:$MICROK8S_REVISION" in *[!0-9:]*|*::*|:*) die "resolved group/revision binding is invalid" ;; esac
case "$MICROK8S_REVISION" in 9072|9075) ;; *) die "MicroK8s revision is unsupported" ;; esac

mkdir -p -- "$(dirname -- "$RECEIPT")"
chmod 0700 "$(dirname -- "$RECEIPT")"
[ "$(stat -c '%u:%a' "$(dirname -- "$RECEIPT")")" = 0:700 ] || die "ownership directory is unsafe before receipt publication"
if [ ! -e "$RECEIPT" ]; then
  for managed in "$ROOT" "$BINARY" "$UNIT" "$TMPFILES"; do [ ! -e "$managed" ] || die "managed issuer path exists without its receipt: $managed"; done
  mkdir -p -- "$RECOVERY"
  chmod 0700 "$RECOVERY"
  [ "$(stat -c '%u:%a' "$RECOVERY")" = 0:700 ] || die "pre-receipt recovery directory is unsafe"
  inventory=$RECOVERY/inventory.json
  if find "$RECOVERY" -mindepth 1 -maxdepth 1 ! -name inventory.json ! -name control-plane.env ! -name control-plane.json -print | grep . >/dev/null; then die "pre-receipt recovery root contains unreviewed residue"; fi
  if [ -e "$RECOVERY/control-plane.env" ]; then if [ ! -f "$RECOVERY/control-plane.env" ] || [ -L "$RECOVERY/control-plane.env" ] || [ "$(stat -c '%u:%a:%h' "$RECOVERY/control-plane.env")" != 0:600:1 ]; then die "pre-receipt environment backup is unsafe"; fi; cmp "$ENV_FILE" "$RECOVERY/control-plane.env" >/dev/null || die "pre-receipt environment backup differs"; else cp -- "$ENV_FILE" "$RECOVERY/control-plane.env"; chmod 0600 "$RECOVERY/control-plane.env"; sync_path "$RECOVERY/control-plane.env"; fi
  if [ -e "$RECOVERY/control-plane.json" ]; then if [ ! -f "$RECOVERY/control-plane.json" ] || [ -L "$RECOVERY/control-plane.json" ] || [ "$(stat -c '%u:%a:%h' "$RECOVERY/control-plane.json")" != 0:600:1 ]; then die "pre-receipt ownership backup is unsafe"; fi; cmp "$MAIN_RECEIPT" "$RECOVERY/control-plane.json" >/dev/null || die "pre-receipt ownership backup differs"; else cp -- "$MAIN_RECEIPT" "$RECOVERY/control-plane.json"; chmod 0600 "$RECOVERY/control-plane.json"; sync_path "$RECOVERY/control-plane.json"; fi
  inventory_tmp=$inventory.tmp.$$
  jq -cn --arg source "$SOURCE_DIGEST" --arg key "$RECOVERY/issuer-hmac-v1" --arg env "$RECOVERY/control-plane.env" --arg envDigest "sha256:$(sha "$ENV_FILE")" --arg ownership "$RECOVERY/control-plane.json" --arg ownershipDigest "sha256:$(sha "$MAIN_RECEIPT")" '{schemaVersion:"blazn.dev/microk8s-worker-issuer-recovery/v1",prior:{config:"absent",binary:"absent",unit:"absent",tmpfiles:"absent",environment:{backupPath:$env,digest:$envDigest},ownership:{backupPath:$ownership,digest:$ownershipDigest}},sourceDigest:$source,secretRecoveryPath:$key}' >"$inventory_tmp"
  chmod 0600 "$inventory_tmp"; sync_path "$inventory_tmp"; mv -- "$inventory_tmp" "$inventory"
  chmod 0600 "$inventory"; sync_path "$inventory"
  fault recovery-created
  tmp=$RECEIPT.tmp.$$
  jq -cn --arg host "$(hostname)" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg binary "$BINARY" --arg config "$ROOT/config.json" --arg secret "$ROOT/issuer-hmac-v1" --argjson brokerUid "$BROKER_UID" --argjson brokerGid "$BROKER_GID" --argjson microGid "$MICROK8S_GID" --argjson revision "$MICROK8S_REVISION" --arg recovery "$RECOVERY" --arg inventoryDigest "sha256:$(sha "$inventory")" --argjson groupCreated "$group_created" \
    --arg unit "$UNIT" --arg tmpfiles "$TMPFILES" --arg state "$STATE_ROOT" --arg env "$ENV_FILE" --arg envBackup "$RECOVERY/control-plane.env" --arg envPrior "sha256:$(sha "$ENV_FILE")" --arg ownership "$MAIN_RECEIPT" --arg ownershipBackup "$RECOVERY/control-plane.json" --arg ownershipPrior "sha256:$(sha "$MAIN_RECEIPT")" '{schemaVersion:"blazn.dev/microk8s-worker-issuer-infra/v1",owner:"blazn-poc",host:$host,phase:"initialized",createdAt:$createdAt,binary:{path:$binary,digest:("sha256:"+("0"*64)),uid:0,mode:"0755"},config:{path:$config,digest:("sha256:"+("0"*64)),uid:0,mode:"0400"},unit:{path:$unit,digest:("sha256:"+("0"*64)),uid:0,mode:"0644"},tmpfiles:{path:$tmpfiles,digest:("sha256:"+("0"*64)),uid:0,mode:"0644"},state:{path:$state,uid:0,mode:"0700"},environment:{path:$env,backupPath:$envBackup,priorDigest:$envPrior,digest:("sha256:"+("0"*64))},ownership:{path:$ownership,backupPath:$ownershipBackup,priorDigest:$ownershipPrior},secret:{path:$secret,digest:("sha256:"+("0"*64)),encoding:"base64url-unpadded",decodedBytes:32},socket:{path:"/run/blazn/microk8s-worker-issuer.sock",uid:0,gid:$brokerGid,mode:"0660",brokerGroup:"blazn-node-broker"},microk8s:{version:"v1.35.6",revision:$revision,gid:$microGid,tokenFile:"/var/snap/microk8s/current/credentials/cluster-tokens.txt"},recovery:{root:$recovery,inventoryDigest:$inventoryDigest},brokerUid:$brokerUid,brokerGroupCreated:$groupCreated,liveJoinBlocked:false}' >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; ln -- "$tmp" "$RECEIPT" || { rm -f -- "$tmp"; die "receipt appeared concurrently"; }; rm -f -- "$tmp"; sync_path "$(dirname -- "$RECEIPT")"; fault initialized
fi
if [ ! -f "$RECEIPT" ] || [ -L "$RECEIPT" ] || [ "$(stat -c '%u:%a:%h' "$RECEIPT")" != 0:600:1 ]; then die "issuer receipt is unsafe"; fi
jq -e --arg host "$(hostname)" --argjson uid "$BROKER_UID" --argjson gid "$BROKER_GID" --argjson mgid "$MICROK8S_GID" '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .owner=="blazn-poc" and .host==$host and .brokerUid==$uid and .socket.gid==$gid and .microk8s.gid==$mgid' "$RECEIPT" >/dev/null || die "issuer receipt binding differs"
[ "$(jq -er .sourceDigest "$RECOVERY/inventory.json")" = "$SOURCE_DIGEST" ] || die "recovery inventory source digest differs from reviewed helper"

current=$(jq -er .phase "$RECEIPT")
case "$current" in initialized|secret-created|config-bound|files-installed|service-started|complete) ;; *) die "issuer receipt is not install-resumable" ;; esac
state_parent=$(dirname -- "$STATE_ROOT")
if [ ! -e "$state_parent" ]; then mkdir -p -- "$state_parent"; chmod 0700 "$state_parent"; fi
if [ ! -d "$state_parent" ] || [ -L "$state_parent" ] || [ "$(stat -c '%u:%a:%F' "$state_parent")" != "0:700:directory" ]; then die "issuer state parent is unsafe"; fi
mkdir -p -- "$ROOT" "$(dirname -- "$BINARY")" "$(dirname -- "$UNIT")" "$(dirname -- "$TMPFILES")"
chmod 0700 "$ROOT"
validate_managed_parent "$(dirname -- "$BINARY")"
validate_managed_parent "$(dirname -- "$UNIT")"
validate_managed_parent "$(dirname -- "$TMPFILES")"
if [ "$(stat -c '%u:%a' "$ROOT")" != 0:700 ] || [ "$(stat -c '%u:%a' "$RECOVERY")" != 0:700 ]; then die "issuer root or recovery directory is unsafe"; fi
[ "$(stat -c '%u:%a' "$(dirname -- "$RECEIPT")")" = 0:700 ] || die "ownership directory is unsafe"
if [ "$current" = initialized ]; then
  active_key=$ROOT/issuer-hmac-v1; keytmp=$ROOT/.issuer-key.pending
  if [ -e "$active_key" ]; then validate_key "$active_key"; else
    if [ -e "$keytmp" ] && { [ ! -f "$keytmp" ] || [ -L "$keytmp" ] || [ "$(stat -c %u "$keytmp")" != 0 ]; }; then die "issuer key pending path is unsafe"; fi
    hex=$(openssl rand -hex 32); printf '%s' "$hex" | xxd -r -p | openssl base64 -A | tr '+/' '-_' | tr -d '=' >"$keytmp"; unset hex
    chmod 0400 "$keytmp"; sync_path "$keytmp"; validate_key "$keytmp"; fault key-pending; mv -- "$keytmp" "$active_key"; sync_path "$ROOT"
  fi
  recovery_key=$RECOVERY/issuer-hmac-v1; recovery_tmp=$RECOVERY/.issuer-hmac.pending
  if [ -e "$recovery_key" ]; then validate_key "$recovery_key"; cmp "$active_key" "$recovery_key" >/dev/null || die "recovery key generation conflicts"; else
    if [ -e "$recovery_tmp" ] && { [ ! -f "$recovery_tmp" ] || [ -L "$recovery_tmp" ] || [ "$(stat -c %u "$recovery_tmp")" != 0 ]; }; then die "recovery key pending path is unsafe"; fi
    cp -- "$active_key" "$recovery_tmp"; chmod 0400 "$recovery_tmp"; sync_path "$recovery_tmp"; fault recovery-key-pending; mv -- "$recovery_tmp" "$recovery_key"; sync_path "$RECOVERY"
  fi
  tmp=$RECEIPT.tmp.$$; jq --arg digest "sha256:$(sha "$ROOT/issuer-hmac-v1")" '.secret.digest=$digest' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; phase secret-created; current=secret-created; fault secret-created
fi
if [ "$current" = secret-created ]; then
  tmp=$ROOT/config.json.tmp.$$
  jq -cn --argjson uid "$BROKER_UID" --argjson gid "$BROKER_GID" --argjson mgid "$MICROK8S_GID" '{schemaVersion:"blazn.dev/microk8s-worker-issuer-config/v1",brokerUid:$uid,brokerGid:$gid,microk8sGid:$mgid}' >"$tmp"
  chmod 0400 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$ROOT/config.json"; sync_path "$ROOT"
  bind_environment
  tmp=$RECEIPT.tmp.$$; jq --arg config "sha256:$(sha "$ROOT/config.json")" --arg env "sha256:$(sha "$ENV_FILE")" '.config.digest=$config|.environment.digest=$env' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; phase config-bound; current=config-bound; fault config-bound
fi
if [ "$current" = config-bound ]; then
  install_artifact "$SOURCE" "$BINARY" "$SOURCE_DIGEST" 0755 0755
  install_artifact "$UNIT_SOURCE" "$UNIT" "$UNIT_SOURCE_DIGEST" 0644 0644
  install_artifact "$TMPFILES_SOURCE" "$TMPFILES" "$TMPFILES_SOURCE_DIGEST" 0644 0644
  tmp=$RECEIPT.tmp.$$; jq --arg binary "sha256:$(sha "$BINARY")" --arg unit "sha256:$(sha "$UNIT")" --arg tmpfiles "sha256:$(sha "$TMPFILES")" '.binary.digest=$binary|.unit.digest=$unit|.tmpfiles.digest=$tmpfiles' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; phase files-installed; current=files-installed; fault files-installed
fi
if [ "$current" = files-installed ]; then
  "$TMPFILES_CMD" --create "$TMPFILES"
  "$SYSTEMCTL" daemon-reload
  "$SYSTEMCTL" enable --now blazn-microk8s-worker-issuer.service
  if [ "$TEST_MODE" != 1 ]; then
    "$SYSTEMCTL" is-active --quiet blazn-microk8s-worker-issuer.service || die "issuer service is not active"
    [ "$(stat -c '%u:%a:%F' "$state_parent")" = "0:700:directory" ] || die "issuer state parent is unsafe"
    [ "$(stat -c '%u:%a:%F' "$STATE_ROOT")" = "0:700:directory" ] || die "issuer state root differs from receipt"
    [ "$(stat -c '%u:%g:%a:%F' /run/blazn)" = "0:$BROKER_GID:750:directory" ] || die "issuer socket parent differs from receipt"
    if [ ! -S /run/blazn/microk8s-worker-issuer.sock ] || [ -L /run/blazn/microk8s-worker-issuer.sock ]; then die "issuer socket is unavailable or linked"; fi
    [ "$(stat -c '%u:%g:%a:%h' /run/blazn/microk8s-worker-issuer.sock)" = "0:$BROKER_GID:660:1" ] || die "issuer socket metadata differs from receipt"
    [ "$(stat -c '%u:%g:%a:%F' /var/snap/microk8s/current/credentials)" = "0:$MICROK8S_GID:770:directory" ] || die "MicroK8s credential directory is unsafe"
    token_file=/var/snap/microk8s/current/credentials/cluster-tokens.txt
    if [ -e "$token_file" ] && { [ ! -f "$token_file" ] || [ -L "$token_file" ] || [ "$(stat -c '%u:%g:%a:%h' "$token_file")" != "0:$MICROK8S_GID:660:1" ]; }; then die "MicroK8s token file is unsafe"; fi
  fi
  phase service-started; current=service-started; fault service-started
fi
if [ "$current" = service-started ]; then phase complete; current=complete; fault complete; fi
[ "$current" = complete ] || die "issuer installation did not complete"
if [ "$(stat -c '%u:%a:%h' "$BINARY")" != 0:755:1 ] || [ "$(stat -c '%u:%a:%h' "$ROOT/config.json")" != 0:400:1 ] || [ "$(stat -c '%u:%a:%h' "$UNIT")" != 0:644:1 ] || [ "$(stat -c '%u:%a:%h' "$TMPFILES")" != 0:644:1 ]; then die "issuer artifact ownership, mode, or link count is unsafe"; fi
validate_managed_parent "$(dirname -- "$BINARY")"
validate_managed_parent "$(dirname -- "$UNIT")"
validate_managed_parent "$(dirname -- "$TMPFILES")"
if [ "$TEST_MODE" != 1 ]; then [ "$(stat -c '%u:%a:%F' "$state_parent")" = "0:700:directory" ] || die "issuer state parent is unsafe"; [ "$(stat -c '%u:%a:%F' "$STATE_ROOT")" = "0:700:directory" ] || die "issuer state root differs from receipt"; fi
bind_main_receipt
[ "sha256:$(sha "$BINARY")" = "$SOURCE_DIGEST" ] || die "installed helper differs from reviewed source digest"
[ "sha256:$(sha "$BINARY")" = "$(jq -er .binary.digest "$RECEIPT")" ] || die "helper binary differs from receipt"
[ "sha256:$(sha "$ROOT/config.json")" = "$(jq -er .config.digest "$RECEIPT")" ] || die "helper config differs from receipt"
[ "sha256:$(sha "$ROOT/issuer-hmac-v1")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "helper secret differs from receipt"
[ "sha256:$(sha "$UNIT")" = "$(jq -er .unit.digest "$RECEIPT")" ] || die "systemd unit differs from receipt"
[ "sha256:$(sha "$TMPFILES")" = "$(jq -er .tmpfiles.digest "$RECEIPT")" ] || die "tmpfiles policy differs from receipt"
[ "sha256:$(sha "$ENV_FILE")" = "$(jq -er .environment.digest "$RECEIPT")" ] || die "control-plane environment differs from receipt"
[ "sha256:$(sha "$RECOVERY/inventory.json")" = "$(jq -er .recovery.inventoryDigest "$RECEIPT")" ] || die "recovery inventory differs from receipt"
[ "sha256:$(sha "$RECOVERY/control-plane.env")" = "$(jq -er .environment.priorDigest "$RECEIPT")" ] || die "environment recovery copy differs from receipt"
[ "sha256:$(sha "$RECOVERY/control-plane.json")" = "$(jq -er .ownership.priorDigest "$RECEIPT")" ] || die "ownership recovery copy differs from receipt"
validate_key "$ROOT/issuer-hmac-v1"
recovery_key=$(jq -er .secretRecoveryPath "$RECOVERY/inventory.json"); [ "$recovery_key" = "$RECOVERY/issuer-hmac-v1" ] || die "recovery key path differs from inventory"; validate_key "$recovery_key"; cmp "$ROOT/issuer-hmac-v1" "$recovery_key" >/dev/null || die "recovery key differs from active generation"
printf 'MicroK8s worker issuer infrastructure is receipt-bound; join consumption requires authoritative quarantined-worker observation\n'
