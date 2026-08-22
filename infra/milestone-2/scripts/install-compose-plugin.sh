#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "Compose plugin installation must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Compose plugin installation must run through with-control-plane-lock.sh"
require_command curl
require_command docker
require_command jq
require_command sha256sum

version=v2.39.2
case $(uname -m) in
  x86_64|amd64)
    asset=docker-compose-linux-x86_64
    expected=a55a8cd4ef103aac282812554e531aac8df7e914a287ee81e14d695556a22902
    ;;
  aarch64|arm64)
    asset=docker-compose-linux-aarch64
    expected=54488fffb60782f3c8787a48b95ed15f49f5a3a85f4105304bd46db5edd9db61
    ;;
  *) die "unsupported Compose plugin architecture: $(uname -m)" ;;
esac

config_root=${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}
plugin_dir=$config_root/cli-plugins
plugin=$plugin_dir/docker-compose
receipt=${BLAZN_COMPOSE_RECEIPT_PATH:-/var/lib/blazn/ownership/docker-compose-plugin.json}
require_absolute_path BLAZN_DOCKER_CONFIG_ROOT "$config_root"
require_absolute_path BLAZN_COMPOSE_RECEIPT_PATH "$receipt"
assert_not_symlink_chain "$plugin_dir"
assert_not_symlink_chain "$receipt"

if [ -e "$plugin" ]; then
  [ -f "$plugin" ] && [ ! -L "$plugin" ] || die "existing Compose plugin is not a regular owned file"
  [ "$(stat -c '%u' "$plugin")" -eq 0 ] || die "Compose plugin must be owned by root"
  [ "$(stat -c '%a' "$plugin")" = 755 ] || die "Compose plugin must have mode 0755"
  [ -f "$receipt" ] || die "existing Compose plugin has no Blazn ownership receipt"
  [ "$(stat -c '%u' "$receipt")" -eq 0 ] || die "Compose plugin receipt must be owned by root"
  [ "$(stat -c '%a' "$receipt")" = 600 ] || die "Compose plugin receipt must have mode 0600"
  actual=$(sha256_file "$plugin")
  [ "$actual" = "$expected" ] || die "owned Compose plugin checksum does not match $version"
  jq -e --arg path "$plugin" --arg digest "sha256:$expected" --arg version "$version" \
    '.schemaVersion == "blazn.dev/dependency-ownership/v1" and .owner == "blazn-poc" and .path == $path and .digest == $digest and .version == $version' \
    "$receipt" >/dev/null || die "Compose plugin ownership receipt is invalid"
  "$plugin" version --short | grep -Fx "${version#v}" >/dev/null || die "owned Compose plugin direct version smoke failed"
  DOCKER_CONFIG=$config_root docker compose version --short | grep -Fx "${version#v}" >/dev/null || die "owned Compose plugin discovery smoke failed"
  printf 'Docker Compose %s is already installed and owned by Blazn\n' "$version"
  exit 0
fi
[ ! -e "$receipt" ] || die "Compose ownership receipt exists without its plugin"

umask 077
mkdir -p -- "$plugin_dir" "$(dirname -- "$receipt")"
tmp=$plugin_dir/.docker-compose.$$
installed=0
receipt_tmp=
cleanup() {
  rm -f -- "$tmp"
  [ -z "$receipt_tmp" ] || rm -f -- "$receipt_tmp"
  [ "$installed" -eq 0 ] || rm -f -- "$plugin" "$receipt"
}
trap cleanup EXIT HUP INT TERM
url=https://github.com/docker/compose/releases/download/$version/$asset
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --output "$tmp" "$url"
actual=$(sha256_file "$tmp")
[ "$actual" = "$expected" ] || die "downloaded Compose plugin checksum mismatch"
chmod 0755 "$tmp"
ln -- "$tmp" "$plugin" || die "Compose plugin target appeared during installation"
installed=1
rm -f -- "$tmp"
"$plugin" version --short | grep -Fx "${version#v}" >/dev/null || {
  rm -f -- "$plugin"
  die "Compose plugin direct version smoke failed"
}
DOCKER_CONFIG=$config_root docker compose version --short | grep -Fx "${version#v}" >/dev/null || {
  rm -f -- "$plugin"
  die "Compose plugin discovery smoke failed"
}

created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
receipt_tmp=$receipt.tmp.$$
jq -cn --arg path "$plugin" --arg digest "sha256:$expected" --arg version "$version" --arg createdAt "$created_at" \
  '{schemaVersion:"blazn.dev/dependency-ownership/v1",owner:"blazn-poc",path:$path,digest:$digest,version:$version,createdAt:$createdAt}' >"$receipt_tmp"
chmod 0600 "$receipt_tmp"
ln -- "$receipt_tmp" "$receipt" || {
  rm -f -- "$plugin" "$receipt_tmp"
  die "Compose ownership receipt target appeared during installation"
}
rm -f -- "$receipt_tmp"
installed=0
trap - EXIT HUP INT TERM
printf 'installed Docker Compose %s with verified sha256:%s\n' "$version" "$expected"
