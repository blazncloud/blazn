#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
node_image=node:22.19.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90

command -v docker >/dev/null 2>&1 || {
  printf 'docker is required for the pinned Node 22 control API test\n' >&2
  exit 1
}

tar -C "$repo_root" -cf - services/control-api packages/contracts | docker run --rm -i "$node_image" sh -euc '
  mkdir /work
  tar -xf - -C /work
  cd /work/services/control-api
  npm ci
  npm run check
  npm run build
  npm test
'
