#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "workspace secret upgrade must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] && [ -n "${BLAZN_CORRELATION_ID:-}" ] || die "workspace secret upgrade must run through with-control-plane-lock.sh"
require_command jq
require_command openssl
require_command sha256sum

SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
UPGRADE_RECEIPT=${BLAZN_WORKSPACE_SECRET_UPGRADE_RECEIPT_PATH:-/var/lib/blazn/ownership/workspace-secret-upgrade.json}
SECRET=$SECRETS_ROOT/workspace-invitation-hmac-v1
STAGE=$SECRETS_ROOT/.workspace-secret-upgrade-staging
ACTIVE_RELEASE_RECEIPT=${BLAZN_ACTIVE_RELEASE_RECEIPT:-/var/lib/blazn/ownership/active-release.json}

assert_directory_owned_mode "$SECRETS_ROOT" 0 700
assert_regular_file_owned_mode "$MAIN_RECEIPT" 0 600
assert_regular_file_owned_mode "$ACTIVE_RELEASE_RECEIPT" 0 600
release_digest=$(jq -er '.releaseDigest | select(test("^sha256:[a-f0-9]{64}$"))' "$ACTIVE_RELEASE_RECEIPT")
require_absolute_path BLAZN_WORKSPACE_SECRET_UPGRADE_RECEIPT_PATH "$UPGRADE_RECEIPT"
assert_not_symlink_chain "$UPGRADE_RECEIPT"
jq -e --arg host "$(hostname)" --arg secrets "$SECRETS_ROOT" \
  '.schemaVersion == "blazn.dev/control-plane-ownership/v1" and .owner == "blazn-poc" and .host == $host and .paths.secrets == $secrets' \
  "$MAIN_RECEIPT" >/dev/null || die "control-plane receipt does not match this host and secrets root"

validate_upgrade_receipt() {
  assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
  validate_workspace_invitation_secret "$SECRET"
  digest=sha256:$(sha256_file "$SECRET")
  jq -e --arg host "$(hostname)" --arg secrets "$SECRETS_ROOT" --arg digest "$digest" --arg releaseDigest "$release_digest" --arg correlationId "$BLAZN_CORRELATION_ID" --argjson fencingToken "$BLAZN_FENCING_TOKEN" \
    '.schemaVersion == "blazn.dev/workspace-secret-upgrade/v1" and .owner == "blazn-poc" and .host == $host and .secretsRoot == $secrets and .secretDigest == $digest and .releaseDigest == $releaseDigest and .correlationId == $correlationId and (.fencingToken | type == "number" and . <= $fencingToken)' \
    "$UPGRADE_RECEIPT" >/dev/null || die "workspace secret upgrade receipt is invalid"
  main_digest=$(jq -r '.secretDigests["workspace-invitation-hmac-v1"] // empty' "$MAIN_RECEIPT")
  [ -z "$main_digest" ] || [ "$main_digest" = "$digest" ] || \
    die "main receipt binds a different workspace invitation HMAC key"
}

if [ -e "$UPGRADE_RECEIPT" ]; then
  validate_upgrade_receipt
else
  if [ ! -e "$STAGE" ]; then
    [ ! -e "$SECRET" ] || die "workspace invitation HMAC key exists without an upgrade receipt or recovery staging directory"
    umask 077
    mkdir -- "$STAGE"
    chmod 0700 "$STAGE"
  fi
  assert_directory_owned_mode "$STAGE" 0 700
  staged=$STAGE/workspace-invitation-hmac-v1
  if [ ! -e "$staged" ]; then
    tmp=$staged.tmp.$$
    umask 077
    openssl rand -hex 32 >"$tmp"
    chmod 0444 "$tmp"
    ln -- "$tmp" "$staged" || {
      rm -f -- "$tmp"
      die "workspace secret staging target appeared during upgrade"
    }
    rm -f -- "$tmp"
  fi
  validate_workspace_invitation_secret "$staged"
  if [ -e "$SECRET" ]; then
    validate_workspace_invitation_secret "$SECRET"
    [ "$(sha256_file "$staged")" = "$(sha256_file "$SECRET")" ] || \
      die "partial workspace secret upgrade target differs from staged value"
  else
    ln -- "$staged" "$SECRET" || die "workspace secret target appeared during atomic installation"
  fi
  validate_workspace_invitation_secret "$SECRET"

  mkdir -p -- "$(dirname -- "$UPGRADE_RECEIPT")"
  assert_directory_owned_mode "$(dirname -- "$UPGRADE_RECEIPT")" 0 700
  receipt_tmp=$UPGRADE_RECEIPT.tmp.$$
  jq -cn \
    --arg host "$(hostname)" \
    --arg secrets "$SECRETS_ROOT" \
    --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg digest "sha256:$(sha256_file "$SECRET")" \
    --arg releaseDigest "$release_digest" \
    --arg correlationId "$BLAZN_CORRELATION_ID" \
    --argjson fencingToken "$BLAZN_FENCING_TOKEN" \
    '{schemaVersion:"blazn.dev/workspace-secret-upgrade/v1",owner:"blazn-poc",host:$host,secretsRoot:$secrets,createdAt:$createdAt,secretDigest:$digest,releaseDigest:$releaseDigest,fencingToken:$fencingToken,correlationId:$correlationId}' \
    >"$receipt_tmp"
  chmod 0600 "$receipt_tmp"
  ln -- "$receipt_tmp" "$UPGRADE_RECEIPT" || {
    rm -f -- "$receipt_tmp"
    die "workspace secret upgrade receipt target appeared during installation"
  }
  rm -f -- "$receipt_tmp"
  validate_upgrade_receipt
fi

if [ -d "$STAGE" ]; then
  assert_directory_owned_mode "$STAGE" 0 700
  staged=$STAGE/workspace-invitation-hmac-v1
  if [ -e "$staged" ]; then
    validate_workspace_invitation_secret "$staged"
    [ "$(sha256_file "$staged")" = "$(sha256_file "$SECRET")" ] || die "staged workspace secret differs from installed value"
    rm -f -- "$staged"
  fi
  rmdir -- "$STAGE" || die "workspace secret staging directory contains an unexpected entry"
fi

printf 'workspace invitation HMAC key is installed; reconcile the main ownership receipt separately\n'
