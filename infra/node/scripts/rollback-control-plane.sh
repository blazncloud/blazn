#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
M2_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/../../milestone-2" && pwd)
# shellcheck disable=SC1091
. "$M2_ROOT/scripts/common.sh"

[ "$(id -u)" -eq 0 ] || die "Node infrastructure rollback must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "Node infrastructure rollback must run through the control-plane lock"
require_command docker
require_command jq
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"

ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
UPGRADE_RECEIPT=${BLAZN_NODE_BROKER_UPGRADE_RECEIPT:-/var/lib/blazn/ownership/node-broker-upgrade.json}
assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
phase=$(jq -er '.phase' "$UPGRADE_RECEIPT")
[ "$phase" = receipt-bound ] || die "rollback requires a receipt-bound Node infrastructure upgrade"
backup=$(jq -er '.mainReceipt.backupPath' "$UPGRADE_RECEIPT")
[ "$(jq -er '.mainReceipt.backupDigest' "$UPGRADE_RECEIPT")" = "sha256:$(sha256_file "$backup")" ] || die "main receipt rollback backup digest changed"

compose() { docker compose -f "$M2_ROOT/compose.yaml" --env-file "$ENV_FILE" "$@"; }
applied=$(compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from schema_migrations where version='004_nodes.sql'")
[ "$applied" = 0 ] || die "migration 004 is applied; automatic prerequisite rollback is forbidden"

node_root=/etc/blazn/node-broker
node_secrets=$node_root/secrets
assert_directory_owned_mode "$node_root" 0 700
assert_directory_owned_mode "$node_secrets" 0 700
for name in database-url enrollment-hmac-v1 join-credential-v1; do
  expected=$(jq -er --arg name "$name" '.nodeBroker.digests[$name]' "$UPGRADE_RECEIPT")
  [ "$expected" = "sha256:$(sha256_file "$node_secrets/$name")" ] || die "installed Node broker secret differs from rollback receipt: $name"
done

retained=/var/lib/blazn/ownership/node-broker-rollback-${BLAZN_CORRELATION_ID:-manual}
case "$retained" in /var/lib/blazn/ownership/node-broker-rollback-[a-zA-Z0-9._-]*) ;; *) die "rollback correlation ID is invalid" ;; esac
[ ! -e "$retained" ] || die "rollback retention target already exists"
printf 'DROP ROLE blazn_node_broker;\n' | compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null
mv -- "$node_root" "$retained"
tmp=$MAIN_RECEIPT.tmp.$$
cp --preserve=mode,timestamps -- "$backup" "$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$MAIN_RECEIPT"
tmp=$UPGRADE_RECEIPT.tmp.$$
jq --arg rolledBackAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --arg retained "$retained" '.phase="rolled-back" | .rolledBackAt=$rolledBackAt | .retainedSecretsPath=$retained' "$UPGRADE_RECEIPT" >"$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$UPGRADE_RECEIPT"
printf 'Node broker prerequisites rolled back; secrets retained recoverably at %s\n' "$retained"
