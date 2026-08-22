#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "ngrok service identity installation must run as root"
require_command getent
mode=${1:-install}
case "$mode" in install|--validate-only) ;; *) die "usage: install-ngrok-user.sh [--validate-only]" ;; esac

if ! getent group blazn-ngrok >/dev/null 2>&1; then
  [ "$mode" = install ] || die "dedicated blazn-ngrok group is not installed"
  require_command groupadd
  groupadd --system blazn-ngrok
fi
if ! getent passwd blazn-ngrok >/dev/null 2>&1; then
  [ "$mode" = install ] || die "dedicated blazn-ngrok user is not installed"
  require_command useradd
  useradd --system --gid blazn-ngrok --home-dir /nonexistent --shell /usr/sbin/nologin blazn-ngrok
fi

config_parent=${BLAZN_CONFIG_ROOT:-/etc/blazn}
require_absolute_path BLAZN_CONFIG_ROOT "$config_parent"
assert_not_symlink_chain "$config_parent"
if [ ! -e "$config_parent" ]; then
  [ "$mode" = install ] || die "Blazn configuration root is not installed"
  install -d -o root -g blazn-ngrok -m 0710 "$config_parent"
elif [ "$mode" = install ]; then
  if [ ! -d "$config_parent" ] || [ -L "$config_parent" ]; then
    die "Blazn configuration root is unsafe"
  fi
  chown root:blazn-ngrok "$config_parent"
  chmod 0710 "$config_parent"
fi
if [ ! -d "$config_parent" ] || [ -L "$config_parent" ]; then
  die "Blazn configuration root is unsafe"
fi
[ "$(stat -c '%U:%G:%a' "$config_parent")" = root:blazn-ngrok:710 ] || die "Blazn configuration root must be root:blazn-ngrok mode 0710"

group_name=$(id -gn blazn-ngrok)
home=$(getent passwd blazn-ngrok | awk -F: '{print $6}')
shell=$(getent passwd blazn-ngrok | awk -F: '{print $7}')
uid=$(id -u blazn-ngrok)
primary_gid=$(id -g blazn-ngrok)
group_ids=$(id -G blazn-ngrok)
[ "$group_name" = blazn-ngrok ] || die "blazn-ngrok user has an unexpected primary group"
[ "$home" = /nonexistent ] || die "blazn-ngrok user has an unexpected home directory"
[ "$shell" = /usr/sbin/nologin ] || die "blazn-ngrok user has an interactive or unexpected shell"
[ "$uid" -lt 1000 ] || die "blazn-ngrok must use a system UID"
[ "$group_ids" = "$primary_gid" ] || die "blazn-ngrok has unreviewed supplementary group memberships"
printf 'validated dedicated blazn-ngrok system identity\n'
