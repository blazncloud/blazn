#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "${script_dir}/.." && pwd)
fixture_root="${repo_root}/tests/fixtures/release-module"
tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/blazn-release-test.XXXXXX")

cleanup() {
  rm -rf -- "$tmp_root"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

key_file="${tmp_root}/release-key"
allowed_signers="${tmp_root}/allowed_signers"
output_dir="${tmp_root}/dist"

ssh-keygen -q -t ed25519 -N '' -C release-test -f "$key_file"
printf 'blazn-release namespaces="blazn-release" %s\n' "$(cat "${key_file}.pub")" >"$allowed_signers"

SOURCE_DATE_EPOCH=1724198400 "${repo_root}/scripts/release.sh" \
  --source-root "$fixture_root" \
  --version v1.2.3 \
  --commit 0123456789abcdef0123456789abcdef01234567 \
  --output "$output_dir" \
  --signing-key "$key_file"

expected_archives='blazn_1.2.3_darwin_arm64.tar.gz
blazn_1.2.3_linux_amd64.tar.gz
blazn_1.2.3_linux_arm64.tar.gz'
actual_archives=$(find "$output_dir" -type f -name 'blazn_*.tar.gz' -exec basename {} \; | LC_ALL=C sort)
[ "$actual_archives" = "$expected_archives" ] || {
  echo "unexpected archives:" >&2
  printf '%s\n' "$actual_archives" >&2
  exit 1
}

[ "$(cat "${output_dir}/version.txt")" = v1.2.3 ] || {
  echo "unexpected version.txt content" >&2
  exit 1
}

for archive in "$output_dir"/blazn_*.tar.gz; do
  contents=$(tar -tzf "$archive")
  [ "$contents" = blazn ] || {
    echo "unexpected archive content in $archive: $contents" >&2
    exit 1
  }
done

(
  cd "$output_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c SHA256SUMS
  else
    shasum -a 256 -c SHA256SUMS
  fi
)

ssh-keygen -q -Y verify -f "$allowed_signers" -I blazn-release \
  -n blazn-release -s "${output_dir}/SHA256SUMS.sig" \
  <"${output_dir}/SHA256SUMS"

mkdir -p "${tmp_root}/extract"
tar -C "${tmp_root}/extract" -xzf "${output_dir}/blazn_1.2.3_linux_amd64.tar.gz"
metadata=$("${tmp_root}/extract/blazn")
[ "$metadata" = 'v1.2.3 0123456789abcdef0123456789abcdef01234567 2024-08-21T00:00:00Z' ] || {
  echo "unexpected embedded metadata: $metadata" >&2
  exit 1
}

if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  real_one="${tmp_root}/real-one"
  real_two="${tmp_root}/real-two"
  for destination in "$real_one" "$real_two"; do
    BLAZN_RELEASE_MODE=publish SOURCE_DATE_EPOCH=1724198400 "${repo_root}/scripts/release.sh" \
      --source-root "$repo_root" \
      --version v9.9.9-test \
      --commit 0123456789abcdef0123456789abcdef01234567 \
      --output "$destination" \
      --signing-key "$key_file"
  done
  cmp "$real_one/SHA256SUMS" "$real_two/SHA256SUMS"
  for archive in "$real_one"/blazn_*.tar.gz; do
    name=$(basename "$archive")
    cmp "$archive" "$real_two/$name"
  done
  mkdir -p "${tmp_root}/real-extract"
  tar -C "${tmp_root}/real-extract" -xzf "$real_one/blazn_9.9.9-test_linux_amd64.tar.gz"
  "${tmp_root}/real-extract/blazn" version --output=json | grep -F '"version":"v9.9.9-test"' >/dev/null
else
  echo "real reproducibility check is reserved for the pinned GNU-tar release builder" >&2
fi

echo "release packaging tests passed"
