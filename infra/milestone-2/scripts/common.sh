#!/bin/sh
set -eu

die() {
  printf 'blazn-m2: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

require_absolute_path() {
  case "$2" in
    /*) ;;
    *) die "$1 must be an absolute path" ;;
  esac
}

nearest_existing_parent() {
  candidate=$1
  while [ ! -e "$candidate" ]; do
    next=$(dirname -- "$candidate")
    [ "$next" != "$candidate" ] || die "cannot resolve an existing parent for $1"
    candidate=$next
  done
  printf '%s\n' "$candidate"
}

assert_not_symlink_chain() {
  candidate=$1
  while [ "$candidate" != / ]; do
    if [ -L "$candidate" ]; then
      die "path contains a symbolic link: $candidate"
    fi
    candidate=$(dirname -- "$candidate")
  done
}

assert_directory_owned_mode() {
  path=$1
  expected_uid=$2
  expected_modes=$3
  if [ ! -d "$path" ] || [ -L "$path" ]; then
    die "expected a non-symlink directory: $path"
  fi
  [ "$(stat -c '%u' "$path")" = "$expected_uid" ] || die "directory has unexpected owner: $path"
  actual_mode=$(stat -c '%a' "$path")
  case ",$expected_modes," in
    *,"$actual_mode",*) ;;
    *) die "directory has unexpected mode: $path" ;;
  esac
}

assert_regular_file_owned_mode() {
  path=$1
  expected_uid=$2
  expected_mode=$3
  if [ ! -f "$path" ] || [ -L "$path" ]; then
    die "expected a non-symlink regular file: $path"
  fi
  [ "$(stat -c '%u' "$path")" = "$expected_uid" ] || die "file has unexpected owner: $path"
  [ "$(stat -c '%a' "$path")" = "$expected_mode" ] || die "file has unexpected mode: $path"
}

assert_approved_backup_mount() {
  backup_root=$1
  backup_mount=${BLAZN_BACKUP_MOUNT:-}
  backup_source=${BLAZN_BACKUP_SOURCE:-}
  backup_fstype=${BLAZN_BACKUP_FSTYPE:-}
  if [ -z "$backup_mount" ] || [ -z "$backup_source" ] || [ -z "$backup_fstype" ]; then
    die "BLAZN_BACKUP_MOUNT, BLAZN_BACKUP_SOURCE, and BLAZN_BACKUP_FSTYPE are required"
  fi
  require_absolute_path BLAZN_BACKUP_MOUNT "$backup_mount"
  require_command findmnt
  require_command realpath
  assert_not_symlink_chain "$backup_mount"
  if [ ! -d "$backup_mount" ] || [ -L "$backup_mount" ]; then
    die "approved backup mountpoint is unavailable: $backup_mount"
  fi
  canonical_mount=$(realpath -e "$backup_mount")
  canonical_root=$(realpath -m "$backup_root")
  case "$canonical_root" in
    "$canonical_mount"/*) ;;
    *) die "backup root is outside the approved mountpoint" ;;
  esac
  mount_record=$(findmnt -rn --mountpoint "$canonical_mount" -o TARGET,SOURCE,FSTYPE) || \
    die "approved backup mountpoint is not actively mounted: $canonical_mount"
  actual_target=
  actual_source=
  actual_fstype=
  extra=
  IFS=' ' read -r actual_target actual_source actual_fstype extra <<EOF
$mount_record
EOF
  if [ -z "$actual_target" ] || [ -z "$actual_source" ] || [ -z "$actual_fstype" ] || [ -n "$extra" ]; then
    die "approved backup mount record is ambiguous"
  fi
  [ "$actual_target" = "$canonical_mount" ] || die "backup mount target does not match the approved mountpoint"
  [ "$actual_source" = "$backup_source" ] || die "backup mount source does not match the approved source"
  [ "$actual_fstype" = "$backup_fstype" ] || die "backup mount filesystem type does not match the approved type"
}

filesystem_device() {
  existing=$(nearest_existing_parent "$1")
  stat -c '%d' "$existing"
}

available_bytes() {
  existing=$(nearest_existing_parent "$1")
  df -PB1 "$existing" | awk 'NR == 2 { print $4 }'
}

available_inodes() {
  existing=$(nearest_existing_parent "$1")
  df -Pi "$existing" | awk 'NR == 2 { print $4 }'
}

is_uint() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
    *) return 0 ;;
  esac
}

sha256_file() {
  sha256sum "$1" | awk '{ print $1 }'
}

validate_workspace_invitation_secret() {
  secret_file=$1
  assert_regular_file_owned_mode "$secret_file" 0 444
  [ "$(wc -c <"$secret_file" | tr -d ' ')" = 65 ] || \
    die "workspace invitation HMAC key must contain exactly 64 lowercase hexadecimal characters and one newline"
  [ "$(wc -l <"$secret_file" | tr -d ' ')" = 1 ] || \
    die "workspace invitation HMAC key must contain exactly one line"
  LC_ALL=C grep -Eq '^[a-f0-9]{64}$' "$secret_file" || \
    die "workspace invitation HMAC key must contain exactly 64 lowercase hexadecimal characters"
}

identity_overlay_enabled() {
  case ${BLAZN_IDENTITY_ENABLED:-false} in
    true) return 0 ;;
    false) return 1 ;;
    *) die "BLAZN_IDENTITY_ENABLED must be true or false" ;;
  esac
}

control_plane_compose() {
  infra_root=$1
  env_file=$2
  shift 2
  if identity_overlay_enabled; then
    identity_env=${BLAZN_IDENTITY_ENV_FILE:-/etc/blazn/identity/control-api.env}
    require_absolute_path BLAZN_IDENTITY_ENV_FILE "$identity_env"
    assert_not_symlink_chain "$identity_env"
    assert_regular_file_owned_mode "$identity_env" 0 600
    [ -f "$infra_root/compose.identity.yaml" ] && [ ! -L "$infra_root/compose.identity.yaml" ] || \
      die "identity Compose overlay is unavailable"
    docker compose -f "$infra_root/compose.yaml" -f "$infra_root/compose.identity.yaml" \
      --env-file "$env_file" --env-file "$identity_env" "$@"
  else
    docker compose -f "$infra_root/compose.yaml" --env-file "$env_file" "$@"
  fi
}

validate_identity_overlay() {
  infra_root=$1
  identity_overlay_enabled || return 0
  identity_env=${BLAZN_IDENTITY_ENV_FILE:-/etc/blazn/identity/control-api.env}
  require_absolute_path BLAZN_IDENTITY_ENV_FILE "$identity_env"
  assert_not_symlink_chain "$identity_env"
  assert_regular_file_owned_mode "$identity_env" 0 600
  [ -f "$infra_root/compose.identity.yaml" ] && [ ! -L "$infra_root/compose.identity.yaml" ] || \
    die "identity Compose overlay is unavailable"
  identity_size=$(wc -c <"$identity_env" | tr -d ' ')
  case $identity_size in ''|*[!0-9]*) die "identity environment size is invalid" ;; esac
  [ "$identity_size" -le 8192 ] || die "identity environment is unexpectedly large"
  if LC_ALL=C grep -Ev '^[A-Z][A-Z0-9_]*=[a-zA-Z0-9._@:/+-]+[[:space:]]*$|^[[:space:]]*(#.*)?$' "$identity_env" | grep . >/dev/null; then
    die "identity environment contains unsupported syntax"
  fi
  identity_root=$(sed -n 's/^BLAZN_IDENTITY_SECRETS_ROOT=//p' "$identity_env")
  identity_issuer=$(sed -n 's/^ZITADEL_ISSUER_URL=//p' "$identity_env")
  identity_client=$(sed -n 's/^ZITADEL_CLIENT_ID=//p' "$identity_env")
  identity_mfa=$(sed -n 's/^ZITADEL_REQUIRE_MFA=//p' "$identity_env")
  [ "$identity_root" = /etc/blazn/identity/secrets ] || die "identity secrets root differs from the reviewed path"
  [ "$identity_issuer" = https://auth.blazn.benpelo.com ] || die "ZITADEL issuer differs from the reviewed public origin"
  case $identity_client in ''|*[!0-9]*) die "ZITADEL client ID is invalid" ;; esac
  [ "$identity_mfa" = true ] || die "ZITADEL MFA enforcement must remain enabled"
  assert_directory_owned_mode /etc/blazn/identity 0 700
  assert_directory_owned_mode "$identity_root" 0 700
  assert_regular_file_owned_mode "$identity_root/zitadel-client-secret" 0 600
  assert_regular_file_owned_mode "$identity_root/oidc-cookie-key" 0 600
  client_secret_size=$(wc -c <"$identity_root/zitadel-client-secret" | tr -d ' ')
  case $client_secret_size in ''|*[!0-9]*) die "ZITADEL client secret size is invalid" ;; esac
  [ "$client_secret_size" -ge 16 ] && [ "$client_secret_size" -le 1024 ] || die "ZITADEL client secret size is invalid"
  [ "$(wc -c <"$identity_root/oidc-cookie-key" | tr -d ' ')" = 43 ] || die "OIDC cookie key must contain 32 base64url bytes"
  LC_ALL=C grep -Eq '^[A-Za-z0-9_-]{43}$' "$identity_root/oidc-cookie-key" || die "OIDC cookie key is invalid"
}

control_api_source_digest() {
  infra_root=$1
  repo_root=$(CDPATH='' cd -- "$infra_root/../.." && pwd)
  for required_input in \
    services/control-api/Dockerfile \
    services/control-api/package.json \
    services/control-api/package-lock.json \
    services/control-api/tsconfig.json \
    infra/milestone-2/scripts/build-control-api.sh \
    infra/milestone-2/scripts/common.sh; do
    if [ ! -f "$repo_root/$required_input" ] || [ -L "$repo_root/$required_input" ]; then
      die "control API build input is missing or symlinked: $required_input"
    fi
  done
  for required_dir in services/control-api/src services/control-api/migrations packages/contracts; do
    if [ ! -d "$repo_root/$required_dir" ] || [ -L "$repo_root/$required_dir" ]; then
      die "control API build directory is missing or symlinked: $required_dir"
    fi
  done
  if find "$repo_root/services/control-api/src" "$repo_root/services/control-api/migrations" "$repo_root/packages/contracts" -type l -print | grep . >/dev/null; then
    die "control API build tree contains a symbolic link"
  fi
  (
    cd "$repo_root"
    {
      printf '%s\0' \
        services/control-api/Dockerfile \
        services/control-api/package.json \
        services/control-api/package-lock.json \
        services/control-api/tsconfig.json \
        infra/milestone-2/scripts/build-control-api.sh \
        infra/milestone-2/scripts/common.sh
      find services/control-api/src services/control-api/migrations packages/contracts -type f -print0
    } | LC_ALL=C sort -z | xargs -0 sha256sum
  ) | sha256sum | awk '{ print $1 }'
}

validate_control_api_build() {
  infra_root=$1
  build_receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
  assert_regular_file_owned_mode "$build_receipt" 0 600
  source_digest=sha256:$(control_api_source_digest "$infra_root")
  expected_image=blazn-control-api:source-${source_digest#sha256:}
  image_id=$(docker image inspect "$expected_image" --format '{{.Id}}') || die "receipt-bound control API image is unavailable"
  jq -e --arg sourceDigest "$source_digest" --arg image "$expected_image" --arg imageId "$image_id" \
    '.schemaVersion == "blazn.dev/control-api-build/v1" and .image == $image and .sourceDigest == $sourceDigest and .imageId == $imageId' \
    "$build_receipt" >/dev/null || die "control API build receipt does not match source and image"
}

load_control_api_image() {
  infra_root=$1
  validate_control_api_build "$infra_root"
  build_receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
  CONTROL_API_IMAGE=$(jq -er .image "$build_receipt")
  export CONTROL_API_IMAGE
}

verify_control_api_containers() {
  infra_root=$1
  env_file=$2
  expected_id=$(docker image inspect "$CONTROL_API_IMAGE" --format '{{.Id}}') || die "receipt-bound control API image is unavailable"
  for service in api api-migrate api-bootstrap; do
    container=$(control_plane_compose "$infra_root" "$env_file" ps -a -q "$service")
    [ -n "$container" ] || die "control API service has no container: $service"
    identity=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.Image}}' "$container")
    [ "$identity" = "blazn-m2/$service/$expected_id" ] || die "control API service container does not match its receipt: $service"
    state=$(docker inspect --format '{{.State.Status}}/{{.State.ExitCode}}' "$container")
    case "$service:$state" in
      api:running/0|api-migrate:exited/0|api-bootstrap:exited/0) ;;
      *) die "control API service has unexpected state: $service ($state)" ;;
    esac
  done
}

verify_node_prerequisite_containers() {
  infra_root=$1
  env_file=$2
  expected_id=$(docker image inspect "$POSTGRES_IMAGE" --format '{{.Id}}') || die "pinned PostgreSQL image is unavailable"
  for service in node-migration-preflight node-broker-verify; do
    container=$(control_plane_compose "$infra_root" "$env_file" ps -a -q "$service")
    [ -n "$container" ] || die "Node prerequisite service has no container: $service"
    identity=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.Image}}' "$container")
    [ "$identity" = "blazn-m2/$service/$expected_id" ] || die "Node prerequisite container does not match its pinned image: $service"
    [ "$(docker inspect --format '{{.State.Status}}/{{.State.ExitCode}}' "$container")" = exited/0 ] || die "Node prerequisite service did not pass: $service"
  done
}

verify_node_plan_container() {
  infra_root=$1
  env_file=$2
  expected_id=$(docker image inspect "$CONTROL_API_IMAGE" --format '{{.Id}}') || die "receipt-bound control API image is unavailable"
  container=$(control_plane_compose "$infra_root" "$env_file" ps -a -q node-plan-verify)
  [ -n "$container" ] || die "Node plan validation service has no container"
  identity=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.Image}}' "$container")
  [ "$identity" = "blazn-m2/node-plan-verify/$expected_id" ] || die "Node plan validation container does not match its receipt"
  [ "$(docker inspect --format '{{.State.Status}}/{{.State.ExitCode}}' "$container")" = exited/0 ] || die "Node plan validation service did not pass"
}

control_plane_config_digest() {
  root=$1
  (
    cd "$root"
    {
      printf '%s\0' \
        compose.yaml \
        compose.identity.yaml \
        postgres-init/01-roles.sh \
        ngrok.example.yml \
        systemd/blazn-control-plane.service \
        systemd/blazn-ngrok.service \
        systemd/blazn-ngrok-qualification.service
      find . -maxdepth 1 -type f -name '*.schema.json' -print0
      find scripts -maxdepth 1 -type f -name '*.sh' -print0
      find ../node -type f -print0
    } | LC_ALL=C sort -z | xargs -0 sha256sum
    printf 'control-api-source sha256:%s\n' "$(control_api_source_digest "$root")"
  ) | sha256sum | awk '{ print $1 }'
}

verify_versioned_release() {
  release=$1
  receipt=$2
  assert_directory_owned_mode "$release" 0 555
  assert_regular_file_owned_mode "$release/.blazn-release-tree.sha256" 0 444
  assert_regular_file_owned_mode "$receipt" 0 600
  manifest_digest=$(sed -n '1p' "$release/.blazn-release-tree.sha256")
  case $manifest_digest in sha256:????????????????????????????????????????????????????????????????) ;; *) die "versioned release tree digest is invalid" ;; esac
  jq -e --arg path "$release" --arg manifestDigest "$manifest_digest" \
    '.schemaVersion == "blazn.dev/release/v1" and .path == $path and .manifestDigest == $manifestDigest and (.releaseDigest | test("^sha256:[a-f0-9]{64}$"))' \
    "$receipt" >/dev/null || die "versioned release receipt is invalid: $release"
  [ "$manifest_digest" = "sha256:$(release_tree_digest "$release")" ] || die "versioned release tree digest failed: $release"
  expected_source=$(jq -er .controlApiSourceDigest "$receipt")
  expected_config=$(jq -er .controlPlaneConfigDigest "$receipt")
  [ "$expected_source" = "sha256:$(control_api_source_digest "$release/infra/milestone-2")" ] || die "versioned release API/migration source digest changed"
  [ "$expected_config" = "sha256:$(control_plane_config_digest "$release/infra/milestone-2")" ] || die "versioned release control-plane config digest changed"
}

release_tree_digest() (
  release_root_path=$1
  require_command tar
  list=$(mktemp /tmp/blazn-release-list.XXXXXX)
  archive=$(mktemp /tmp/blazn-release-archive.XXXXXX)
  cleanup_release_digest() { unlink "$list" "$archive" 2>/dev/null || true; }
  trap cleanup_release_digest HUP INT TERM
  (
    cd "$release_root_path"
    find . -mindepth 1 ! -name '.blazn-release.json' ! -name '.blazn-release-tree.sha256' -print0 | LC_ALL=C sort -z >"$list"
    tar --null --no-recursion --files-from="$list" --format=posix --pax-option=delete=atime,delete=ctime \
      --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX,go-w' -cf "$archive"
  )
  digest=$(sha256_file "$archive")
  cleanup_release_digest
  trap - HUP INT TERM
  printf '%s\n' "$digest"
)

verify_legacy_release() {
  release=$1
  receipt=$2
  assert_directory_owned_mode "$release" 0 555
  assert_regular_file_owned_mode "$receipt" 0 600
  manifest=$(jq -er .manifestPath "$receipt")
  assert_regular_file_owned_mode "$manifest" 0 600
  manifest_digest=$(sed -n '1p' "$manifest")
  case $manifest_digest in sha256:????????????????????????????????????????????????????????????????) ;; *) die "legacy release tree digest is invalid" ;; esac
  jq -e --arg path "$release" --arg manifestDigest "$manifest_digest" --arg manifestPath "$manifest" \
    '.schemaVersion == "blazn.dev/legacy-release/v1" and .path == $path and .manifestPath == $manifestPath and .manifestDigest == $manifestDigest and (.releaseDigest | test("^sha256:[a-f0-9]{64}$"))' \
    "$receipt" >/dev/null || die "legacy release receipt is invalid: $release"
  [ "$manifest_digest" = "sha256:$(release_tree_digest "$release")" ] || die "legacy release tree digest failed: $release"
}

verify_managed_release() {
  release=$1
  receipt=$2
  schema=$(jq -er .schemaVersion "$receipt")
  case $schema in
    blazn.dev/release/v1) verify_versioned_release "$release" "$receipt" ;;
    blazn.dev/legacy-release/v1) verify_legacy_release "$release" "$receipt" ;;
    *) die "unsupported managed release receipt schema" ;;
  esac
}
