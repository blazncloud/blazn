#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$#" -ge 2 ] || { printf 'usage: with-public-origin-lock.sh CORRELATION_ID COMMAND [ARG...]\n' >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "public-origin lifetime lock must run as root"
correlation=$1
shift
case "$correlation" in ''|*[!a-zA-Z0-9._-]*) printf 'invalid correlation ID\n' >&2; exit 1 ;; esac
require_command flock

state_root=${BLAZN_PUBLIC_ORIGIN_LOCK_ROOT:-/run/lock/blazn-poc/public-origin}
require_absolute_path BLAZN_PUBLIC_ORIGIN_LOCK_ROOT "$state_root"
assert_not_symlink_chain "$state_root"
umask 077
mkdir -p "$state_root"
chmod 0700 "$state_root"
exec 9>"$state_root/blazn.benpelo.com.lock"
flock -n -x 9 || die "the blazn.benpelo.com public origin is already owned"
holder=$state_root/blazn.benpelo.com.holder
[ ! -e "$holder" ] || { printf 'stale public-origin holder requires reconciliation: %s\n' "$holder" >&2; exit 1; }
printf '{"correlationId":"%s","pid":%s,"createdAt":"%s"}\n' "$correlation" "$$" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$holder"
cleanup() { rm -f "$holder"; }
trap cleanup EXIT HUP INT TERM
"$@"
