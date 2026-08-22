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
  'file: ${BLAZN_NODE_BROKER_SECRETS_ROOT:?set BLAZN_NODE_BROKER_SECRETS_ROOT}/database-url' \
  'node-plan-verify:' \
  'NODE_PLAN_SIGNING_PRIVATE_KEY_FILE: /run/secrets/node_plan_signing_private_key_v1' \
  'NODE_PLAN_SIGNING_KEY_ID: control-plane-node-plan/v1' \
  'NODE_INSTALL_PLAN_TEMPLATE_FILE: /opt/blazn-node/node-install-plan-template-v1.json' \
  'file: ${BLAZN_NODE_PLAN_ROOT:?set BLAZN_NODE_PLAN_ROOT}/signing-private-v1.b64url'; do
  grep -F "$expected" "$compose" >/dev/null || { printf 'Node Compose prerequisite is missing: %s\n' "$expected" >&2; exit 1; }
done

for service in postgres object object-client object-init api-migrate node-migration-preflight node-broker-verify api-bootstrap poc-identity-provision poc-identity-cleanup poc-identity-verify-cleanup; do
  body=$(awk -v marker="  $service:" '$0==marker {p=1; next} p && /^  [a-z]/ {exit} p {print}' "$compose")
  if printf '%s\n' "$body" | grep -E 'node_plan_signing_private_key_v1|signing-private-v1' >/dev/null; then
    printf 'Node plan signing key reaches unapproved service: %s\n' "$service" >&2
    exit 1
  fi
done
for service in api node-plan-verify; do
  body=$(awk -v marker="  $service:" '$0==marker {p=1; next} p && /^  [a-z]/ {exit} p {print}' "$compose")
  printf '%s\n' "$body" | grep -F 'node_plan_signing_private_key_v1' >/dev/null || { printf 'approved plan-signing service lacks key: %s\n' "$service" >&2; exit 1; }
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
grep -F 'Node migrations are applied; automatic prerequisite rollback is forbidden' "$NODE_ROOT/scripts/rollback-control-plane.sh" >/dev/null
grep -F 'REASSIGN OWNED BY blazn_node_broker TO blazn_migration' "$NODE_ROOT/scripts/rollback-control-plane.sh" >/dev/null
for service in node-migration-preflight node-broker-verify; do
  body=$(awk -v marker="  $service:" '$0==marker {p=1; next} p && /^  [a-z]/ {exit} p {print}' "$compose")
  printf '%s\n' "$body" | grep -F 'user: "999:999"' >/dev/null
  printf '%s\n' "$body" | grep -F 'read_only: true' >/dev/null
done

for script in "$NODE_ROOT"/scripts/*.sh "$NODE_ROOT"/tests/*.sh; do sh -n "$script"; done
jq empty "$NODE_ROOT"/*.schema.json "$NODE_ROOT"/templates/*.json "$M2_ROOT/ownership-receipt.schema.json"
node_schema_id=$(jq -er '."$id"' "$NODE_ROOT/node-broker-receipt.schema.json")
[ "$(jq -er '.properties.nodeBroker."$ref"' "$M2_ROOT/ownership-receipt.schema.json")" = "$node_schema_id" ]
[ "$(jq -er '.properties.nodeBroker."$ref"' "$NODE_ROOT/node-broker-upgrade-receipt.schema.json")" = "$node_schema_id" ]
plan_schema_id=$(jq -er '."$id"' "$NODE_ROOT/node-plan-material-receipt.schema.json")
[ "$(jq -er '.properties.nodePlan."$ref"' "$M2_ROOT/ownership-receipt.schema.json")" = "$plan_schema_id" ]
[ "$(jq -er '.properties.nodePlan."$ref"' "$NODE_ROOT/node-broker-upgrade-receipt.schema.json")" = "$plan_schema_id" ]
python3 "$TEST_DIR/test-schemas.py"
python3 "$TEST_DIR/test-template-semantics.py"
printf 'Node infrastructure contract tests passed\n'
