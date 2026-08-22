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
      ngrok.example.yml \
      systemd/blazn-control-plane.service
  ) | sha256sum | awk '{ print $1 }'
}
