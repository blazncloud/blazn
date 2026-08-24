#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
BRANDING_DIR=$SCRIPT_DIR/branding
TOKEN_FILE=${ZITADEL_BRANDING_TOKEN_FILE:-/run/blazn-zitadel-admin-session-token}
API_ORIGIN=${ZITADEL_BRANDING_API_ORIGIN:-http://127.0.0.1:58081}
DOMAIN=${ZITADEL_BRANDING_DOMAIN:-auth.blazn.benpelo.com}

die() { printf 'blazn identity branding: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "installer must run as root"
for command_name in curl jq stat mktemp grep wc cmp sed; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
[ "$API_ORIGIN" = http://127.0.0.1:58081 ] || die "branding API origin differs from the reviewed loopback endpoint"
[ "$DOMAIN" = auth.blazn.benpelo.com ] || die "branding domain differs from the reviewed identity hostname"
case $TOKEN_FILE in /*) ;; *) die "token file path must be absolute" ;; esac
if [ -L "$TOKEN_FILE" ] || [ ! -f "$TOKEN_FILE" ]; then
  die "token file is unavailable or symlinked"
fi
[ "$(stat -c '%u:%a:%h' -- "$TOKEN_FILE")" = 0:600:1 ] || die "token file must be root-owned mode 0600 with one link"
token=$(cat "$TOKEN_FILE")
token_size=$(printf '%s' "$token" | wc -c | tr -d ' ')
case $token_size in ''|*[!0-9]*) die "token size is invalid" ;; esac
if [ "$token_size" -lt 32 ] || [ "$token_size" -gt 4096 ]; then
  die "token size is invalid"
fi
printf '%s\n' "$token" | LC_ALL=C grep -Eq '^[A-Za-z0-9._~-]+$' || die "token format is invalid"

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

read_request() {
  path=$1
  shift
  curl --config "$auth_config" --fail-with-body --silent --show-error --retry 2 \
    --retry-max-time 30 --connect-timeout 3 --max-time 15 \
    --request GET --header "Host: $DOMAIN" --header 'X-Forwarded-Proto: https' \
    "$@" "$API_ORIGIN$path"
}

write_request() {
  method=$1
  path=$2
  shift 2
  curl --config "$auth_config" --fail-with-body --silent --show-error \
    --connect-timeout 3 --max-time 15 \
    --request "$method" --header "Host: $DOMAIN" --header 'X-Forwarded-Proto: https' \
    "$@" "$API_ORIGIN$path"
}

policy_matches() {
  jq -e --slurpfile expected "$BRANDING_DIR/branding.json" '
    .policy as $p | $expected[0] as $e |
    $p.primaryColor == $e.primaryColor and $p.warnColor == $e.warnColor and
    $p.backgroundColor == $e.backgroundColor and $p.fontColor == $e.fontColor and
    $p.primaryColorDark == $e.primaryColorDark and $p.warnColorDark == $e.warnColorDark and
    $p.backgroundColorDark == $e.backgroundColorDark and $p.fontColorDark == $e.fontColorDark and
    $p.hideLoginNameSuffix == true and $p.disableWatermark == true and
    $p.themeMode == "THEME_MODE_AUTO"
  ' "$1" >/dev/null
}

active_matches() {
  policy_matches "$1" && jq -e '
    (.policy.logoUrl | length > 0) and (.policy.logoUrlDark | length > 0) and
    (.policy.iconUrl | length > 0) and (.policy.iconUrlDark | length > 0)
  ' "$1" >/dev/null
}

mutation_response=$request_dir/mutation-response.json
draft=$request_dir/draft.json
read_request /admin/v1/policies/label/_preview >"$draft"
if ! policy_matches "$draft"; then
  if ! write_request PUT /admin/v1/policies/label --header 'Content-Type: application/json' --data-binary "@$BRANDING_DIR/branding.json" >"$mutation_response"; then
    sed -n '1,20p' "$mutation_response" >&2
    die "branding policy update failed"
  fi
fi

upload_asset() {
  api_path=$1
  source_file=$2
  current=$request_dir/current-$(basename "$source_file")
  status=$(curl --config "$auth_config" --silent --show-error --connect-timeout 3 --max-time 15 \
    --output "$current" --write-out '%{http_code}' --request GET \
    --header "Host: $DOMAIN" --header 'X-Forwarded-Proto: https' "$API_ORIGIN$api_path/_preview") || die "branding asset read failed: $source_file"
  if [ "$status" = 200 ] && cmp -s "$current" "$source_file"; then
    return 0
  fi
  if [ "$status" != 200 ] && [ "$status" != 404 ]; then
    sed -n '1,20p' "$current" >&2
    die "branding asset read failed: $source_file"
  fi
  if ! write_request POST "$api_path" --form "file=@$source_file;type=image/svg+xml" >"$mutation_response"; then
    sed -n '1,20p' "$mutation_response" >&2
    die "branding asset upload failed: $source_file"
  fi
}

upload_asset /assets/v1/instance/policy/label/logo "$BRANDING_DIR/logo-light.svg"
upload_asset /assets/v1/instance/policy/label/logo/dark "$BRANDING_DIR/logo-dark.svg"
upload_asset /assets/v1/instance/policy/label/icon "$BRANDING_DIR/icon-light.svg"
upload_asset /assets/v1/instance/policy/label/icon/dark "$BRANDING_DIR/icon-dark.svg"

actual=$request_dir/actual.json
read_request /admin/v1/policies/label >"$actual"
if ! active_matches "$actual"; then
  if ! write_request POST /admin/v1/policies/label/_activate --header 'Content-Type: application/json' --data '{}' >"$mutation_response"; then
    sed -n '1,20p' "$mutation_response" >&2
    die "branding activation failed"
  fi
  read_request /admin/v1/policies/label >"$actual"
fi
active_matches "$actual" || die "active branding does not match the reviewed Blazn policy"

printf 'blazn identity branding: active\n'
