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
  expected_mode=$3
  [ -d "$path" ] && [ ! -L "$path" ] || die "expected a non-symlink directory: $path"
  [ "$(stat -c '%u' "$path")" = "$expected_uid" ] || die "directory has unexpected owner: $path"
  [ "$(stat -c '%a' "$path")" = "$expected_mode" ] || die "directory has unexpected mode: $path"
}

assert_regular_file_owned_mode() {
  path=$1
  expected_uid=$2
  expected_mode=$3
  [ -f "$path" ] && [ ! -L "$path" ] || die "expected a non-symlink regular file: $path"
  [ "$(stat -c '%u' "$path")" = "$expected_uid" ] || die "file has unexpected owner: $path"
  [ "$(stat -c '%a' "$path")" = "$expected_mode" ] || die "file has unexpected mode: $path"
}

assert_approved_backup_mount() {
  backup_root=$1
  backup_mount=${BLAZN_BACKUP_MOUNT:-}
  backup_source=${BLAZN_BACKUP_SOURCE:-}
  backup_fstype=${BLAZN_BACKUP_FSTYPE:-}
  [ -n "$backup_mount" ] && [ -n "$backup_source" ] && [ -n "$backup_fstype" ] || \
    die "BLAZN_BACKUP_MOUNT, BLAZN_BACKUP_SOURCE, and BLAZN_BACKUP_FSTYPE are required"
  require_absolute_path BLAZN_BACKUP_MOUNT "$backup_mount"
  require_command findmnt
  require_command realpath
  assert_not_symlink_chain "$backup_mount"
  [ -d "$backup_mount" ] && [ ! -L "$backup_mount" ] || die "approved backup mountpoint is unavailable: $backup_mount"
  canonical_mount=$(realpath -e "$backup_mount")
  canonical_root=$(realpath -m "$backup_root")
  case "$canonical_root" in
    "$canonical_mount"/*) ;;
    *) die "backup root is outside the approved mountpoint" ;;
  esac
  mount_record=$(findmnt -rn --mountpoint "$canonical_mount" -o TARGET,SOURCE,FSTYPE) || \
    die "approved backup mountpoint is not actively mounted: $canonical_mount"
  set -- $mount_record
  [ "$#" -eq 3 ] || die "approved backup mount record is ambiguous"
  [ "$1" = "$canonical_mount" ] || die "backup mount target does not match the approved mountpoint"
  [ "$2" = "$backup_source" ] || die "backup mount source does not match the approved source"
  [ "$3" = "$backup_fstype" ] || die "backup mount filesystem type does not match the approved type"
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

control_plane_config_digest() {
  root=$1
  (
    cd "$root"
    sha256sum \
      compose.yaml \
      postgres-init/01-roles.sh \
      scripts/run-control-plane.sh \
      scripts/with-public-origin-lock.sh \
      ngrok.example.yml \
      systemd/blazn-control-plane.service \
      systemd/blazn-ngrok.service \
      systemd/blazn-ngrok-qualification.service
  ) | sha256sum | awk '{ print $1 }'
}
