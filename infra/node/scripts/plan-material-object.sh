#!/bin/sh
set -eu

ROOT=${BLAZN_NODE_PLAN_ROOT:-/etc/blazn/node-plan}
JOURNAL=${BLAZN_NODE_PLAN_CREATE_JOURNAL:-/var/lib/blazn/ownership/node-plan-material-create.json}
for command_name in jq openssl sha256sum stat wc; do command -v "$command_name" >/dev/null 2>&1 || { printf 'Node plan verifier requires %s\n' "$command_name" >&2; exit 1; }; done
[ -d "$ROOT" ] && [ ! -L "$ROOT" ] && [ "$(stat -c '%u:%a' "$ROOT")" = 0:700 ] || { printf 'Node plan material root is unsafe\n' >&2; exit 1; }
for file in signing-private-v1.b64url signing-public-v1.b64url signing-public-v1.json node-install-plan-template-v1.json; do
  [ -f "$ROOT/$file" ] && [ ! -L "$ROOT/$file" ] && [ "$(stat -c '%u:%a' "$ROOT/$file")" = 0:444 ] || { printf 'Node plan material is unsafe: %s\n' "$file" >&2; exit 1; }
done
[ -f "$JOURNAL" ] && [ ! -L "$JOURNAL" ] && [ "$(stat -c '%u:%a' "$JOURNAL")" = 0:600 ] || { printf 'Node plan journal is unsafe\n' >&2; exit 1; }
private=$(sed -n '1p' "$ROOT/signing-private-v1.b64url")
if [ "$(wc -c <"$ROOT/signing-private-v1.b64url" | tr -d ' ')" != 44 ] || ! LC_ALL=C grep -Eq '^[A-Za-z0-9_-]{43}$' "$ROOT/signing-private-v1.b64url"; then
  printf 'Node plan private seed is not a raw base64url Ed25519 seed\n' >&2
  exit 1
fi
metadata=$(jq -cS . "$ROOT/signing-public-v1.json")
public=$(printf '%s' "$metadata" | jq -er .publicKey)
[ "$public" = "$(sed -n '1p' "$ROOT/signing-public-v1.b64url")" ] || { printf 'Node plan public metadata mismatch\n' >&2; exit 1; }
private_padded=$(printf '%s' "$private" | tr '_-' '/+'); case $((${#private} % 4)) in 2) private_padded=${private_padded}== ;; 3) private_padded=${private_padded}= ;; esac
derived_public=$(
  { printf '\060\056\002\001\000\060\005\006\003\053\145\160\004\042\004\040'; printf '%s' "$private_padded" | openssl base64 -d -A; } |
    openssl pkey -inform DER -pubout -outform DER 2>/dev/null | tail -c 32 | openssl base64 -A | tr '+/' '-_' | tr -d '='
)
[ "$derived_public" = "$public" ] || { printf 'Node plan private seed does not match public metadata\n' >&2; exit 1; }
key_id=$(printf '%s' "$metadata" | jq -er .keyId)
fingerprint=$(printf '%s' "$metadata" | jq -er .publicKeyFingerprint)
[ "$key_id" = control-plane-node-plan/v1 ] || { printf 'Node plan key ID mismatch\n' >&2; exit 1; }
case "$fingerprint" in sha256:????????????????????????????????????????????????????????????????) ;; *) printf 'Node plan fingerprint is invalid\n' >&2; exit 1 ;; esac
public_padded=$(printf '%s' "$public" | tr '_-' '/+'); case $((${#public} % 4)) in 2) public_padded=${public_padded}== ;; 3) public_padded=${public_padded}= ;; esac
derived_fingerprint=sha256:$(printf '%s' "$public_padded" | openssl base64 -d -A | sha256sum | awk '{print $1}')
[ "$fingerprint" = "$derived_fingerprint" ] || { printf 'Node plan public fingerprint does not match key material\n' >&2; exit 1; }
template_id=$(jq -er .templateId "$ROOT/node-install-plan-template-v1.json")
[ "$template_id" = frontro-poc-worker/v1 ] || { printf 'Node plan template ID mismatch\n' >&2; exit 1; }
journal_path=$JOURNAL
case "$journal_path" in /var/lib/blazn/ownership/node-plan-material-create.json|/var/lib/blazn/ownership/node-plan-material-upgrade-create.json) ;; *) [ "${BLAZN_NODE_PLAN_TEST_MODE:-0}" = 1 ] || { printf 'Node plan journal path is not reviewed\n' >&2; exit 1; } ;; esac
jq -cnS \
  --arg fingerprint "$fingerprint" --arg templateId "$template_id" \
  --arg templateDigest "sha256:$(sha256sum "$ROOT/node-install-plan-template-v1.json" | awk '{print $1}')" \
  --arg journal "$journal_path" --arg journalDigest "sha256:$(sha256sum "$JOURNAL" | awk '{print $1}')" \
  '{schemaVersion:"blazn.dev/node-plan-material/v1",root:"/etc/blazn/node-plan",keyId:"control-plane-node-plan/v1",publicKeyFingerprint:$fingerprint,templateId:$templateId,templateDigest:$templateDigest,creationJournal:{path:$journal,digest:$journalDigest}}'
