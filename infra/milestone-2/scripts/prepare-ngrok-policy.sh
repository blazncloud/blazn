#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "ngrok policy preparation must run as root"
require_command getent
getent passwd blazn-ngrok >/dev/null 2>&1 || die "dedicated blazn-ngrok user is not installed"
getent group blazn-ngrok >/dev/null 2>&1 || die "dedicated blazn-ngrok group is not installed"

secret_file=${BLAZN_PROXY_AUTH_SECRET_FILE:-/etc/blazn/control-plane/secrets/proxy-auth-secret}
config_file=${BLAZN_NGROK_CONFIG_FILE:-/etc/blazn/ngrok/ngrok.yml}
policy_file=${BLAZN_NGROK_POLICY_FILE:-/run/blazn-ngrok/traffic-policy.yml}
assert_regular_file_owned_mode "$secret_file" 0 444
config_dir=$(dirname -- "$config_file")
assert_directory_owned_mode "$config_dir" 0 750
[ "$(stat -c '%G' "$config_dir")" = blazn-ngrok ] || die "dedicated ngrok config directory must be readable only by blazn-ngrok"
if [ ! -f "$config_file" ] || [ -L "$config_file" ]; then
  die "dedicated ngrok config is unavailable"
fi
[ "$(stat -c '%u' "$config_file")" = 0 ] || die "dedicated ngrok config must be root-owned"
[ "$(stat -c '%a' "$config_file")" = 640 ] || die "dedicated ngrok config must have mode 0640"
[ "$(stat -c '%G' "$config_file")" = blazn-ngrok ] || die "dedicated ngrok config must be readable only by blazn-ngrok"

secret=$(sed -n '1p' "$secret_file")
case "$secret" in
  ????????????????????????????????????????????????????????????????) ;;
  *) die "proxy authentication secret has an unexpected length" ;;
esac
case "$secret" in *[!a-f0-9]*) die "proxy authentication secret must be lowercase hexadecimal" ;; esac

policy_dir=$(dirname -- "$policy_file")
mkdir -p -- "$policy_dir"
chown root:blazn-ngrok "$policy_dir"
chmod 0750 "$policy_dir"
tmp=$policy_file.tmp.$$
umask 077
{
  printf '%s\n' 'on_http_request:'
  printf '%s\n' '  - actions:'
  printf '%s\n' '      - type: remove-headers'
  printf '%s\n' '        config:'
  printf '%s\n' '          headers:'
  printf '%s\n' '            - x-blazn-proxy-authorization'
  printf '%s\n' '      - type: add-headers'
  printf '%s\n' '        config:'
  printf '%s\n' '          headers:'
  printf '            x-blazn-proxy-authorization: "%s"\n' "$secret"
} >"$tmp"
chown root:blazn-ngrok "$tmp"
chmod 0640 "$tmp"
mv -- "$tmp" "$policy_file"
printf 'prepared root-controlled ngrok traffic policy\n'
