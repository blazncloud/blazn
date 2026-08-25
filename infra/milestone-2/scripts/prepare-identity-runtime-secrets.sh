#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "identity runtime secret publication must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "identity runtime secret publication must run through with-control-plane-lock.sh"
identity_overlay_enabled || exit 0

identity_env=${BLAZN_IDENTITY_ENV_FILE:-/etc/blazn/identity/control-api.env}
assert_regular_file_owned_mode "$identity_env" 0 600
identity_source_root=$(sed -n 's/^BLAZN_IDENTITY_SECRETS_ROOT=//p' "$identity_env")
[ "$(grep -c '^BLAZN_IDENTITY_SECRETS_ROOT=' "$identity_env")" -eq 1 ] || die "BLAZN_IDENTITY_SECRETS_ROOT must occur exactly once"
publish_identity_runtime_secrets "$identity_source_root" /etc/blazn/control-plane/identity-secrets
