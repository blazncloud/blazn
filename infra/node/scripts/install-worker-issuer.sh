#!/bin/sh
set -eu
umask 077
export LC_ALL=C

die(){ printf 'blazn-worker-issuer-infra: %s\n' "$*" >&2; exit 1; }
need(){ command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
[ "$(id -u)" -eq 0 ] || die "installation requires root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "installation requires the control-plane lock"
for command_name in awk cmp cp dirname find getent grep install jq mv openssl rm sha256sum stat sync wc; do need "$command_name"; done

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SOURCE=${BLAZN_ISSUER_BINARY_SOURCE:?set BLAZN_ISSUER_BINARY_SOURCE to the reviewed helper binary}
ROOT=${BLAZN_ISSUER_CONFIG_ROOT:-/etc/blazn/microk8s-worker-issuer}
BINARY=${BLAZN_ISSUER_BINARY_PATH:-/usr/libexec/blazn/blazn-microk8s-worker-issuer}
UNIT=${BLAZN_ISSUER_UNIT_PATH:-/etc/systemd/system/blazn-microk8s-worker-issuer.service}
TMPFILES=${BLAZN_ISSUER_TMPFILES_PATH:-/etc/tmpfiles.d/blazn-microk8s-worker-issuer.conf}
RECEIPT=${BLAZN_ISSUER_RECEIPT_PATH:-/var/lib/blazn/ownership/microk8s-worker-issuer.json}
RECOVERY=${BLAZN_ISSUER_RECOVERY_ROOT:-/var/lib/blazn/ownership/microk8s-worker-issuer-recovery}
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
BROKER_UID=${BLAZN_NODE_BROKER_UID:?set the receipt-bound node broker UID}
TEST_MODE=${BLAZN_ISSUER_INFRA_TEST_MODE:-0}
case "$BROKER_UID" in ''|*[!0-9]*|0) die "broker UID must be a positive integer" ;; esac
for path in "$SOURCE" "$ROOT" "$BINARY" "$UNIT" "$TMPFILES" "$RECEIPT" "$RECOVERY" "$ENV_FILE"; do case "$path" in /*) ;; *) die "all issuer paths must be absolute" ;; esac; done
[ -f "$SOURCE" ] && [ ! -L "$SOURCE" ] || die "helper binary source is unavailable or linked"
[ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] && [ "$(stat -c '%u:%a' "$ENV_FILE")" = 0:600 ] || die "control-plane environment is unsafe"

sha(){ sha256sum "$1" | awk '{print $1}'; }
sync_path(){ sync -f "$1"; }
validate_key(){
  key=$1; [ -f "$key" ] && [ ! -L "$key" ] && [ "$(stat -c '%u:%a' "$key")" = 0:400 ] || die "issuer key is unsafe"
  if [ "$(wc -c <"$key" | tr -d ' ')" != 43 ] || ! LC_ALL=C grep -Eq '^[A-Za-z0-9_-]{43}$' "$key"; then die "issuer key encoding is invalid"; fi
  decoded=$ROOT/.decoded-key.$$
  { tr '_-' '/+' <"$key"; printf '='; } | openssl base64 -d -A >"$decoded" 2>/dev/null || { rm -f -- "$decoded"; die "issuer key cannot be decoded"; }
  [ "$(wc -c <"$decoded" | tr -d ' ')" = 32 ] || { rm -f -- "$decoded"; die "issuer key entropy length is invalid"; }
  rm -f -- "$decoded"
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
phase(){ value=$1; tmp=$RECEIPT.tmp.$$; jq --arg phase "$value" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; sync_path "$(dirname -- "$RECEIPT")"; }
fault(){ [ "$TEST_MODE" = 1 ] || return 0; [ "${BLAZN_ISSUER_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected issuer fault after $1"; }
safe_parent(){ candidate=$1; while [ "$candidate" != / ]; do [ ! -L "$candidate" ] || die "issuer path contains a symlink: $candidate"; candidate=$(dirname -- "$candidate"); done; }
for path in "$ROOT" "$BINARY" "$UNIT" "$TMPFILES" "$RECEIPT" "$RECOVERY" "$ENV_FILE"; do safe_parent "$path"; done

if [ "$TEST_MODE" = 1 ]; then
  BROKER_GID=${BLAZN_ISSUER_TEST_BROKER_GID:?}
  MICROK8S_GID=${BLAZN_ISSUER_TEST_MICROK8S_GID:?}
  MICROK8S_REVISION=${BLAZN_ISSUER_TEST_REVISION:-9072}
  SYSTEMCTL=${BLAZN_ISSUER_TEST_SYSTEMCTL:?}
  TMPFILES_CMD=${BLAZN_ISSUER_TEST_TMPFILES:?}
  group_created=false
else
  getent group blazn-node-broker >/dev/null || die "dedicated blazn-node-broker group must be provisioned before this transaction"
  group_created=false
  BROKER_GID=$(getent group blazn-node-broker | awk -F: '{print $3}')
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
if [ ! -e "$RECEIPT" ]; then
  for managed in "$ROOT" "$BINARY" "$UNIT" "$TMPFILES"; do [ ! -e "$managed" ] || die "managed issuer path exists without its receipt: $managed"; done
  mkdir -p -- "$RECOVERY"
  chmod 0700 "$RECOVERY"
  inventory=$RECOVERY/inventory.json
  if find "$RECOVERY" -mindepth 1 -maxdepth 1 ! -name inventory.json ! -name control-plane.env -print | grep . >/dev/null; then die "pre-receipt recovery root contains unreviewed residue"; fi
  if [ -e "$RECOVERY/control-plane.env" ]; then cmp "$ENV_FILE" "$RECOVERY/control-plane.env" >/dev/null || die "pre-receipt environment backup differs"; else cp -- "$ENV_FILE" "$RECOVERY/control-plane.env"; chmod 0600 "$RECOVERY/control-plane.env"; sync_path "$RECOVERY/control-plane.env"; fi
  inventory_tmp=$inventory.tmp.$$
  jq -cn --arg source "sha256:$(sha "$SOURCE")" --arg key "$RECOVERY/issuer-hmac-v1" --arg env "$RECOVERY/control-plane.env" --arg envDigest "sha256:$(sha "$ENV_FILE")" '{schemaVersion:"blazn.dev/microk8s-worker-issuer-recovery/v1",prior:{config:"absent",binary:"absent",unit:"absent",tmpfiles:"absent",environment:{backupPath:$env,digest:$envDigest}},sourceDigest:$source,secretRecoveryPath:$key}' >"$inventory_tmp"
  chmod 0600 "$inventory_tmp"; sync_path "$inventory_tmp"; mv -- "$inventory_tmp" "$inventory"
  chmod 0600 "$inventory"; sync_path "$inventory"
  fault recovery-created
  tmp=$RECEIPT.tmp.$$
  jq -cn --arg host "$(hostname)" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg binary "$BINARY" --arg config "$ROOT/config.json" --arg secret "$ROOT/issuer-hmac-v1" --argjson brokerUid "$BROKER_UID" --argjson brokerGid "$BROKER_GID" --argjson microGid "$MICROK8S_GID" --argjson revision "$MICROK8S_REVISION" --arg recovery "$RECOVERY" --arg inventoryDigest "sha256:$(sha "$inventory")" --argjson groupCreated "$group_created" \
    --arg unit "$UNIT" --arg tmpfiles "$TMPFILES" --arg env "$ENV_FILE" --arg envBackup "$RECOVERY/control-plane.env" --arg envPrior "sha256:$(sha "$ENV_FILE")" '{schemaVersion:"blazn.dev/microk8s-worker-issuer-infra/v1",owner:"blazn-poc",host:$host,phase:"initialized",createdAt:$createdAt,binary:{path:$binary,digest:("sha256:"+("0"*64)),uid:0,mode:"0755"},config:{path:$config,digest:("sha256:"+("0"*64)),uid:0,mode:"0400"},unit:{path:$unit,digest:("sha256:"+("0"*64)),uid:0,mode:"0644"},tmpfiles:{path:$tmpfiles,digest:("sha256:"+("0"*64)),uid:0,mode:"0644"},environment:{path:$env,backupPath:$envBackup,priorDigest:$envPrior,digest:("sha256:"+("0"*64))},secret:{path:$secret,digest:("sha256:"+("0"*64)),encoding:"base64url-unpadded",decodedBytes:32},socket:{path:"/run/blazn/microk8s-worker-issuer.sock",uid:0,gid:$brokerGid,mode:"0660",brokerGroup:"blazn-node-broker"},microk8s:{version:"v1.35.6",revision:$revision,gid:$microGid,tokenFile:"/var/snap/microk8s/current/credentials/cluster-tokens.txt"},recovery:{root:$recovery,inventoryDigest:$inventoryDigest},brokerUid:$brokerUid,brokerGroupCreated:$groupCreated,liveJoinBlocked:true}' >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; ln -- "$tmp" "$RECEIPT" || { rm -f -- "$tmp"; die "receipt appeared concurrently"; }; rm -f -- "$tmp"; sync_path "$(dirname -- "$RECEIPT")"; fault initialized
fi
[ -f "$RECEIPT" ] && [ ! -L "$RECEIPT" ] && [ "$(stat -c '%u:%a' "$RECEIPT")" = 0:600 ] || die "issuer receipt is unsafe"
jq -e --arg host "$(hostname)" --argjson uid "$BROKER_UID" --argjson gid "$BROKER_GID" --argjson mgid "$MICROK8S_GID" '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .owner=="blazn-poc" and .host==$host and .brokerUid==$uid and .socket.gid==$gid and .microk8s.gid==$mgid' "$RECEIPT" >/dev/null || die "issuer receipt binding differs"

current=$(jq -er .phase "$RECEIPT")
case "$current" in initialized|secret-created|config-bound|files-installed|service-started|complete) ;; *) die "issuer receipt is not install-resumable" ;; esac
mkdir -p -- "$ROOT" "$(dirname -- "$BINARY")" "$(dirname -- "$UNIT")" "$(dirname -- "$TMPFILES")"
chmod 0700 "$ROOT"
if [ "$current" = initialized ]; then
  raw=$ROOT/.issuer-raw.$$
  keytmp=$ROOT/.issuer-key.$$
  openssl rand -out "$raw" 32; chmod 0600 "$raw"; sync_path "$raw"
  openssl base64 -A -in "$raw" | tr '+/' '-_' | tr -d '=' >"$keytmp"
  chmod 0400 "$keytmp"; sync_path "$keytmp"; rm -f -- "$raw"
  [ "$(wc -c <"$keytmp" | tr -d ' ')" = 43 ] || die "issuer key encoding is invalid"
  mv -- "$keytmp" "$ROOT/issuer-hmac-v1"; sync_path "$ROOT"
  validate_key "$ROOT/issuer-hmac-v1"
  cp -- "$ROOT/issuer-hmac-v1" "$RECOVERY/issuer-hmac-v1"; chmod 0400 "$RECOVERY/issuer-hmac-v1"; sync_path "$RECOVERY/issuer-hmac-v1"
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
  install -o root -g root -m 0755 "$SOURCE" "$BINARY"
  install -o root -g root -m 0644 "$SCRIPT_DIR/../systemd/blazn-microk8s-worker-issuer.service" "$UNIT"
  install -o root -g root -m 0644 "$SCRIPT_DIR/../systemd/blazn-microk8s-worker-issuer.tmpfiles" "$TMPFILES"
  sync_path "$BINARY"; sync_path "$UNIT"; sync_path "$TMPFILES"
  tmp=$RECEIPT.tmp.$$; jq --arg binary "sha256:$(sha "$BINARY")" --arg unit "sha256:$(sha "$UNIT")" --arg tmpfiles "sha256:$(sha "$TMPFILES")" '.binary.digest=$binary|.unit.digest=$unit|.tmpfiles.digest=$tmpfiles' "$RECEIPT" >"$tmp"; chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$RECEIPT"; phase files-installed; current=files-installed; fault files-installed
fi
if [ "$current" = files-installed ]; then
  "$TMPFILES_CMD" --create "$TMPFILES"
  "$SYSTEMCTL" daemon-reload
  "$SYSTEMCTL" enable --now blazn-microk8s-worker-issuer.service
  if [ "$TEST_MODE" != 1 ]; then
    "$SYSTEMCTL" is-active --quiet blazn-microk8s-worker-issuer.service || die "issuer service is not active"
    [ "$(stat -c '%u:%g:%a:%F' /run/blazn)" = "0:$BROKER_GID:750:directory" ] || die "issuer socket parent differs from receipt"
    [ -S /run/blazn/microk8s-worker-issuer.sock ] && [ ! -L /run/blazn/microk8s-worker-issuer.sock ] || die "issuer socket is unavailable or linked"
    [ "$(stat -c '%u:%g:%a:%h' /run/blazn/microk8s-worker-issuer.sock)" = "0:$BROKER_GID:660:1" ] || die "issuer socket metadata differs from receipt"
    [ "$(stat -c '%u:%g:%a:%F' /var/snap/microk8s/current/credentials)" = "0:$MICROK8S_GID:770:directory" ] || die "MicroK8s credential directory is unsafe"
    token_file=/var/snap/microk8s/current/credentials/cluster-tokens.txt
    if [ -e "$token_file" ]; then [ -f "$token_file" ] && [ ! -L "$token_file" ] && [ "$(stat -c '%u:%g:%a:%h' "$token_file")" = "0:$MICROK8S_GID:660:1" ] || die "MicroK8s token file is unsafe"; fi
  fi
  phase service-started; current=service-started; fault service-started
fi
if [ "$current" = service-started ]; then phase complete; current=complete; fault complete; fi
[ "$current" = complete ] || die "issuer installation did not complete"
[ "sha256:$(sha "$BINARY")" = "$(jq -er .binary.digest "$RECEIPT")" ] || die "helper binary differs from receipt"
[ "sha256:$(sha "$ROOT/config.json")" = "$(jq -er .config.digest "$RECEIPT")" ] || die "helper config differs from receipt"
[ "sha256:$(sha "$ROOT/issuer-hmac-v1")" = "$(jq -er .secret.digest "$RECEIPT")" ] || die "helper secret differs from receipt"
[ "sha256:$(sha "$UNIT")" = "$(jq -er .unit.digest "$RECEIPT")" ] || die "systemd unit differs from receipt"
[ "sha256:$(sha "$TMPFILES")" = "$(jq -er .tmpfiles.digest "$RECEIPT")" ] || die "tmpfiles policy differs from receipt"
[ "sha256:$(sha "$ENV_FILE")" = "$(jq -er .environment.digest "$RECEIPT")" ] || die "control-plane environment differs from receipt"
validate_key "$ROOT/issuer-hmac-v1"
recovery_key=$(jq -er .secretRecoveryPath "$RECOVERY/inventory.json"); [ "$recovery_key" = "$RECOVERY/issuer-hmac-v1" ] || die "recovery key path differs from inventory"; validate_key "$recovery_key"; cmp "$ROOT/issuer-hmac-v1" "$recovery_key" >/dev/null || die "recovery key differs from active generation"
printf 'MicroK8s worker issuer infrastructure is receipt-bound; live join remains blocked\n'
