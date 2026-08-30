#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  printf 'usage: %s BLAZN_BIN SANDBOX_ID OUTPUT_PATH EVIDENCE_JSON\n' "$0" >&2
  exit 64
fi

blazn=$1
sandbox_id=$2
output=$3
evidence=$4
checksum=$output.sha256
parent=$(dirname -- "$output")
if [ ! -d "$parent" ] || [ ! -w "$parent" ]; then printf 'patch output directory is not writable: %s\n' "$parent" >&2; exit 1; fi
output=$(CDPATH='' cd -- "$parent" && pwd)/$(basename -- "$output")
checksum=$output.sha256
if [ -e "$output" ] || [ -e "$checksum" ]; then printf 'refusing to overwrite patch output or checksum: %s\n' "$output" >&2; exit 1; fi

patch_temp=$(mktemp "$parent/.blazn-patch.XXXXXX")
checksum_temp=$(mktemp "$parent/.blazn-checksum.XXXXXX")
rm -f -- "$patch_temp" "$checksum_temp"
complete=0
output_linked=0
cleanup() {
  rm -f -- "$patch_temp" "$checksum_temp"
  if [ "$complete" -eq 0 ] && [ "$output_linked" -eq 1 ]; then
    rm -f -- "$output"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if command -v sha256sum >/dev/null 2>&1; then
  digest_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  digest_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  printf '%s\n' 'sha256sum or shasum is required' >&2
  exit 1
fi

"$blazn" --output json sandbox download "$sandbox_id" \
  /workspace/artifacts/change.patch "$patch_temp" >"$evidence"
test -s "$patch_temp"
expected=$(jq -er '.sha256' "$evidence")
actual=sha256:$(digest_file "$patch_temp")
[ "$actual" = "$expected" ] || {
  printf 'downloaded patch checksum mismatch: expected %s, got %s\n' "$expected" "$actual" >&2
  exit 1
}
printf '%s  %s\n' "${actual#sha256:}" "$(basename -- "$output")" >"$checksum_temp"
ln -- "$patch_temp" "$output" 2>/dev/null || {
  printf 'refusing to overwrite patch output: %s\n' "$output" >&2
  exit 1
}
output_linked=1
ln -- "$checksum_temp" "$checksum" 2>/dev/null || {
  rm -f -- "$output"
  output_linked=0
  printf 'refusing to overwrite patch checksum: %s\n' "$checksum" >&2
  exit 1
}
rm -f -- "$patch_temp" "$checksum_temp"
complete=1
printf '%s\n' "$actual"
