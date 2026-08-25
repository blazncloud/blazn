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
