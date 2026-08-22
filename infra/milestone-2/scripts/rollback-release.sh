#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "release rollback must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "release rollback must run through with-control-plane-lock.sh"
[ "$#" -eq 0 ] || die "usage: rollback-release.sh"
active_receipt=${BLAZN_ACTIVE_RELEASE_RECEIPT:-/var/lib/blazn/ownership/active-release.json}
assert_regular_file_owned_mode "$active_receipt" 0 600
previous=$(jq -er '.previousId // empty' "$active_receipt")
[ -n "$previous" ] || die "active release receipt has no previous release"
exec "$SCRIPT_DIR/promote-release.sh" "$previous"
