#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
policy=$SCRIPT_DIR/branding/branding.json
installer=$SCRIPT_DIR/apply-branding.sh

sh -n "$installer"
jq -e '
  keys == ["backgroundColor","backgroundColorDark","disableWatermark","fontColor","fontColorDark","hideLoginNameSuffix","primaryColor","primaryColorDark","themeMode","warnColor","warnColorDark"] and
  .primaryColor == "#f97316" and .primaryColorDark == "#fb923c" and
  .backgroundColor == "#fff7ed" and .backgroundColorDark == "#090a0f" and
  .fontColor == "#18181b" and .fontColorDark == "#f8fafc" and
  .hideLoginNameSuffix == true and .disableWatermark == true and
  .themeMode == "THEME_MODE_AUTO"
' "$policy" >/dev/null

for asset in logo-light.svg logo-dark.svg icon-light.svg icon-dark.svg; do
  file=$SCRIPT_DIR/branding/$asset
  [ -s "$file" ]
  [ "$(wc -c <"$file" | tr -d ' ')" -le 262144 ]
  grep -F '<svg' "$file" >/dev/null
  grep -F 'Blazn' "$file" >/dev/null
  if grep -Ei '<script|javascript:|xlink:href|[[:space:]]href=' "$file" >/dev/null; then
    printf 'branding asset contains executable or remote content: %s\n' "$asset" >&2
    exit 1
  fi
done

for required in \
  '/admin/v1/policies/label' \
  '/assets/v1/instance/policy/label/logo' \
  '/assets/v1/instance/policy/label/logo/dark' \
  '/assets/v1/instance/policy/label/icon' \
  '/assets/v1/instance/policy/label/icon/dark' \
  '/admin/v1/policies/label/_activate' \
  '--retry-max-time 30' \
  '--connect-timeout 3' \
  '--max-time 15' \
  'token_size=' \
  'token_size" -gt 4096' \
  "grep -Eq '^[A-Za-z0-9._~-]+" \
  'stat -c '\''%u:%a:%h'\'''; do
  grep -F -- "$required" "$installer" >/dev/null
done

printf 'identity branding contract: ok\n'
