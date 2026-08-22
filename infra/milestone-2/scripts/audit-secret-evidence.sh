#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "secret evidence audit must run as root"
require_command docker
require_command jq
require_command base64
require_command od
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
load_control_api_image "$ROOT_DIR"

audit=$(mktemp /tmp/blazn-secret-audit.XXXXXX)
case "$audit" in /tmp/blazn-secret-audit.*) ;; *) die "unsafe audit path" ;; esac
cleanup() { [ ! -e "$audit" ] || unlink "$audit"; }
trap cleanup EXIT HUP INT TERM

{
  journalctl -u blazn-control-plane.service -u blazn-ngrok-qualification.service --no-pager
  docker compose -f "$ROOT_DIR/compose.yaml" --env-file /etc/blazn/control-plane/control-plane.env logs --no-color
  # Audit command arguments. Container initialization environments are a
  # separate hardening boundary and are not user-facing evidence or argv.
  ps axww
  for receipt in /var/lib/blazn/ownership/*.json; do
    [ -f "$receipt" ] || continue
    printf 'receipt=%s\n' "$receipt"
    sed -n '1,260p' "$receipt"
  done
  sed -n '1,260p' /etc/blazn/control-plane/control-plane.env
} >"$audit" 2>&1

assert_absent() {
  value=$1
  label=$2
  [ -z "$value" ] && return
  if grep -Fq -- "$value" "$audit"; then
    die "secret value appeared in logs, process state, receipts, or nonsecret configuration: $label"
  fi
  if grep -R -I -Fq -- "$value" /opt/blazn; then
    die "secret value appeared in deployed source: $label"
  fi
}

for secret_file in /etc/blazn/control-plane/secrets/*; do
  [ -f "$secret_file" ] || continue
  value=$(sed -n '1p' "$secret_file")
  assert_absent "$value" "$(basename -- "$secret_file")"
  case "$value" in
    postgresql://*:*@*)
      password=${value#*://*:}
      password=${password%%@*}
      assert_absent "$password" "$(basename -- "$secret_file") password"
      ;;
  esac
done

node_secrets=${BLAZN_NODE_BROKER_SECRETS_ROOT:-/etc/blazn/node-broker/secrets}
assert_directory_owned_mode "$node_secrets" 0 700
assert_regular_file_owned_mode "$node_secrets/database-url" 0 444
node_database_url=$(sed -n '1p' "$node_secrets/database-url")
assert_absent "$node_database_url" "Node broker database URL"
node_database_password=${node_database_url#*://*:}
node_database_password=${node_database_password%%@*}
assert_absent "$node_database_password" "Node broker database password"
for key_name in enrollment-hmac-v1 join-credential-v1; do
  key_file=$node_secrets/$key_name
  assert_regular_file_owned_mode "$key_file" 0 400
  key_hex=$(od -An -v -tx1 "$key_file" | tr -d ' \n')
  key_base64=$(base64 -w 0 "$key_file")
  assert_absent "$key_hex" "$key_name hex"
  assert_absent "$key_base64" "$key_name base64"
done

ngrok_token=$(awk '$1 == "authtoken:" { print $2; exit }' /etc/blazn/ngrok/ngrok.yml)
assert_absent "$ngrok_token" "ngrok authtoken"

cleanup
trap - EXIT HUP INT TERM
printf 'secret evidence audit passed\n'
