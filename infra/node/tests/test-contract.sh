#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
M2_ROOT=$(CDPATH='' cd -- "$NODE_ROOT/../milestone-2" && pwd)
compose=$M2_ROOT/compose.yaml

# The final literal intentionally asserts required Compose interpolation.
# shellcheck disable=SC2016
for expected in \
  'node-migration-preflight:' \
  'entrypoint: ["/bin/sh", "/opt/blazn-node/verify-database.sh", "pre-migration"]' \
  'node-broker-verify:' \
  'entrypoint: ["/bin/sh", "/opt/blazn-node/verify-database.sh", "post-migration"]' \
  'NODE_BROKER_DATABASE_URL_FILE: /run/secrets/node_broker_database_url' \
  'condition: service_completed_successfully' \
  'file: ${BLAZN_NODE_BROKER_SECRETS_ROOT:?set BLAZN_NODE_BROKER_SECRETS_ROOT}/database-url'; do
  grep -F "$expected" "$compose" >/dev/null || { printf 'Node Compose prerequisite is missing: %s\n' "$expected" >&2; exit 1; }
done

api_migrate=$(awk '/^  api-migrate:$/ {p=1; next} p && /^  [a-z]/ {exit} p {print}' "$compose")
api_bootstrap=$(awk '/^  api-bootstrap:$/ {p=1; next} p && /^  [a-z]/ {exit} p {print}' "$compose")
api_runtime=$(awk '/^  api:$/ {p=1; next} p && /^  [a-z]/ {exit} p {print}' "$compose")
printf '%s\n' "$api_migrate" | grep -F 'node-migration-preflight:' >/dev/null
printf '%s\n' "$api_bootstrap" | grep -F 'node-broker-verify:' >/dev/null
for service_body in "$api_migrate" "$api_bootstrap" "$api_runtime"; do
  for forbidden in node_broker_database_url enrollment-hmac-v1 join-credential-v1; do
    if printf '%s\n' "$service_body" | grep -F "$forbidden" >/dev/null; then
      printf 'API lifecycle service receives a Node broker secret: %s\n' "$forbidden" >&2
      exit 1
    fi
  done
done
if grep -F 'enrollment-hmac-v1' "$compose" >/dev/null || grep -F 'join-credential-v1' "$compose" >/dev/null; then
  printf 'broker cryptographic keys are mounted before a broker service exists\n' >&2
  exit 1
fi

grep -F 'CREATE ROLE blazn_node_broker' "$M2_ROOT/postgres-init/01-roles.sh" >/dev/null
grep -F 'GRANT CONNECT ON DATABASE :"database_name" TO blazn_node_broker' "$M2_ROOT/postgres-init/01-roles.sh" >/dev/null
grep -F 'GRANT USAGE ON SCHEMA public TO blazn_node_broker' "$M2_ROOT/postgres-init/01-roles.sh" >/dev/null
grep -F 'nodeBroker' "$M2_ROOT/ownership-receipt.schema.json" >/dev/null
grep -F 'nodeBrokerReceiptDigest' "$M2_ROOT/scripts/backup.sh" >/dev/null
grep -F 'migration 004 is applied; automatic prerequisite rollback is forbidden' "$NODE_ROOT/scripts/rollback-control-plane.sh" >/dev/null

for script in "$NODE_ROOT"/scripts/*.sh "$NODE_ROOT"/tests/*.sh; do sh -n "$script"; done
jq empty "$NODE_ROOT"/*.schema.json "$M2_ROOT/ownership-receipt.schema.json"
printf 'Node infrastructure contract tests passed\n'
