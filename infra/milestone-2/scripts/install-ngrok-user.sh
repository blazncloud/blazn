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

group_name=$(id -gn blazn-ngrok)
home=$(getent passwd blazn-ngrok | awk -F: '{print $6}')
shell=$(getent passwd blazn-ngrok | awk -F: '{print $7}')
uid=$(id -u blazn-ngrok)
[ "$group_name" = blazn-ngrok ] || die "blazn-ngrok user has an unexpected primary group"
[ "$home" = /nonexistent ] || die "blazn-ngrok user has an unexpected home directory"
[ "$shell" = /usr/sbin/nologin ] || die "blazn-ngrok user has an interactive or unexpected shell"
[ "$uid" -lt 1000 ] || die "blazn-ngrok must use a system UID"
printf 'validated dedicated blazn-ngrok system identity\n'
