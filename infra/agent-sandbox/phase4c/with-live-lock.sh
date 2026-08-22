#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || { printf 'the authoritative lock launcher must run as root\n' >&2; exit 1; }
[ "$#" -ge 1 ] || { printf 'usage: %s COMMAND [ARG...]\n' "$0" >&2; exit 64; }
command -v flock >/dev/null 2>&1 || { printf 'flock is required\n' >&2; exit 1; }
lock_dir=/run/lock/blazn
lock_path=$lock_dir/live-cluster-mutation.lock
fence_path=$lock_dir/live-cluster-mutation.fence
install -d -o root -g root -m 0700 "$lock_dir"
[ ! -L "$lock_path" ] && [ ! -L "$fence_path" ] || { printf 'lock state must not be symlinks\n' >&2; exit 1; }
if [ -e "$lock_path" ]; then
  [ -f "$lock_path" ] && [ "$(stat -c '%u:%a:%h' "$lock_path")" = '0:600:1' ] || {
    printf 'unsafe live-cluster lock metadata\n' >&2
    exit 1
  }
else
  (umask 077; set -C; : >"$lock_path") || { printf 'could not create lock without clobbering\n' >&2; exit 1; }
  chown root:root "$lock_path"
fi
exec 9>"$lock_path"
flock -n 9 || { printf 'another live-cluster mutation owns the lock\n' >&2; exit 75; }

if [ -e "$fence_path" ]; then
  [ -f "$fence_path" ] && [ "$(stat -c '%u:%a:%h' "$fence_path")" = '0:600:1' ] || {
    printf 'unsafe fencing counter metadata\n' >&2
    exit 1
  }
  old=$(cat "$fence_path")
  case "$old" in ''|*[!0-9]*) printf 'invalid fencing counter\n' >&2; exit 1 ;; esac
else
  old=0
fi
token=$((old + 1))
tmp=$(mktemp "$lock_dir/.live-cluster-mutation.fence.XXXXXX")
printf '%s\n' "$token" >"$tmp"
chown root:root "$tmp"
chmod 0600 "$tmp"
mv "$tmp" "$fence_path"

export BLAZN_FENCING_TOKEN=$token
export BLAZN_LIVE_CLUSTER_LOCK_HELD="token:$token"
exec "$@"
