#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

MODE=plan
case ${1:-} in
  '') ;;
  --plan) MODE=plan ;;
  --deploy) MODE=deploy ;;
  --existing-deploy) MODE=existing-deploy ;;
  *) die "usage: preflight.sh [--plan|--deploy|--existing-deploy]" ;;
esac

DATA_ROOT=${BLAZN_DATA_ROOT:-/srv/frontro/blazn-poc/control-plane}
BACKUP_ROOT=${BLAZN_BACKUP_ROOT:-}
SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
NODE_SECRETS_ROOT=${BLAZN_NODE_BROKER_SECRETS_ROOT:-/etc/blazn/node-broker/secrets}
NODE_PLAN_ROOT=${BLAZN_NODE_PLAN_ROOT:-/etc/blazn/node-plan}
RECEIPT_PATH=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
BIND_ADDRESS=${BLAZN_BIND_ADDRESS:-127.0.0.1}
MIN_DATA_BYTES=${BLAZN_MIN_DATA_BYTES:-42949672960}
MIN_BACKUP_BYTES=${BLAZN_MIN_BACKUP_BYTES:-21474836480}
MIN_FREE_INODES=${BLAZN_MIN_FREE_INODES:-100000}

[ -n "$BACKUP_ROOT" ] || die "BLAZN_BACKUP_ROOT must identify a separate backup destination"
[ "$BIND_ADDRESS" = 127.0.0.1 ] || die "BLAZN_BIND_ADDRESS must remain 127.0.0.1 for the POC"

