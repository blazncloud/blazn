#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
BRANDING_DIR=$SCRIPT_DIR/branding
TOKEN_FILE=${ZITADEL_BRANDING_TOKEN_FILE:-/run/blazn-zitadel-admin-session-token}
API_ORIGIN=${ZITADEL_BRANDING_API_ORIGIN:-http://127.0.0.1:58081}
DOMAIN=${ZITADEL_BRANDING_DOMAIN:-auth.blazn.benpelo.com}

die() { printf 'blazn identity branding: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "installer must run as root"
for command_name in curl jq stat mktemp grep wc; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
[ "$API_ORIGIN" = http://127.0.0.1:58081 ] || die "branding API origin differs from the reviewed loopback endpoint"
[ "$DOMAIN" = auth.blazn.benpelo.com ] || die "branding domain differs from the reviewed identity hostname"
case $TOKEN_FILE in /*) ;; *) die "token file path must be absolute" ;; esac
if [ -L "$TOKEN_FILE" ] || [ ! -f "$TOKEN_FILE" ]; then
  die "token file is unavailable or symlinked"
fi
[ "$(stat -c '%u:%a:%h' -- "$TOKEN_FILE")" = 0:600:1 ] || die "token file must be root-owned mode 0600 with one link"
token=$(cat "$TOKEN_FILE")
printf '%s\n' "$token" | LC_ALL=C grep -Eq '^[A-Za-z0-9._~-]{32,4096}$' || die "token format is invalid"

for asset in branding.json logo-light.svg logo-dark.svg icon-light.svg icon-dark.svg; do
  path=$BRANDING_DIR/$asset
  if [ -L "$path" ] || [ ! -f "$path" ]; then
    die "branding asset is missing or symlinked: $asset"
  fi
  size=$(wc -c <"$path" | tr -d ' ')
  case $size in ''|*[!0-9]*) die "branding asset size is invalid: $asset" ;; esac
  if [ "$size" -le 0 ] || [ "$size" -gt 262144 ]; then
    die "branding asset size is invalid: $asset"
  fi
done
jq -e 'type == "object" and .primaryColor == "#f97316" and .primaryColorDark == "#fb923c" and .backgroundColor == "#fff7ed" and .backgroundColorDark == "#090a0f" and .disableWatermark == true and .themeMode == "THEME_MODE_AUTO"' "$BRANDING_DIR/branding.json" >/dev/null || die "branding policy differs from the reviewed palette"

request_dir=$(mktemp -d /tmp/blazn-branding.XXXXXX)
cleanup() { rm -rf -- "$request_dir"; }
trap cleanup EXIT HUP INT TERM
chmod 0700 "$request_dir"
auth_config=$request_dir/curl-auth.conf
umask 077
printf 'header = "Authorization: Bearer %s"\n' "$token" >"$auth_config"
unset token

request() {
  method=$1
  path=$2
  shift 2
  curl --config "$auth_config" --fail-with-body --silent --show-error --retry 2 \
    --retry-max-time 30 --connect-timeout 3 --max-time 15 \
    --request "$method" --header "Host: $DOMAIN" --header 'X-Forwarded-Proto: https' \
    "$@" "$API_ORIGIN$path"
}

request PUT /admin/v1/policies/label --header 'Content-Type: application/json' --data-binary "@$BRANDING_DIR/branding.json" >/dev/null
request POST /assets/v1/instance/policy/label/logo --header 'Content-Type: image/svg+xml' --data-binary "@$BRANDING_DIR/logo-light.svg" >/dev/null
request POST /assets/v1/instance/policy/label/logo/dark --header 'Content-Type: image/svg+xml' --data-binary "@$BRANDING_DIR/logo-dark.svg" >/dev/null
request POST /assets/v1/instance/policy/label/icon --header 'Content-Type: image/svg+xml' --data-binary "@$BRANDING_DIR/icon-light.svg" >/dev/null
request POST /assets/v1/instance/policy/label/icon/dark --header 'Content-Type: image/svg+xml' --data-binary "@$BRANDING_DIR/icon-dark.svg" >/dev/null
request POST /admin/v1/policies/label/_activate --header 'Content-Type: application/json' --data '{}' >/dev/null

actual=$request_dir/actual.json
request GET /admin/v1/policies/label >"$actual"
jq -e --slurpfile expected "$BRANDING_DIR/branding.json" '
  .policy as $p | $expected[0] as $e |
  $p.primaryColor == $e.primaryColor and $p.warnColor == $e.warnColor and
  $p.backgroundColor == $e.backgroundColor and $p.fontColor == $e.fontColor and
  $p.primaryColorDark == $e.primaryColorDark and $p.warnColorDark == $e.warnColorDark and
  $p.backgroundColorDark == $e.backgroundColorDark and $p.fontColorDark == $e.fontColorDark and
  $p.hideLoginNameSuffix == true and $p.disableWatermark == true and
  $p.themeMode == "THEME_MODE_AUTO" and
  ($p.logoUrl | length > 0) and ($p.logoUrlDark | length > 0) and
  ($p.iconUrl | length > 0) and ($p.iconUrlDark | length > 0)
' "$actual" >/dev/null || die "active branding does not match the reviewed Blazn policy"

printf 'blazn identity branding: active\n'
