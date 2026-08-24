#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

qual_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd -P)
export PYTHONDONTWRITEBYTECODE=1
python3 "${qual_dir}/tests/test_static.py"

for file in "${qual_dir}"/*.py "${qual_dir}"/schemas/*.json "${qual_dir}"/profiles/*.json; do
  [ -s "$file" ] || {
    printf 'empty proxy qualification artifact: %s\n' "$file" >&2
    exit 1
  }
done

if grep -R -n -E '(subprocess\.(run|Popen|call|check_call|check_output)|os\.system)[^\n]*(launchctl|systemctl|busctl|dbus-send|security|pkill|reboot|shutdown|sudo)' \
  "${qual_dir}"/*.py "${qual_dir}"/tests/test_static.py; then
  printf 'static qualification harness contains a forbidden native mutation command\n' >&2
  exit 1
fi

printf 'Proxy qualification static tests passed.\n'
