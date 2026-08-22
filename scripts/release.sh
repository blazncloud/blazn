#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
Usage: scripts/release.sh [options]

Build deterministic Blazn release archives and a checksum manifest.

Options:
  --version VERSION       Version embedded in the binary and archive names.
  --commit COMMIT         Source commit embedded in the binary.
  --output DIR            Output directory (default: dist).
  --signing-key FILE      OpenSSH Ed25519 private key used to sign SHA256SUMS.
  --source-root DIR       Go module root (default: repository root).
  --build-package PKG     Go main package (default: ./cmd/blazn).
  -h, --help              Show this help.

Environment equivalents: VERSION, COMMIT, OUTPUT_DIR, SIGNING_KEY,
BLAZN_SOURCE_ROOT, BLAZN_BUILD_PACKAGE, SOURCE_DATE_EPOCH.
EOF
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "${script_dir}/.." && pwd)
source_root=${BLAZN_SOURCE_ROOT:-$repo_root}
output_dir=${OUTPUT_DIR:-${repo_root}/dist}
build_package=${BLAZN_BUILD_PACKAGE:-./cmd/blazn}
version=${VERSION:-}
commit=${COMMIT:-}
signing_key=${SIGNING_KEY:-}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || { echo "--version requires a value" >&2; exit 2; }
      version=$2
      shift 2
      ;;
    --commit)
      [ "$#" -ge 2 ] || { echo "--commit requires a value" >&2; exit 2; }
      commit=$2
      shift 2
      ;;
    --output)
      [ "$#" -ge 2 ] || { echo "--output requires a value" >&2; exit 2; }
      output_dir=$2
      shift 2
      ;;
    --signing-key)
      [ "$#" -ge 2 ] || { echo "--signing-key requires a value" >&2; exit 2; }
      signing_key=$2
      shift 2
      ;;
    --source-root)
      [ "$#" -ge 2 ] || { echo "--source-root requires a value" >&2; exit 2; }
      source_root=$2
      shift 2
      ;;
    --build-package)
      [ "$#" -ge 2 ] || { echo "--build-package requires a value" >&2; exit 2; }
      build_package=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ -z "$version" ]; then
  if git -C "$source_root" describe --tags --exact-match >/dev/null 2>&1; then
    version=$(git -C "$source_root" describe --tags --exact-match)
  else
    short_commit=$(git -C "$source_root" rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')
    version="0.0.0-${short_commit}"
  fi
fi

if [ -z "$commit" ]; then
  commit=$(git -C "$source_root" rev-parse HEAD 2>/dev/null || printf 'unknown')
fi

case "$version" in
  *[!A-Za-z0-9._+-]*|'')
    echo "version contains characters unsafe for release metadata: $version" >&2
    exit 2
    ;;
esac

case "$commit" in
  *[!A-Fa-f0-9]*|'')
    if [ "$commit" != unknown ]; then
      echo "commit must be a hexadecimal Git object ID or 'unknown'" >&2
      exit 2
    fi
    ;;
esac

command -v go >/dev/null 2>&1 || { echo "go is required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 1; }

archive_version=${version#v}
build_date_epoch=${SOURCE_DATE_EPOCH:-}
if [ -z "$build_date_epoch" ]; then
  build_date_epoch=$(git -C "$source_root" show -s --format=%ct "$commit" 2>/dev/null || printf '0')
fi
case "$build_date_epoch" in
  *[!0-9]*|'')
    echo "SOURCE_DATE_EPOCH must be a non-negative integer" >&2
    exit 2
    ;;
esac
build_date=$(date -u -r "$build_date_epoch" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d "@${build_date_epoch}" '+%Y-%m-%dT%H:%M:%SZ')

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/blazn-release.XXXXXX")
cleanup() {
  rm -rf -- "$tmp_root"
}
trap cleanup EXIT HUP INT TERM

if [ -e "$output_dir" ]; then
  [ -d "$output_dir" ] || { echo "output path is not a directory: $output_dir" >&2; exit 1; }
  if [ -n "$(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    echo "output directory must be empty: $output_dir" >&2
    exit 1
  fi
else
  mkdir -p -- "$output_dir"
fi

ldflags="-s -w -X main.version=${version} -X main.commit=${commit} -X main.buildTime=${build_date}"
targets='darwin arm64
linux amd64
linux arm64'

printf '%s\n' "$targets" | while read -r target_os target_arch; do
  [ -n "$target_os" ] || continue
  stage_dir="${tmp_root}/${target_os}-${target_arch}"
  mkdir -p -- "$stage_dir"

  echo "building ${target_os}/${target_arch}" >&2
  (
    cd "$source_root"
    CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch \
      go build -buildvcs=false -trimpath -ldflags "$ldflags" \
      -o "${stage_dir}/blazn" "$build_package"
  )
  chmod 0755 "${stage_dir}/blazn"

  archive="${output_dir}/blazn_${archive_version}_${target_os}_${target_arch}.tar.gz"
  if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    tar --sort=name --mtime="@${build_date_epoch}" --owner=0 --group=0 \
      --numeric-owner -C "$stage_dir" -czf "$archive" blazn
  else
    COPYFILE_DISABLE=1 tar -C "$stage_dir" -czf "$archive" blazn
  fi
done

printf '%s\n' "$version" >"${output_dir}/version.txt"

(
  cd "$output_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum blazn_*.tar.gz version.txt
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 blazn_*.tar.gz version.txt
  else
    echo "sha256sum or shasum is required" >&2
    exit 1
  fi
) >"${output_dir}/SHA256SUMS"

if [ -n "$signing_key" ]; then
  command -v ssh-keygen >/dev/null 2>&1 || { echo "ssh-keygen is required for signing" >&2; exit 1; }
  [ -f "$signing_key" ] || { echo "signing key does not exist: $signing_key" >&2; exit 1; }
  rm -f -- "${output_dir}/SHA256SUMS.sig"
  ssh-keygen -q -Y sign -f "$signing_key" -n blazn-release "${output_dir}/SHA256SUMS"
fi

echo "release artifacts written to ${output_dir}" >&2