for named_path in \
  "BLAZN_DATA_ROOT:$DATA_ROOT" \
  "BLAZN_BACKUP_ROOT:$BACKUP_ROOT" \
  "BLAZN_SECRETS_ROOT:$SECRETS_ROOT" \
  "BLAZN_NODE_BROKER_SECRETS_ROOT:$NODE_SECRETS_ROOT" \
  "BLAZN_NODE_PLAN_ROOT:$NODE_PLAN_ROOT" \
  "BLAZN_RECEIPT_PATH:$RECEIPT_PATH"; do
  name=${named_path%%:*}
  value=${named_path#*:}
  require_absolute_path "$name" "$value"
  assert_not_symlink_chain "$value"
done

case "$DATA_ROOT" in
  /srv/frontro/blazn-poc/control-plane|/srv/frontro/blazn-poc/control-plane/*) ;;
  *) die "BLAZN_DATA_ROOT is outside the reviewed /srv/frontro/blazn-poc/control-plane boundary" ;;
esac

[ "$DATA_ROOT" != "$BACKUP_ROOT" ] || die "data and backup roots must differ"
case "$BACKUP_ROOT/" in
  "$DATA_ROOT/"*) die "backup root must not be inside the data root" ;;
esac
case "$DATA_ROOT/" in
  "$BACKUP_ROOT/"*) die "data root must not be inside the backup root" ;;
esac
assert_approved_backup_mount "$BACKUP_ROOT"

for number in "$MIN_DATA_BYTES" "$MIN_BACKUP_BYTES" "$MIN_FREE_INODES"; do
  is_uint "$number" || die "capacity thresholds must be unsigned integers"
done

data_device=$(filesystem_device "$DATA_ROOT")
backup_device=$(filesystem_device "$BACKUP_ROOT")
[ "$data_device" != "$backup_device" ] || die "backup and data roots resolve to the same filesystem; this cannot satisfy the isolated-backup gate"

data_bytes=$(available_bytes "$DATA_ROOT")
backup_bytes=$(available_bytes "$BACKUP_ROOT")
data_inodes=$(available_inodes "$DATA_ROOT")
backup_inodes=$(available_inodes "$BACKUP_ROOT")
[ "$data_bytes" -ge "$MIN_DATA_BYTES" ] || die "data filesystem has $data_bytes bytes free; require $MIN_DATA_BYTES"
[ "$backup_bytes" -ge "$MIN_BACKUP_BYTES" ] || die "backup filesystem has $backup_bytes bytes free; require $MIN_BACKUP_BYTES"
[ "$data_inodes" -ge "$MIN_FREE_INODES" ] || die "data filesystem has $data_inodes inodes free; require $MIN_FREE_INODES"
[ "$backup_inodes" -ge "$MIN_FREE_INODES" ] || die "backup filesystem has $backup_inodes inodes free; require $MIN_FREE_INODES"

ports="${POSTGRES_PORT:-55432} ${S3_PORT:-59000} ${S3_CONSOLE_PORT:-59001} ${API_PORT:-58080}"
seen=' '
for port in $ports; do
  is_uint "$port" || die "invalid port: $port"
  if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    die "port is out of range: $port"
  fi
  case "$seen" in
    *" $port "*) die "duplicate port in control-plane plan: $port" ;;
  esac
  seen="$seen$port "
done

if command -v ss >/dev/null 2>&1; then
  listeners=$(ss -H -ltn 2>/dev/null || true)
  for port in $ports; do
    if printf '%s\n' "$listeners" | awk -v port="$port" '$4 ~ (":" port "$") { found=1 } END { exit !found }'; then
      [ "$MODE" = existing-deploy ] || die "TCP port is already in use: $port"
    fi
  done
elif [ "$MODE" != plan ]; then
  die "required command is unavailable: ss"
fi

if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files --no-legend 2>/dev/null | awk '{print $1}' | grep -Fx 'blazn-control-plane.service' >/dev/null 2>&1; then
  [ -f "$RECEIPT_PATH" ] || die "blazn-control-plane.service exists without the ownership receipt"
fi

for image in "${POSTGRES_IMAGE:-}" "${MINIO_IMAGE:-}" "${MINIO_MC_IMAGE:-}"; do
  case "$image" in
    *@sha256:????????????????????????????????????????????????????????????????) ;;
    *) die "all infrastructure images must use an immutable sha256 digest" ;;
  esac
done

if [ "$MODE" != plan ]; then
  [ "$(id -u)" -eq 0 ] || die "deploy preflight must run as root"
  require_command docker
  require_command jq
  require_command sha256sum
  require_command cmp
  load_control_api_image "$ROOT_DIR"
  control_api_build_receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
  control_api_source=$(jq -er .sourceDigest "$control_api_build_receipt")
  control_api_image=$(jq -er .image "$control_api_build_receipt")
  control_api_image_id=$(jq -er .imageId "$control_api_build_receipt")
  validate_workspace_invitation_secret "$SECRETS_ROOT/workspace-invitation-hmac-v1"
  workspace_invitation_hmac_digest=sha256:$(sha256_file "$SECRETS_ROOT/workspace-invitation-hmac-v1")
  [ "${PUBLIC_URL:-}" = https://blazn.benpelo.com ] || die "PUBLIC_URL must be https://blazn.benpelo.com for the live POC deployment"
  [ -n "${BLAZN_INITIAL_LOGIN:-}" ] || die "BLAZN_INITIAL_LOGIN is required"
  [ "${BLAZN_INITIAL_LOGIN:-}" != admin@example.invalid ] || die "the placeholder BLAZN_INITIAL_LOGIN is forbidden for deployment"
  [ -n "${BLAZN_INITIAL_DISPLAY_NAME:-}" ] || die "BLAZN_INITIAL_DISPLAY_NAME is required"
  DOCKER_CONFIG=${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli} docker compose version >/dev/null 2>&1 || die "Blazn-owned Docker Compose v2 is unavailable"
  require_command flock
  assert_directory_owned_mode "$DATA_ROOT" 0 700,2700
  assert_directory_owned_mode "$DATA_ROOT/postgres" 999 700
  assert_directory_owned_mode "$DATA_ROOT/objects" 1000 700,2700
  assert_directory_owned_mode "$SECRETS_ROOT" 0 700
  assert_regular_file_owned_mode "$RECEIPT_PATH" 0 600
  config_digest=sha256:$(control_plane_config_digest "$ROOT_DIR")
  jq -e \
    --arg host "$(hostname)" \
    --arg data "$DATA_ROOT" \
    --arg backup "$BACKUP_ROOT" \
    --arg secrets "$SECRETS_ROOT" \
    --arg backupMount "$BLAZN_BACKUP_MOUNT" \
    --arg backupSource "$BLAZN_BACKUP_SOURCE" \
    --arg backupFstype "$BLAZN_BACKUP_FSTYPE" \
    --arg configDigest "$config_digest" \
    --arg controlApiSource "$control_api_source" \
    --arg controlApiImage "$control_api_image" \
    --arg controlApiImageId "$control_api_image_id" \
    --arg workspaceInvitationHmacDigest "$workspace_invitation_hmac_digest" \
    --arg postgresImage "$POSTGRES_IMAGE" \
    --arg minioImage "$MINIO_IMAGE" \
    --arg minioMcImage "$MINIO_MC_IMAGE" \
    --argjson postgresPort "${POSTGRES_PORT:-55432}" \
    --argjson s3Port "${S3_PORT:-59000}" \
    --argjson s3ConsolePort "${S3_CONSOLE_PORT:-59001}" \
    --argjson apiPort "${API_PORT:-58080}" \
    '.schemaVersion == "blazn.dev/control-plane-ownership/v1" and
     .owner == "blazn-poc" and .host == $host and
     .paths == {data:$data,backup:$backup,secrets:$secrets} and
     .backupMount == {target:$backupMount,source:$backupSource,fstype:$backupFstype} and
     .controlApi == {sourceDigest:$controlApiSource,image:$controlApiImage,imageId:$controlApiImageId} and
     .secretDigests == {"workspace-invitation-hmac-v1":$workspaceInvitationHmacDigest} and
     .ports == [$postgresPort,$s3Port,$s3ConsolePort,$apiPort] and
     .images == [$postgresImage,$minioImage,$minioMcImage] and
     .configDigest == $configDigest' \
    "$RECEIPT_PATH" >/dev/null || die "ownership receipt does not match the requested deployment"
  for secret in postgres-password migration-database-url bootstrap-database-url runtime-database-url initial-password s3-root-access-key s3-root-secret-key s3-runtime-access-key s3-runtime-secret-key proxy-auth-secret workspace-invitation-hmac-v1; do
    assert_regular_file_owned_mode "$SECRETS_ROOT/$secret" 0 444
  done
  installed_unit=${BLAZN_SYSTEMD_UNIT_PATH:-/etc/systemd/system/blazn-control-plane.service}
  assert_regular_file_owned_mode "$installed_unit" 0 644
  cmp -s "$ROOT_DIR/systemd/blazn-control-plane.service" "$installed_unit" || die "installed control-plane systemd unit differs from the active release"
  if [ "$MODE" = existing-deploy ]; then
    ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
    assert_regular_file_owned_mode "$ENV_FILE" 0 600
    verify_control_api_containers "$ROOT_DIR" "$ENV_FILE"
    for service in postgres object api; do
      container=$(docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" ps -q "$service")
      [ -n "$container" ] || die "live service has no running container: $service"
      identity=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}' "$container")
      [ "$identity" = "blazn-m2/$service" ] || die "live container identity is not receipt-scoped: $service"
      state=$(docker inspect --format '{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container")
      [ "$state" = running/healthy ] || die "live service is not healthy: $service ($state)"
    done
    for binding in "postgres:5432:${POSTGRES_PORT:-55432}" "object:9000:${S3_PORT:-59000}" "object:9001:${S3_CONSOLE_PORT:-59001}" "api:8080:${API_PORT:-58080}"; do
      service=${binding%%:*}
      remainder=${binding#*:}
      container_port=${remainder%%:*}
      host_port=${remainder##*:}
      actual=$(docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" port "$service" "$container_port")
      [ "$actual" = "127.0.0.1:$host_port" ] || die "live service has an unexpected published binding: $service ($actual)"
    done
  fi
  [ "$NODE_SECRETS_ROOT" = /etc/blazn/node-broker/secrets ] || die "node broker secrets root differs from the reviewed path"
  assert_directory_owned_mode /etc/blazn/node-broker 0 700
  assert_directory_owned_mode "$NODE_SECRETS_ROOT" 0 700
  assert_regular_file_owned_mode "$NODE_SECRETS_ROOT/database-url" 0 444
  assert_regular_file_owned_mode "$NODE_SECRETS_ROOT/enrollment-hmac-v1" 0 400
  assert_regular_file_owned_mode "$NODE_SECRETS_ROOT/join-credential-v1" 0 400
  [ "$NODE_PLAN_ROOT" = /etc/blazn/node-plan ] || die "node plan material root differs from the reviewed path"
  node_plan_journal=$(jq -er .nodePlan.creationJournal.path "$RECEIPT_PATH")
  case "$node_plan_journal" in /var/lib/blazn/ownership/node-plan-material-create.json|/var/lib/blazn/ownership/node-plan-material-upgrade-create.json) ;; *) die "ownership receipt has an unreviewed Node plan journal" ;; esac
  assert_regular_file_owned_mode "$node_plan_journal" 0 600
  node_plan_json=$(BLAZN_NODE_PLAN_ROOT="$NODE_PLAN_ROOT" BLAZN_NODE_PLAN_CREATE_JOURNAL="$node_plan_journal" "$ROOT_DIR/../node/scripts/plan-material-object.sh")
  [ "$(printf '%s' "$node_plan_json" | jq -cS .)" = "$(jq -cS .nodePlan "$RECEIPT_PATH")" ] || die "ownership receipt does not bind the Node plan material"
  node_creation_journal=$(jq -er .nodeBroker.creationJournal.path "$RECEIPT_PATH")
  case "$node_creation_journal" in /var/lib/blazn/ownership/node-broker-secret-create.json|/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json) ;; *) die "ownership receipt has an unreviewed Node secret-creation journal" ;; esac
  assert_regular_file_owned_mode "$node_creation_journal" 0 600
  jq -e \
    --arg nodeDatabaseDigest "sha256:$(sha256_file "$NODE_SECRETS_ROOT/database-url")" \
    --arg nodeEnrollmentDigest "sha256:$(sha256_file "$NODE_SECRETS_ROOT/enrollment-hmac-v1")" \
    --arg nodeJoinDigest "sha256:$(sha256_file "$NODE_SECRETS_ROOT/join-credential-v1")" \
    --arg nodeCreationJournal "$node_creation_journal" \
    --arg nodeCreationJournalDigest "sha256:$(sha256_file "$node_creation_journal")" \
    '.nodeBroker == {schemaVersion:"blazn.dev/node-broker-infra/v1",secretsRoot:"/etc/blazn/node-broker/secrets",databaseRole:"blazn_node_broker",keyIds:{enrollment:"node-enrollment/v1",joinCredential:"node-join-credential/v1"},digests:{"database-url":$nodeDatabaseDigest,"enrollment-hmac-v1":$nodeEnrollmentDigest,"join-credential-v1":$nodeJoinDigest},creationJournal:{path:$nodeCreationJournal,digest:$nodeCreationJournalDigest}}' \
    "$RECEIPT_PATH" >/dev/null || die "ownership receipt does not bind the Node broker prerequisites"
  broker_mode=$(sed -n 's/^BLAZN_NODE_BROKER_LOOPBACK=//p' "$ENV_FILE"); [ -n "$broker_mode" ] || broker_mode=disabled
  if [ "$broker_mode" = enabled ]; then
    issuer_receipt=/var/lib/blazn/ownership/microk8s-worker-issuer.json
    assert_regular_file_owned_mode "$issuer_receipt" 0 600
    jq -e '.schemaVersion=="blazn.dev/microk8s-worker-issuer-infra/v1" and .phase=="complete" and .liveJoinBlocked==true' "$issuer_receipt" >/dev/null || die "issuer receipt is not complete and blocked"
    issuer_material=$(jq -cS '{binary,config,unit,tmpfiles,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$issuer_receipt")
    issuer_digest=sha256:$(printf '%s' "$issuer_material" | sha256sum | awk '{print $1}')
    jq -e --arg digest "$issuer_digest" '.microk8sIssuer=={receiptPath:"/var/lib/blazn/ownership/microk8s-worker-issuer.json",materialDigest:$digest}' "$RECEIPT_PATH" >/dev/null || die "main ownership receipt does not bind issuer material"
    assert_regular_file_owned_mode /usr/libexec/blazn/blazn-microk8s-worker-issuer 0 755
    [ "sha256:$(sha256_file /usr/libexec/blazn/blazn-microk8s-worker-issuer)" = "$(jq -er .binary.digest "$issuer_receipt")" ] || die "issuer binary differs from receipt"
    if [ ! -S /run/blazn/microk8s-worker-issuer.sock ] || [ -L /run/blazn/microk8s-worker-issuer.sock ]; then die "issuer socket is unavailable"; fi
  elif [ "$broker_mode" != disabled ]; then die "Node broker loopback mode is invalid or duplicated"; fi
fi

printf '{"status":"ok","mode":"%s","bindAddress":"%s","ports":[%s,%s,%s,%s],"dataBytesFree":%s,"backupBytesFree":%s,"dataInodesFree":%s,"backupInodesFree":%s,"separateFilesystem":true}\n' \
  "$MODE" "$BIND_ADDRESS" \
  "${POSTGRES_PORT:-55432}" "${S3_PORT:-59000}" "${S3_CONSOLE_PORT:-59001}" "${API_PORT:-58080}" \
  "$data_bytes" "$backup_bytes" "$data_inodes" "$backup_inodes"
