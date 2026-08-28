#!/bin/sh
set -eu

mkdir -p "$HOME" "$TMPDIR" "$GOCACHE" "$NPM_CONFIG_CACHE"
control_api=/workspace/src/blazn/services/control-api
if [ -d "$control_api" ] && [ ! -e "$control_api/node_modules" ]; then
  ln -s /opt/blazn-control-api/node_modules "$control_api/node_modules"
fi
exec sleep infinity
