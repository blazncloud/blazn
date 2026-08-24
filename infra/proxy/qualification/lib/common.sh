#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

proxy_qual_dir=$(unset CDPATH; cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)

proxy_qual_exec() {
  action=$1
  shift
  exec python3 "${proxy_qual_dir}/qualification.py" "$action" "$@"
}
