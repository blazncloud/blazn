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
    container=$(docker compose -f "$infra_root/compose.yaml" --env-file "$env_file" ps -a -q "$service")
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

control_plane_config_digest() {
  root=$1
  (
    cd "$root"
    {
      printf '%s\0' \
        compose.yaml \
        postgres-init/01-roles.sh \
        ngrok.example.yml \
        systemd/blazn-control-plane.service \
        systemd/blazn-ngrok.service \
        systemd/blazn-ngrok-qualification.service
      find . -maxdepth 1 -type f -name '*.schema.json' -print0
      find scripts -maxdepth 1 -type f -name '*.sh' -print0
    } | LC_ALL=C sort -z | xargs -0 sha256sum
    printf 'control-api-source sha256:%s\n' "$(control_api_source_digest "$root")"
  ) | sha256sum | awk '{ print $1 }'
}

verify_versioned_release() {
  release=$1
  receipt=$2
  assert_directory_owned_mode "$release" 0 555
  assert_regular_file_owned_mode "$release/.blazn-release-files.sha256" 0 444
  assert_regular_file_owned_mode "$receipt" 0 600
  manifest_digest=sha256:$(sha256_file "$release/.blazn-release-files.sha256")
  jq -e --arg path "$release" --arg manifestDigest "$manifest_digest" \
    '.schemaVersion == "blazn.dev/release/v1" and .path == $path and .manifestDigest == $manifestDigest and (.releaseDigest | test("^sha256:[a-f0-9]{64}$"))' \
    "$receipt" >/dev/null || die "versioned release receipt is invalid: $release"
  (
    cd "$release"
    sha256sum -c .blazn-release-files.sha256 >/dev/null
  ) || die "versioned release file manifest failed: $release"
  expected_source=$(jq -er .controlApiSourceDigest "$receipt")
  expected_config=$(jq -er .controlPlaneConfigDigest "$receipt")
  [ "$expected_source" = "sha256:$(control_api_source_digest "$release/infra/milestone-2")" ] || die "versioned release API/migration source digest changed"
  [ "$expected_config" = "sha256:$(control_plane_config_digest "$release/infra/milestone-2")" ] || die "versioned release control-plane config digest changed"
}

verify_legacy_release() {
  release=$1
  receipt=$2
  assert_directory_owned_mode "$release" 0 555
  assert_regular_file_owned_mode "$receipt" 0 600
  manifest=$(jq -er .manifestPath "$receipt")
  assert_regular_file_owned_mode "$manifest" 0 600
  manifest_digest=sha256:$(sha256_file "$manifest")
  jq -e --arg path "$release" --arg manifestDigest "$manifest_digest" --arg manifestPath "$manifest" \
    '.schemaVersion == "blazn.dev/legacy-release/v1" and .path == $path and .manifestPath == $manifestPath and .manifestDigest == $manifestDigest and (.releaseDigest | test("^sha256:[a-f0-9]{64}$"))' \
    "$receipt" >/dev/null || die "legacy release receipt is invalid: $release"
  (
    cd "$release"
    sha256sum -c "$manifest" >/dev/null
  ) || die "legacy release file manifest failed: $release"
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
