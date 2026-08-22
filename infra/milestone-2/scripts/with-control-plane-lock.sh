#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

[ "$#" -ge 4 ] || die "usage: with-control-plane-lock.sh PURPOSE CORRELATION_ID EXPECTED_TOKEN COMMAND [ARG...]"
[ "$(id -u)" -eq 0 ] || die "control-plane mutation lock must run as root"
require_command flock

purpose=$1
correlation=$2
expected=$3
shift 3
LOCK_ROOT=${BLAZN_LOCK_ROOT:-/run/lock/blazn-poc}
require_absolute_path BLAZN_LOCK_ROOT "$LOCK_ROOT"
assert_not_symlink_chain "$LOCK_ROOT"

umask 077
mkdir -p -- "$LOCK_ROOT"
chmod 0700 -- "$LOCK_ROOT"
exec 9>"$LOCK_ROOT/ben1-control-plane-mutation.lock"
flock -x 9

counter=$LOCK_ROOT/ben1-control-plane-mutation.counter
current=0
if [ -f "$counter" ]; then
  current=$(sed -n '1p' "$counter")
  is_uint "$current" || die "lock fencing counter is corrupt"
fi
token=$((current + 1))
[ "$expected" = auto ] || [ "$expected" = "$token" ] || die "fencing token mismatch: expected $expected, next is $token"

counter_tmp=$counter.tmp.$$
printf '%s\n' "$token" >"$counter_tmp"
chmod 0600 "$counter_tmp"
mv -f -- "$counter_tmp" "$counter"

holder=$LOCK_ROOT/ben1-control-plane-mutation.holder
holder_tmp=$holder.tmp.$$
created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
printf '{"owner":"blazn-poc","purpose":"%s","correlationId":"%s","fencingToken":%s,"pid":%s,"createdAt":"%s"}\n' \
  "$purpose" "$correlation" "$token" "$$" "$created_at" >"$holder_tmp"
chmod 0600 "$holder_tmp"
mv -f -- "$holder_tmp" "$holder"

cleanup() {
  rm -f -- "$holder"
}
trap cleanup EXIT HUP INT TERM

BLAZN_FENCING_TOKEN=$token BLAZN_CORRELATION_ID=$correlation BLAZN_MUTATION_PURPOSE=$purpose "$@"
