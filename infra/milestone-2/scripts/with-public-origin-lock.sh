#!/bin/sh
set -eu

[ "$#" -ge 2 ] || { printf 'usage: with-public-origin-lock.sh CORRELATION_ID COMMAND [ARG...]\n' >&2; exit 1; }
correlation=$1
shift
case "$correlation" in ''|*[!a-zA-Z0-9._-]*) printf 'invalid correlation ID\n' >&2; exit 1 ;; esac
command -v flock >/dev/null 2>&1 || { printf 'flock is required\n' >&2; exit 1; }

state_root=${XDG_STATE_HOME:-$HOME/.local/state}/blazn/public-origin
umask 077
mkdir -p "$state_root"
chmod 0700 "$state_root"
exec 9>"$state_root/blazn.benpelo.com.lock"
flock -x 9
holder=$state_root/blazn.benpelo.com.holder
[ ! -e "$holder" ] || { printf 'stale public-origin holder requires reconciliation: %s\n' "$holder" >&2; exit 1; }
printf '{"correlationId":"%s","pid":%s,"createdAt":"%s"}\n' "$correlation" "$$" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$holder"
cleanup() { rm -f "$holder"; }
trap cleanup EXIT HUP INT TERM
"$@"
