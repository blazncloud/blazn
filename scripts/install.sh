#!/bin/sh

# Install a signed, immutable Blazn CLI release.
#
# Configuration:
#   BLAZN_VERSION       Required immutable release tag (for example, v0.1.0).
#   BLAZN_INSTALL_DIR   Destination directory (default: $HOME/.local/bin).
#   BLAZN_DIST_URL      Release root (default: GitHub releases).
#
# A controlled distribution can provide BLAZN_ALLOWED_SIGNERS and
# BLAZN_SIGNING_FINGERPRINT together. Production releases use the public key
# embedded below; replacing that key is a reviewed source change.

set -eu

BLAZN_RELEASE_IDENTITY="blazn-release"
BLAZN_SIGNATURE_NAMESPACE="blazn-release"
BLAZN_DEFAULT_DIST_URL="https://github.com/KingJammin/blazn/releases"

# Public half of the release key held by the release workflow. Rotation requires
# shipping a reviewed installer that contains the new trust root.
BLAZN_EMBEDDED_ALLOWED_SIGNERS='blazn-release namespaces="blazn-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOAAOSVVKmi6rjId2kH7hm06Tlew2O+S+CL6II9Xe/Yu blazn-poc-release'
BLAZN_EMBEDDED_SIGNING_FINGERPRINT='SHA256:7YNVtjsrLjtanzQluFUPQly75P2sNarToYIy4r7+Szs'

blazn_err() {
  printf 'blazn installer: %s\n' "$*" >&2
}

blazn_die() {
  blazn_err "$*"
  exit 1
}

blazn_command_required() {
  command -v "$1" >/dev/null 2>&1 || blazn_die "required command not found: $1"
}

blazn_download() {
  blazn_download_url=$1
  blazn_download_output=$2

  case "$blazn_download_url" in
    https://*)
      curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-delay 1 \
        --connect-timeout 15 --output "$blazn_download_output" "$blazn_download_url"
      ;;
    file://*)
      if [ "${BLAZN_ALLOW_INSECURE_TEST_ORIGIN:-}" != "1" ]; then
        blazn_die "file origins are allowed only with BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1"
      fi
      curl -fsSL --output "$blazn_download_output" "$blazn_download_url"
      ;;
    *)
      blazn_die "distribution URL must use HTTPS"
      ;;
  esac
}

blazn_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    blazn_die "sha256sum or shasum is required"
  fi
}

blazn_cleanup() {
  if [ -n "${blazn_stage_binary:-}" ]; then
    rm -f "$blazn_stage_binary" 2>/dev/null || true
  fi
  if [ -n "${blazn_stage_receipt:-}" ]; then
    rm -f "$blazn_stage_receipt" 2>/dev/null || true
  fi
  if [ -n "${blazn_tmp_dir:-}" ] && [ -d "$blazn_tmp_dir" ]; then
    rm -f \
      "$blazn_tmp_dir/archive.tar.gz" \
      "$blazn_tmp_dir/SHA256SUMS" \
      "$blazn_tmp_dir/SHA256SUMS.sig" \
      "$blazn_tmp_dir/allowed_signers" \
      "$blazn_tmp_dir/signing_keys" \
      "$blazn_tmp_dir/archive.list" \
      "$blazn_tmp_dir/archive.verbose" \
      "$blazn_tmp_dir/extract/blazn" 2>/dev/null || true
    rmdir "$blazn_tmp_dir/extract" 2>/dev/null || true
    rmdir "$blazn_tmp_dir" 2>/dev/null || true
  fi
}

blazn_command_required curl
blazn_command_required tar
blazn_command_required ssh-keygen
blazn_command_required awk
blazn_command_required mktemp

case "$(uname -s)" in
  Darwin) blazn_os=darwin ;;
  Linux) blazn_os=linux ;;
  *) blazn_die "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) blazn_arch=amd64 ;;
  arm64|aarch64) blazn_arch=arm64 ;;
  *) blazn_die "unsupported architecture: $(uname -m)" ;;
esac

blazn_dist_url=${BLAZN_DIST_URL:-$BLAZN_DEFAULT_DIST_URL}
blazn_dist_url=${blazn_dist_url%/}
case "$blazn_dist_url" in
  https://*|file://*) ;;
  *) blazn_die "BLAZN_DIST_URL must be an HTTPS URL" ;;
esac

blazn_tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/blazn-install.XXXXXX") || \
  blazn_die "could not create temporary directory"
trap blazn_cleanup EXIT HUP INT TERM
mkdir "$blazn_tmp_dir/extract"

blazn_version=${BLAZN_VERSION:-}
case "$blazn_version" in
  '') blazn_die "BLAZN_VERSION is required; use an immutable release tag such as v0.1.0" ;;
  .|..|*[!A-Za-z0-9._+-]*) blazn_die "invalid release version" ;;
