#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SOURCE_TEMPLATE=${BLAZN_NODE_PLAN_TEMPLATE_SOURCE:-$SCRIPT_DIR/../templates/node-install-plan-template-v1.json}
ROOT=${BLAZN_NODE_PLAN_ROOT:-/etc/blazn/node-plan}
JOURNAL=${BLAZN_NODE_PLAN_CREATE_JOURNAL:-/var/lib/blazn/ownership/node-plan-material-create.json}

die() { printf 'blazn-node-plan: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "plan material provisioning must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "plan material provisioning must run through the control-plane lock"
for command_name in jq openssl sha256sum sync wc; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
if [ "${BLAZN_NODE_PLAN_TEST_MODE:-0}" != 1 ]; then
  [ "$ROOT" = /etc/blazn/node-plan ] || die "plan material root is outside the reviewed path"
  case "$JOURNAL" in /var/lib/blazn/ownership/node-plan-material-create.json|/var/lib/blazn/ownership/node-plan-material-upgrade-create.json) ;; *) die "plan material journal is outside the reviewed path" ;; esac
fi
case "$ROOT:$JOURNAL:$SOURCE_TEMPLATE" in /*:/*:/*) ;; *) die "plan material paths must be absolute" ;; esac
if [ ! -f "$SOURCE_TEMPLATE" ] || [ -L "$SOURCE_TEMPLATE" ]; then die "source template must be a regular non-symlink file"; fi

no_links() { candidate=$1; while [ "$candidate" != / ]; do [ ! -L "$candidate" ] || die "path contains a symbolic link: $candidate"; candidate=$(dirname -- "$candidate"); done; }
no_links "$ROOT"; no_links "$JOURNAL"; no_links "$SOURCE_TEMPLATE"
sync_path() { sync -f "$1"; }
fault() { [ "${BLAZN_NODE_PLAN_TEST_MODE:-0}" = 1 ] || return 0; [ "${BLAZN_NODE_PLAN_FAIL_AFTER:-}" != "$1" ] || die "injected plan-material fault after $1"; }
write_phase() {
  next=$1
  tmp=$JOURNAL.tmp.$$
  jq --arg phase "$next" --arg updatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.phase=$phase|.updatedAt=$updatedAt' "$JOURNAL" >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$JOURNAL"; sync_path "$(dirname -- "$JOURNAL")"
}
validate_b64() {
  file=$1; mode=$2
  if [ ! -f "$file" ] || [ -L "$file" ] || [ "$(stat -c '%u:%a' "$file")" != "0:$mode" ]; then die "key material has unsafe ownership or mode"; fi
  [ "$(wc -c <"$file" | tr -d ' ')" = 44 ] || die "key material must contain 43 characters and one newline"
  [ "$(wc -l <"$file" | tr -d ' ')" = 1 ] || die "key material must contain one line"
  LC_ALL=C grep -Eq '^[A-Za-z0-9_-]{43}$' "$file" || die "key material is not unpadded base64url"
}
validate_tree() {
  base=$1
  if [ ! -d "$base" ] || [ -L "$base" ] || [ "$(stat -c '%u:%a' "$base")" != 0:700 ]; then die "plan material root is unsafe"; fi
  validate_b64 "$base/signing-private-v1.b64url" 444
  validate_b64 "$base/signing-public-v1.b64url" 444
  for file in signing-public-v1.json node-install-plan-template-v1.json; do
    if [ ! -f "$base/$file" ] || [ -L "$base/$file" ] || [ "$(stat -c '%u:%a' "$base/$file")" != 0:444 ]; then die "plan material file is unsafe: $file"; fi
  done
  jq -e '.schemaVersion=="blazn.dev/node-plan-signing-key/v1" and .keyId=="control-plane-node-plan/v1" and (.publicKey|test("^[A-Za-z0-9_-]{43}$")) and (.publicKeyFingerprint|test("^sha256:[a-f0-9]{64}$"))' "$base/signing-public-v1.json" >/dev/null || die "public signing metadata is invalid"
  [ "$(sed -n '1p' "$base/signing-public-v1.b64url")" = "$(jq -er .publicKey "$base/signing-public-v1.json")" ] || die "public signing metadata does not match public key"
  jq -e '.schemaVersion=="blazn.dev/node-install-plan-templates/v1" and .templateId=="frontro-poc-worker/v1" and (.profiles|keys)==["existing-linux-worker-adopt/v1","macos-lima-worker-adopt/v1","ubuntu-26.04-amd64-worker/v1"]' "$base/node-install-plan-template-v1.json" >/dev/null || die "installed plan template is invalid"
  actual=$(find "$base" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort)
  expected='node-install-plan-template-v1.json
signing-private-v1.b64url
signing-public-v1.b64url
signing-public-v1.json'
  [ "$actual" = "$expected" ] || die "plan material tree contains an unreviewed entry"
}

if [ ! -e "$JOURNAL" ]; then
  [ ! -e "$ROOT" ] || die "plan material root exists without its creation journal"
  mkdir -p -- "$(dirname -- "$JOURNAL")" "$(dirname -- "$ROOT")"
  stage=$(dirname -- "$ROOT")/.node-plan-create-$(openssl rand -hex 12)
  tmp=$JOURNAL.tmp.$$; umask 077
  jq -cn --arg target "$ROOT" --arg stage "$stage" --arg source "$SOURCE_TEMPLATE" --arg sourceDigest "sha256:$(sha256sum "$SOURCE_TEMPLATE"|awk '{print $1}')" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '{schemaVersion:"blazn.dev/node-plan-material-create/v1",owner:"blazn-poc",phase:"initialized",target:$target,stage:$stage,source:$source,sourceDigest:$sourceDigest,createdAt:$createdAt}' >"$tmp"
  chmod 0600 "$tmp"; sync_path "$tmp"; ln -- "$tmp" "$JOURNAL" || { rm -f -- "$tmp"; die "plan material journal target appeared"; }; rm -f -- "$tmp"; sync_path "$(dirname -- "$JOURNAL")"; fault initialized
fi
if [ ! -f "$JOURNAL" ] || [ -L "$JOURNAL" ] || [ "$(stat -c '%u:%a' "$JOURNAL")" != 0:600 ]; then die "plan material journal is unsafe"; fi
# Immutable releases intentionally relocate the reviewed template on every
# promotion. Recovery is bound to its recorded bytes, not to the retired
# release's absolute path.
jq -er .source "$JOURNAL" >/dev/null
[ "$(jq -er .sourceDigest "$JOURNAL")" = "sha256:$(sha256sum "$SOURCE_TEMPLATE"|awk '{print $1}')" ] || die "plan template source digest changed during recovery"
stage=$(jq -er .stage "$JOURNAL"); case "$stage" in "$(dirname -- "$ROOT")"/.node-plan-create-*) ;; *) die "plan material staging path escaped" ;; esac
phase=$(jq -er .phase "$JOURNAL"); case "$phase" in initialized|tree-created|key-written|metadata-written|template-written|published) ;; *) die "plan material phase is invalid" ;; esac

if [ "$phase" = initialized ]; then
  [ ! -e "$stage" ] || die "unexpected plan material staging tree"
  mkdir -- "$stage"; chmod 0700 "$stage"; sync_path "$(dirname -- "$stage")"
  write_phase tree-created; phase=tree-created; fault tree-created
fi
if [ "$phase" = tree-created ]; then
  pem=$stage/.ed25519.pem; private=$stage/signing-private-v1.b64url; public=$stage/signing-public-v1.b64url
  rm -f -- "$pem" "$private" "$public" "$stage/.private.der" "$stage/.public.der"
  openssl genpkey -algorithm ED25519 -out "$pem"
  openssl pkey -in "$pem" -outform DER -out "$stage/.private.der"
  openssl pkey -in "$pem" -pubout -outform DER -out "$stage/.public.der"
  if [ "$(wc -c <"$stage/.private.der"|tr -d ' ')" != 48 ] || [ "$(wc -c <"$stage/.public.der"|tr -d ' ')" != 44 ]; then die "OpenSSL emitted an unexpected Ed25519 encoding"; fi
  tail -c 32 "$stage/.private.der" | openssl base64 -A | tr '+/' '-_' | tr -d '=' >"$private"; printf '\n' >>"$private"
  tail -c 32 "$stage/.public.der" | openssl base64 -A | tr '+/' '-_' | tr -d '=' >"$public"; printf '\n' >>"$public"
  chmod 0444 "$private" "$public"; sync_path "$private"; sync_path "$public"; rm -f -- "$pem" "$stage/.private.der" "$stage/.public.der"; sync_path "$stage"
  validate_b64 "$private" 444; validate_b64 "$public" 444
  write_phase key-written; phase=key-written; fault key-written
fi
if [ "$phase" = key-written ]; then
  public=$(sed -n '1p' "$stage/signing-public-v1.b64url")
  padded=$(printf '%s' "$public" | tr '_-' '/+'); case $((${#public} % 4)) in 2) padded=${padded}== ;; 3) padded=${padded}= ;; esac
  fingerprint=sha256:$(printf '%s' "$padded" | openssl base64 -d -A | sha256sum | awk '{print $1}')
  tmp=$stage/signing-public-v1.json.tmp.$$
  jq -cn --arg publicKey "$public" --arg fingerprint "$fingerprint" '{schemaVersion:"blazn.dev/node-plan-signing-key/v1",keyId:"control-plane-node-plan/v1",publicKey:$publicKey,publicKeyFingerprint:$fingerprint}' >"$tmp"
  chmod 0444 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$stage/signing-public-v1.json"; sync_path "$stage"
  write_phase metadata-written; phase=metadata-written; fault metadata-written
fi
if [ "$phase" = metadata-written ]; then
  tmp=$stage/node-install-plan-template-v1.json.tmp.$$; cp -- "$SOURCE_TEMPLATE" "$tmp"; chmod 0444 "$tmp"; sync_path "$tmp"; mv -- "$tmp" "$stage/node-install-plan-template-v1.json"; sync_path "$stage"
  write_phase template-written; phase=template-written; fault template-written
fi
if [ "$phase" = template-written ]; then
  validate_tree "$stage"
  if [ -d "$stage" ] && [ ! -e "$ROOT" ]; then mv -- "$stage" "$ROOT"; sync_path "$(dirname -- "$ROOT")"; elif [ -d "$ROOT" ] && [ ! -e "$stage" ]; then :; else die "plan material publication is ambiguous"; fi
  validate_tree "$ROOT"; write_phase published; phase=published; fault published
fi
[ "$phase" = published ] || die "plan material creation did not finish"
validate_tree "$ROOT"
printf 'created or verified Node plan material under %s\n' "$ROOT"
