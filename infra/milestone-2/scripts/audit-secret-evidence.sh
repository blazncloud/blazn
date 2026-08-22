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

node_plan_root=${BLAZN_NODE_PLAN_ROOT:-/etc/blazn/node-plan}
assert_directory_owned_mode "$node_plan_root" 0 700
assert_regular_file_owned_mode "$node_plan_root/signing-private-v1.b64url" 0 444
node_plan_private=$(sed -n '1p' "$node_plan_root/signing-private-v1.b64url")
assert_absent "$node_plan_private" "Node plan signing private seed"
node_plan_private_standard=$(printf '%s' "$node_plan_private" | tr '_-' '/+')
node_plan_private_standard_padded=${node_plan_private_standard}=
node_plan_private_hex=$(printf '%s' "$node_plan_private_standard_padded" | base64 -d | od -An -v -tx1 | tr -d ' \n')
assert_absent "$node_plan_private_standard" "Node plan signing private seed standard-base64"
assert_absent "$node_plan_private_standard_padded" "Node plan signing private seed padded-base64"
assert_absent "$node_plan_private_hex" "Node plan signing private seed hex"
for container in $(docker compose -f "$ROOT_DIR/compose.yaml" --env-file /etc/blazn/control-plane/control-plane.env ps -a -q); do
  service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$container")
  has_plan_key=$(docker inspect --format '{{range .Mounts}}{{println .Source}}{{end}}' "$container" | grep -Fx "$node_plan_root/signing-private-v1.b64url" || true)
  if [ -n "$has_plan_key" ]; then
    case "$service" in api|node-plan-verify) ;; *) die "Node plan signing key is mounted into an unapproved service: $service" ;; esac
  fi
done

poc_identity_root=${BLAZN_POC_IDENTITY_ROOT:-/var/lib/blazn/poc-identities/second}
if [ -d "$poc_identity_root" ]; then
  assert_directory_owned_mode "$poc_identity_root" 0 700
  for identity_file in password profile.json; do
    assert_regular_file_owned_mode "$poc_identity_root/$identity_file" 0 444
  done
  assert_absent "$(sed -n '1p' "$poc_identity_root/password")" "POC second identity password"
  assert_absent "$(jq -er .login "$poc_identity_root/profile.json")" "POC second identity login"
  assert_absent "$(jq -er .displayName "$poc_identity_root/profile.json")" "POC second identity display name"
fi
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