esac
blazn_asset_version=${blazn_version#v}
[ -n "$blazn_asset_version" ] || blazn_die "invalid release version"

blazn_asset="blazn_${blazn_asset_version}_${blazn_os}_${blazn_arch}.tar.gz"
blazn_release_url="$blazn_dist_url/download/$blazn_version"

blazn_download "$blazn_release_url/SHA256SUMS" "$blazn_tmp_dir/SHA256SUMS"
blazn_download "$blazn_release_url/SHA256SUMS.sig" "$blazn_tmp_dir/SHA256SUMS.sig"

if [ -n "${BLAZN_ALLOWED_SIGNERS:-}" ]; then
  [ -f "$BLAZN_ALLOWED_SIGNERS" ] || blazn_die "BLAZN_ALLOWED_SIGNERS is not a regular file"
  [ -n "${BLAZN_SIGNING_FINGERPRINT:-}" ] || \
    blazn_die "BLAZN_SIGNING_FINGERPRINT is required with BLAZN_ALLOWED_SIGNERS"
  cp "$BLAZN_ALLOWED_SIGNERS" "$blazn_tmp_dir/allowed_signers"
  blazn_expected_fingerprint=$BLAZN_SIGNING_FINGERPRINT
else
  [ -n "$BLAZN_EMBEDDED_ALLOWED_SIGNERS" ] || \
    blazn_die "no production release signing key is configured"
  [ -n "$BLAZN_EMBEDDED_SIGNING_FINGERPRINT" ] || \
    blazn_die "no production release signing fingerprint is configured"
  printf '%s\n' "$BLAZN_EMBEDDED_ALLOWED_SIGNERS" > "$blazn_tmp_dir/allowed_signers"
  blazn_expected_fingerprint=$BLAZN_EMBEDDED_SIGNING_FINGERPRINT
fi

case "$blazn_expected_fingerprint" in
  SHA256:*) ;;
  *) blazn_die "signing fingerprint must use SHA256 format" ;;
esac

# `ssh-keygen -l` reads public-key records, not allowed_signers records with
# principals and options. Extract only each key type and base64 body before
# fingerprinting it, then require one trust root in total.
awk '
  {
    for (field = 1; field <= NF; field++) {
      if ($field ~ /^(ssh-|ecdsa-)/ && field < NF) {
        print $field " " $(field + 1)
        found++
        break
      }
    }
  }
  END { if (found == 0) exit 1 }
' "$blazn_tmp_dir/allowed_signers" > "$blazn_tmp_dir/signing_keys" || \
  blazn_die "allowed signers does not contain a public key"

blazn_fingerprints=$(ssh-keygen -lf "$blazn_tmp_dir/signing_keys" -E sha256 2>/dev/null) || \
  blazn_die "could not fingerprint allowed signing key"
blazn_fingerprint_total=$(printf '%s\n' "$blazn_fingerprints" | awk 'NF { count++ } END { print count + 0 }')
blazn_fingerprint_count=$(printf '%s\n' "$blazn_fingerprints" \
  | awk -v wanted="$blazn_expected_fingerprint" '$2 == wanted { count++ } END { print count + 0 }')
[ "$blazn_fingerprint_total" -eq 1 ] && [ "$blazn_fingerprint_count" -eq 1 ] || \
  blazn_die "allowed signers must contain exactly the expected signing key"

if ! ssh-keygen -Y verify \
  -f "$blazn_tmp_dir/allowed_signers" \
  -I "$BLAZN_RELEASE_IDENTITY" \
  -n "$BLAZN_SIGNATURE_NAMESPACE" \
  -s "$blazn_tmp_dir/SHA256SUMS.sig" \
  < "$blazn_tmp_dir/SHA256SUMS" >/dev/null 2>&1; then
  blazn_die "checksum signature verification failed"
fi

blazn_checksum_matches=$(awk -v asset="$blazn_asset" '
  {
    name = $2
    sub(/^\*/, "", name)
    if (name == asset) {
      print $1
    }
  }
' "$blazn_tmp_dir/SHA256SUMS")
blazn_checksum_count=$(printf '%s\n' "$blazn_checksum_matches" | awk 'NF { count++ } END { print count + 0 }')
[ "$blazn_checksum_count" -eq 1 ] || \
  blazn_die "checksum manifest must contain exactly one entry for $blazn_asset"
blazn_expected_checksum=$(printf '%s\n' "$blazn_checksum_matches" | awk 'NF { print; exit }')
case "$blazn_expected_checksum" in
  *[!0-9A-Fa-f]*|'') blazn_die "invalid SHA-256 checksum for $blazn_asset" ;;
esac
[ "${#blazn_expected_checksum}" -eq 64 ] || \
  blazn_die "invalid SHA-256 checksum length for $blazn_asset"

