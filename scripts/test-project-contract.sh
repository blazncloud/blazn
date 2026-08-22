#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "${script_dir}/.." && pwd)
service_dir="$repo_root/services/control-api"

if [ ! -d "$service_dir/node_modules" ]; then
  (cd "$service_dir" && npm ci --ignore-scripts)
fi
(cd "$service_dir" && npm run build && node --test dist/project-contract-validation.test.js)
