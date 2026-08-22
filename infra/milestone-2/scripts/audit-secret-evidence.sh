#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "secret evidence audit must run as root"
require_command docker
require_command jq
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
load_control_api_image "$ROOT_DIR"

audit=$(mktemp /tmp/blazn-secret-audit.XXXXXX)
case "$audit" in /tmp/blazn-secret-audit.*) ;; *) die "unsafe audit path" ;; esac
cleanup() { [ ! -e "$audit" ] || unlink "$audit"; }
trap cleanup EXIT HUP INT TERM

{
  journalctl -u blazn-control-plane.service -u blazn-ngrok-qualification.service --no-pager
  docker compose -f "$ROOT_DIR/compose.yaml" --env-file /etc/blazn/control-plane/control-plane.env logs --no-color
  ps axeww
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

ngrok_token=$(awk '$1 == "authtoken:" { print $2; exit }' /etc/blazn/ngrok/ngrok.yml)
assert_absent "$ngrok_token" "ngrok authtoken"

cleanup
trap - EXIT HUP INT TERM
printf 'secret evidence audit passed\n'
