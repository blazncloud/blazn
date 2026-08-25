#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$ROOT_DIR/../.." && pwd)
tmp=$(mktemp -d)
cleanup() {
  find "$tmp" -type f -delete
  find "$tmp" -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$tmp/infra" "$tmp/services" "$tmp/packages"
cp -R "$ROOT_DIR" "$tmp/infra/milestone-2"
cp -R "$REPO_ROOT/infra/node" "$tmp/infra/node"
cp -R "$REPO_ROOT/services/control-api" "$tmp/services/control-api"
cp -R "$REPO_ROOT/packages/contracts" "$tmp/packages/contracts"

# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/common.sh"
before=$(control_plane_config_digest "$tmp/infra/milestone-2")
printf '\n# digest regression probe\n' >>"$tmp/infra/milestone-2/postgres-compat/ensure-controller-roles.sh"
after=$(control_plane_config_digest "$tmp/infra/milestone-2")
[ "$before" != "$after" ] || {
  printf 'privileged PostgreSQL compatibility logic is absent from the configuration digest\n' >&2
  exit 1
}

legacy_config_digest() {
  legacy_root=$1
  (
    cd "$legacy_root"
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
    printf 'control-api-source sha256:%s\n' "$(control_api_source_digest "$legacy_root")"
  ) | sha256sum | awk '{ print $1 }'
}

find "$tmp/infra/milestone-2/postgres-compat" -type f -delete
find "$tmp/infra/milestone-2/postgres-compat" -depth -type d -empty -delete
legacy_expected=$(legacy_config_digest "$tmp/infra/milestone-2")
legacy_actual=$(control_plane_config_digest "$tmp/infra/milestone-2")
[ "$legacy_actual" = "$legacy_expected" ] || {
  printf 'configuration digest is incompatible with releases that predate PostgreSQL compatibility logic\n' >&2
  exit 1
}
