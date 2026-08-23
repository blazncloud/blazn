#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
example_dir=$(CDPATH='' cd -- "${script_dir}/.." && pwd)
repo_root=$(CDPATH='' cd -- "${example_dir}/../.." && pwd)
validator="${repo_root}/services/control-api/dist/development-contract.js"

if [ ! -f "$validator" ] || [ ! -d "${repo_root}/services/control-api/node_modules/ajv" ]; then
  echo "offline validator prerequisite missing; run 'make test-development-contract' first" >&2
  exit 2
fi

(cd "$example_dir" && npm ci --ignore-scripts --offline --no-audit --no-fund)
(cd "$example_dir" && node --test test/coding-agent.test.mjs)
node "${script_dir}/validate-example.mjs"
