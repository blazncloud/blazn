#!/bin/sh
set -eu

die() { printf 'blazn-node-infra: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "secret provisioning must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "secret provisioning must run through the control-plane lock"
for command_name in dirname jq openssl sha256sum sync wc; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done

secrets=${BLAZN_NODE_BROKER_SECRETS_ROOT:-/etc/blazn/node-broker/secrets}
target=$(dirname -- "$secrets")
journal=${BLAZN_NODE_BROKER_CREATE_JOURNAL:-/var/lib/blazn/ownership/node-broker-secret-create.json}
if [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" != 1 ]; then
  [ "$secrets" = /etc/blazn/node-broker/secrets ] || die "node broker secrets root is outside the reviewed path"
  [ "$journal" = /var/lib/blazn/ownership/node-broker-secret-create.json ] || die "node broker create journal is outside the reviewed path"
fi
case "$target:$journal" in /*:/*) ;; *) die "secret and journal paths must be absolute" ;; esac

assert_no_links() { candidate=$1; while [ "$candidate" != / ]; do [ ! -L "$candidate" ] || die "path contains a symbolic link: $candidate"; candidate=$(dirname -- "$candidate"); done; }
assert_no_links "$target"
assert_no_links "$journal"
sync_path() { sync -f "$1"; }
write_journal_phase() {
  next=$1; tmp=$journal.tmp.$$
  jq --arg phase "$next" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase | .updatedAt=$updatedAt' "$journal" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$journal"; sync_path "$(dirname -- "$journal")"
}
test_fault() { [ "${BLAZN_NODE_INFRA_TEST_MODE:-0}" = 1 ] || return 0; [ "${BLAZN_NODE_SECRET_TEST_FAIL_AFTER:-}" != "$1" ] || die "injected secret-create fault after $1"; }
validate_url() {
  file=$1
  if [ ! -f "$file" ] || [ -L "$file" ] || [ "$(stat -c '%u:%a' "$file")" != 0:444 ]; then die "database URL has unsafe ownership or mode"; fi
  value=$(sed -n '1p' "$file")
  case "$value" in postgresql://blazn_node_broker:????????????????????????????????????????????????????????????????@postgres:5432/blazn) ;; *) die "database URL has an invalid fixed form" ;; esac
  password=${value#*://*:}; password=${password%%@*}; case "$password" in *[!a-f0-9]*) die "database password is not lowercase hexadecimal" ;; esac
}
validate_key() { file=$1; if [ ! -f "$file" ] || [ -L "$file" ] || [ "$(stat -c '%u:%a' "$file")" != 0:400 ]; then die "Node broker key has unsafe ownership or mode"; fi; [ "$(wc -c <"$file" | tr -d ' ')" = 32 ] || die "Node broker key must be exactly 32 bytes"; }
validate_tree() {
  root=$1
  if [ ! -d "$root" ] || [ -L "$root" ] || [ "$(stat -c '%u:%a' "$root")" != 0:700 ]; then die "Node broker root has unsafe ownership or mode"; fi
  if [ ! -d "$root/secrets" ] || [ -L "$root/secrets" ] || [ "$(stat -c '%u:%a' "$root/secrets")" != 0:700 ]; then die "Node broker secrets directory has unsafe ownership or mode"; fi
  validate_url "$root/secrets/database-url"; validate_key "$root/secrets/enrollment-hmac-v1"; validate_key "$root/secrets/join-credential-v1"
  actual=$(find "$root" -mindepth 1 -maxdepth 2 -printf '%P\n' | LC_ALL=C sort)
  expected='secrets
secrets/database-url
secrets/enrollment-hmac-v1
secrets/join-credential-v1'
  [ "$actual" = "$expected" ] || die "Node broker secret tree contains an unreviewed entry"
}
validate_partial_tree() {
  root=$1
  if [ ! -d "$root/secrets" ] || [ -L "$root/secrets" ]; then die "secret staging directory is unavailable or symlinked"; fi
  find "$root" -mindepth 1 -maxdepth 1 -print | while IFS= read -r entry; do [ "$entry" = "$root/secrets" ] || die "secret staging root contains an unreviewed entry"; done
  find "$root/secrets" -mindepth 1 -maxdepth 1 -print | while IFS= read -r entry; do
    name=$(basename -- "$entry")
    case "$name" in database-url|enrollment-hmac-v1|join-credential-v1) ;;
      database-url.tmp.*|enrollment-hmac-v1.tmp.*|join-credential-v1.tmp.*) suffix=${name##*.tmp.}; case "$suffix" in ''|*[!0-9]*) die "secret staging temp has an invalid suffix" ;; esac ;;
      *) die "secret staging directory contains an unreviewed entry: $name" ;;
    esac
    if [ ! -f "$entry" ] || [ -L "$entry" ] || [ "$(stat -c %u "$entry")" != 0 ]; then die "secret staging entry has unsafe type or owner: $name"; fi
  done
}
reconcile_temps() {
  destination=$1
  base=$(basename -- "$destination")
  parent=$(dirname -- "$destination")
  find "$parent" -mindepth 1 -maxdepth 1 -name "$base.tmp.*" -print | while IFS= read -r orphan; do
    orphan_name=$(basename -- "$orphan")
    case "$orphan_name" in "$base".tmp.*) suffix=${orphan_name#"$base.tmp."}; case "$suffix" in ''|*[!0-9]*) die "refusing an unrecognized secret temp path" ;; esac ;; *) die "refusing an unrecognized secret temp path" ;; esac
    if [ ! -f "$orphan" ] || [ -L "$orphan" ] || [ "$(stat -c %u "$orphan")" != 0 ]; then die "refusing unsafe secret temp residue"; fi
    orphan_mode=$(stat -c %a "$orphan")
    case "$orphan_mode" in 600|400|444) ;; *) die "refusing secret temp residue with unsafe mode" ;; esac
    rm -f -- "$orphan"
  done
  sync_path "$parent"
}
publish_file() {
  destination=$1; mode=$2; kind=$3; label=$4
  reconcile_temps "$destination"
  if [ -e "$destination" ]; then case "$kind" in url) validate_url "$destination" ;; key) validate_key "$destination" ;; esac; return; fi
  tmp=$destination.tmp.$$
  : >"$tmp"; chmod 0600 "$tmp"; test_fault "$label-temp-created"
  case "$kind" in
    url) password=$(openssl rand -hex 32); printf 'postgresql://blazn_node_broker:%s@postgres:5432/blazn\n' "$password" >"$tmp" ;;
    key) openssl rand 32 >"$tmp" ;;
    *) die "unknown secret kind" ;;
  esac
  test_fault "$label-temp-written"
  chmod "$mode" "$tmp"; test_fault "$label-temp-chmod"
  sync_path "$tmp"; test_fault "$label-temp-fsynced"
  test_fault "$label-before-mv"
  mv -- "$tmp" "$destination"; test_fault "$label-after-mv"
  sync_path "$(dirname -- "$destination")"
  case "$kind" in url) validate_url "$destination" ;; key) validate_key "$destination" ;; esac
}

if [ ! -e "$journal" ]; then
  [ ! -e "$target" ] || die "node broker secret boundary exists without its creation journal"
  mkdir -p -- "$(dirname -- "$journal")" "$(dirname -- "$target")"
  stage=$(dirname -- "$target")/.node-broker-create-$(openssl rand -hex 12)
  tmp=$journal.tmp.$$; umask 077
  jq -cn --arg target "$target" --arg stage "$stage" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '{schemaVersion:"blazn.dev/node-broker-secret-create/v1",owner:"blazn-poc",phase:"initialized",target:$target,stage:$stage,createdAt:$createdAt}' >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; ln -- "$tmp" "$journal" || { rm -f -- "$tmp"; die "secret creation journal target appeared"; }; rm -f -- "$tmp"; sync_path "$(dirname -- "$journal")"; test_fault initialized
fi

if [ ! -f "$journal" ] || [ -L "$journal" ] || [ "$(stat -c '%u:%a' "$journal")" != 0:600 ]; then die "secret creation journal has unsafe ownership or mode"; fi
jq -e --arg target "$target" '.schemaVersion=="blazn.dev/node-broker-secret-create/v1" and .owner=="blazn-poc" and .target==$target and (.stage|type=="string")' "$journal" >/dev/null || die "secret creation journal is invalid"
stage=$(jq -er .stage "$journal")
case "$stage" in "$(dirname -- "$target")"/.node-broker-create-*) ;; *) die "secret creation staging path is outside the reviewed parent" ;; esac
assert_no_links "$stage"
phase=$(jq -er .phase "$journal")
case "$phase" in initialized|tree-created|database-written|hmac-written|join-written|published) ;; *) die "secret creation journal phase is invalid" ;; esac
if [ "$phase" != published ] && [ -d "$stage" ]; then validate_partial_tree "$stage"; fi

if [ "$phase" = initialized ]; then
  if [ ! -e "$stage" ]; then mkdir -p -- "$stage/secrets"; chmod 0700 "$stage" "$stage/secrets"; sync_path "$(dirname -- "$stage")"; fi
  if [ ! -d "$stage/secrets" ] || [ "$(stat -c '%u:%a' "$stage")" != 0:700 ] || [ "$(stat -c '%u:%a' "$stage/secrets")" != 0:700 ]; then die "secret staging tree is invalid"; fi
  write_journal_phase tree-created; phase=tree-created; test_fault tree-created
fi
if [ "$phase" = tree-created ]; then publish_file "$stage/secrets/database-url" 0444 url database; write_journal_phase database-written; phase=database-written; test_fault database-written; fi
if [ "$phase" = database-written ]; then publish_file "$stage/secrets/enrollment-hmac-v1" 0400 key hmac; write_journal_phase hmac-written; phase=hmac-written; test_fault hmac-written; fi
if [ "$phase" = hmac-written ]; then publish_file "$stage/secrets/join-credential-v1" 0400 key join; write_journal_phase join-written; phase=join-written; test_fault join-written; fi
if [ "$phase" = join-written ]; then
  validate_tree "$stage"
  if [ -d "$stage" ] && [ ! -e "$target" ]; then mv -- "$stage" "$target"; sync_path "$(dirname -- "$target")"; elif [ -d "$target" ] && [ ! -e "$stage" ]; then :; else die "secret staging publication is ambiguous"; fi
  validate_tree "$target"; write_journal_phase published; phase=published; test_fault published
fi
[ "$phase" = published ] || die "secret creation did not reach published state"
validate_tree "$target"
printf 'created or verified root-owned Node broker secrets under %s\n' "$secrets"