blazn_download "$blazn_release_url/$blazn_asset" "$blazn_tmp_dir/archive.tar.gz"
blazn_actual_checksum=$(blazn_sha256 "$blazn_tmp_dir/archive.tar.gz")
[ "$blazn_actual_checksum" = "$blazn_expected_checksum" ] || \
  blazn_die "checksum mismatch for $blazn_asset"

tar -tzf "$blazn_tmp_dir/archive.tar.gz" > "$blazn_tmp_dir/archive.list" || \
  blazn_die "release archive is not a valid gzip-compressed tar archive"
[ "$(awk 'END { print NR + 0 }' "$blazn_tmp_dir/archive.list")" -eq 1 ] || \
  blazn_die "release archive must contain only the blazn binary"
[ "$(sed -n '1p' "$blazn_tmp_dir/archive.list")" = "blazn" ] || \
  blazn_die "release archive contains an unexpected or unsafe path"

tar -tvzf "$blazn_tmp_dir/archive.tar.gz" > "$blazn_tmp_dir/archive.verbose" || \
  blazn_die "could not inspect release archive"
case "$(sed -n '1p' "$blazn_tmp_dir/archive.verbose")" in
  -*) ;;
  *) blazn_die "blazn archive member must be a regular file" ;;
esac

tar -xzf "$blazn_tmp_dir/archive.tar.gz" -C "$blazn_tmp_dir/extract" blazn || \
  blazn_die "could not extract blazn binary"
[ -f "$blazn_tmp_dir/extract/blazn" ] && [ ! -L "$blazn_tmp_dir/extract/blazn" ] || \
  blazn_die "extracted blazn binary is not a regular file"
chmod 0755 "$blazn_tmp_dir/extract/blazn"
blazn_binary_checksum=$(blazn_sha256 "$blazn_tmp_dir/extract/blazn")

blazn_install_dir=${BLAZN_INSTALL_DIR:-${HOME:?HOME is required}/.local/bin}
case "$blazn_install_dir" in
  ''|/) blazn_die "unsafe installation directory" ;;
esac
mkdir -p "$blazn_install_dir" || blazn_die "could not create $blazn_install_dir"
[ -d "$blazn_install_dir" ] && [ ! -L "$blazn_install_dir" ] || \
  blazn_die "installation destination must be a real directory, not a symbolic link"

blazn_destination="$blazn_install_dir/blazn"
blazn_receipt="$blazn_install_dir/.blazn-install-receipt"
[ ! -L "$blazn_destination" ] || blazn_die "refusing to replace a symbolic-link destination"
[ ! -L "$blazn_receipt" ] || blazn_die "refusing to replace a symbolic-link receipt"

if [ -f "$blazn_destination" ] && [ ! -L "$blazn_destination" ]; then
  blazn_installed_checksum=$(blazn_sha256 "$blazn_destination")
  blazn_receipt_version=""
  if [ -f "$blazn_receipt" ] && [ ! -L "$blazn_receipt" ]; then
    blazn_receipt_version=$(awk -F= '$1 == "version" { print substr($0, index($0, "=") + 1); exit }' "$blazn_receipt")
  fi
  if [ "$blazn_installed_checksum" = "$blazn_binary_checksum" ] && \
     [ "$blazn_receipt_version" = "$blazn_version" ]; then
    printf 'blazn %s is already installed at %s\n' "$blazn_version" "$blazn_destination"
    exit 0
  fi
fi

blazn_stage_binary="$blazn_install_dir/.blazn.new.$$"
blazn_stage_receipt="$blazn_install_dir/.blazn-receipt.new.$$"
cp "$blazn_tmp_dir/extract/blazn" "$blazn_stage_binary" || \
  blazn_die "could not stage blazn binary"
chmod 0755 "$blazn_stage_binary"

{
  printf 'version=%s\n' "$blazn_version"
  printf 'asset=%s\n' "$blazn_asset"
  printf 'archive_sha256=%s\n' "$blazn_expected_checksum"
  printf 'binary_sha256=%s\n' "$blazn_binary_checksum"
  printf 'source=%s\n' "$blazn_release_url"
} > "$blazn_stage_receipt" || blazn_die "could not stage installation receipt"
chmod 0644 "$blazn_stage_receipt"

if ! mv -f "$blazn_stage_binary" "$blazn_destination"; then
  rm -f "$blazn_stage_binary" "$blazn_stage_receipt"
  blazn_die "could not atomically install blazn"
fi
if ! mv -f "$blazn_stage_receipt" "$blazn_receipt"; then
  rm -f "$blazn_stage_receipt"
  blazn_die "blazn was installed, but its receipt could not be written"
fi

printf 'Installed blazn %s to %s\n' "$blazn_version" "$blazn_destination"
case ":${PATH:-}:" in
  *:"$blazn_install_dir":*) ;;
  *) printf 'Add %s to PATH to run blazn. No shell configuration was changed.\n' "$blazn_install_dir" ;;
esac
