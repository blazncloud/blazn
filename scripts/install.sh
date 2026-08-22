#!/bin/sh

# Install a signed, immutable Blazn CLI release.
#
# Configuration:
#   BLAZN_VERSION       Required immutable release tag (for example, v0.1.0).
#   BLAZN_INSTALL_DIR   Destination directory (default: $HOME/.local/bin).
#   BLAZN_DIST_URL      Release root (default: GitHub releases).
#   BLAZN_NO_PATH_UPDATE
#                       Set to 1 to leave shell profiles unchanged.
#   BLAZN_SHELL_PROFILE Explicit POSIX shell profile to update when PATH setup
#                       cannot be inferred from SHELL.
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
BLAZN_EMBEDDED_ALLOWED_SIGNERS='blazn-release namespaces="blazn-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIItePt9Lyq9CrhFeJ8VUdmH559u1x3sSxEUnVk0zbGp blazn-poc-release-v2'
BLAZN_EMBEDDED_SIGNING_FINGERPRINT='SHA256:/B552TYf50sxCpMS4R6hLAXoHI7vouJ39yM9BQjr5Dk'

blazn_err() {
  printf 'blazn installer: %s\n' "$*" >&2
}

blazn_die() {
  blazn_err "$*"
  exit 1
}

blazn_command_required() {
  if [ "${BLAZN_ALLOW_INSECURE_TEST_ORIGIN:-}" = "1" ] && [ "${BLAZN_TEST_MISSING_COMMAND:-}" = "$1" ]; then
    blazn_die "required command not found: $1"
  fi
  command -v "$1" >/dev/null 2>&1 || blazn_die "required command not found: $1"
}

blazn_configure_path() {
  case "${BLAZN_NO_PATH_UPDATE:-0}" in
    0) ;;
    1)
      printf 'Shell PATH update skipped because BLAZN_NO_PATH_UPDATE=1.\n'
      return 0
      ;;
    *)
      blazn_err "BLAZN_NO_PATH_UPDATE must be 0 or 1; shell profile was not changed"
      return 0
      ;;
  esac

  case ":${PATH:-}:" in
    *:"$blazn_install_dir":*) return 0 ;;
  esac

  case "$blazn_install_dir" in
    *:*)
      blazn_err "cannot add an installation directory containing ':' to PATH"
      return 0
      ;;
    *'
'*)
      blazn_err "cannot add an installation directory containing a newline to PATH"
      return 0
      ;;
  esac

  blazn_profile=${BLAZN_SHELL_PROFILE:-}
  if [ -z "$blazn_profile" ]; then
    blazn_shell_name=${SHELL##*/}
    case "$blazn_shell_name" in
      zsh)
        case "${ZDOTDIR:-}" in
          '') blazn_profile="${HOME:?HOME is required}/.zprofile" ;;
          /*) blazn_profile="$ZDOTDIR/.zprofile" ;;
          *)
            blazn_err "ignoring relative ZDOTDIR while selecting a shell profile"
            blazn_profile="${HOME:?HOME is required}/.zprofile"
            ;;
        esac
        ;;
      bash)
        if [ -e "${HOME:?HOME is required}/.bash_profile" ]; then
          blazn_profile="$HOME/.bash_profile"
        elif [ -e "$HOME/.bash_login" ]; then
          blazn_profile="$HOME/.bash_login"
        else
          blazn_profile="$HOME/.profile"
        fi
        ;;
      sh|dash|ksh|mksh)
        blazn_profile="${HOME:?HOME is required}/.profile"
        ;;
      *)
        blazn_err "could not infer a POSIX shell profile from SHELL=${SHELL:-unset}"
        blazn_err "set BLAZN_SHELL_PROFILE or add $blazn_install_dir to PATH"
        return 0
        ;;
    esac
  fi

  case "$blazn_profile" in
    /*) ;;
    *)
      blazn_err "BLAZN_SHELL_PROFILE must be an absolute path; shell profile was not changed"
      return 0
      ;;
  esac
  if [ -e "$blazn_profile" ] && [ ! -f "$blazn_profile" ]; then
    blazn_err "shell profile is not a regular file: $blazn_profile"
    return 0
  fi

  blazn_profile_parent=${blazn_profile%/*}
  mkdir -p "$blazn_profile_parent" || {
    blazn_err "could not create shell profile directory $blazn_profile_parent"
    return 0
  }
  blazn_escaped_install_dir=$(printf '%s' "$blazn_install_dir" | sed "s/'/'\\\\''/g")
  blazn_path_line="export PATH='${blazn_escaped_install_dir}':\"\$PATH\""
  if [ -f "$blazn_profile" ] && grep -Fqx "$blazn_path_line" "$blazn_profile"; then
    return 0
  fi
  {
    printf '\n# Added by the Blazn installer.\n'
    printf '%s\n' "$blazn_path_line"
  } >> "$blazn_profile" || {
    blazn_err "could not update shell profile $blazn_profile"
    return 0
  }
  printf 'Added %s to PATH in %s. Open a new terminal to run blazn.\n' \
    "$blazn_install_dir" "$blazn_profile"
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

blazn_receipt_value() {
  blazn_receipt_file=$1
  blazn_receipt_key=$2
  awk -F= -v wanted="$blazn_receipt_key" '
    $1 == wanted { count++; value = substr($0, index($0, "=") + 1) }
    END { if (count != 1 || value == "") exit 1; print value }
  ' "$blazn_receipt_file"
}

blazn_process_start() {
  blazn_process_pid=$1
  if [ -r "/proc/$blazn_process_pid/stat" ]; then
    sed 's/^.*) //' "/proc/$blazn_process_pid/stat" | awk '{print $20}'
  else
    LC_ALL=C TZ=UTC ps -p "$blazn_process_pid" -o lstart= 2>/dev/null | awk '{$1=$1; print}'
  fi
}

blazn_acquire_lock() {
  blazn_owner_start=$(blazn_process_start "$$")
  [ -n "$blazn_owner_start" ] || blazn_die "could not determine installer process identity"
  blazn_lock_candidate="$blazn_install_dir/.blazn-lock-owner.$$"
  [ ! -e "$blazn_lock_candidate" ] || blazn_die "stale lock candidate exists at $blazn_lock_candidate"
  {
    printf 'pid=%s\n' "$$"
    printf 'start=%s\n' "$blazn_owner_start"
  } > "$blazn_lock_candidate" || blazn_die "could not write lifecycle lock candidate"
  chmod 0600 "$blazn_lock_candidate"
  if ln "$blazn_lock_candidate" "$blazn_lock_file" 2>/dev/null; then
    rm -f "$blazn_lock_candidate"
    blazn_lock_candidate=""
    blazn_lock_owned=1
    return 0
  fi
  rm -f "$blazn_lock_candidate"
  blazn_lock_candidate=""
  if [ ! -e "$blazn_lock_file" ]; then
    blazn_die "could not create lifecycle lock at $blazn_lock_file"
  fi
  blazn_recover_stale_lock
  blazn_acquire_lock
}

blazn_write_journal() {
  blazn_journal_state=$1
  {
    printf 'state=%s\n' "$blazn_journal_state"
    printf 'had_binary=%s\n' "$blazn_had_binary"
    printf 'had_receipt=%s\n' "$blazn_had_receipt"
  } > "$blazn_lock_journal" || blazn_die "could not write installation journal"
  chmod 0600 "$blazn_lock_journal"
}

blazn_recover_stale_lock() {
  [ -f "$blazn_lock_file" ] && [ ! -L "$blazn_lock_file" ] || \
    blazn_die "lifecycle lock path is not a regular file: $blazn_lock_file"

  if [ -e "$blazn_recovery_claim" ]; then
    [ -f "$blazn_recovery_claim" ] && [ ! -L "$blazn_recovery_claim" ] || \
      blazn_die "recovery owner claim is not a regular file"
    blazn_recovery_pid=$(blazn_receipt_value "$blazn_recovery_claim" pid 2>/dev/null || true)
    blazn_recovery_start=$(blazn_receipt_value "$blazn_recovery_claim" start 2>/dev/null || true)
    if [ -n "$blazn_recovery_pid" ] && [ -n "$blazn_recovery_start" ] && kill -0 "$blazn_recovery_pid" 2>/dev/null; then
      blazn_live_start=$(blazn_process_start "$blazn_recovery_pid" 2>/dev/null || true)
      if [ "$blazn_live_start" = "$blazn_recovery_start" ]; then
        blazn_die "another installer is reconciling $blazn_lock_file"
      fi
    fi
    if [ -e "$blazn_recovery_fence" ]; then
      [ -f "$blazn_recovery_fence" ] && [ ! -L "$blazn_recovery_fence" ] && \
        [ "$blazn_recovery_fence" -ef "$blazn_lock_file" ] || \
        blazn_die "stale recovery fence does not match the lifecycle lock"
    fi
    blazn_die "a previous lifecycle recoverer stopped unexpectedly; preserve the claim and fence for manual reconciliation"
  fi

  blazn_recovery_start=$(blazn_process_start "$$")
  [ -n "$blazn_recovery_start" ] || blazn_die "could not determine recovery process identity"
  blazn_recovery_candidate="$blazn_install_dir/.blazn-recovery-owner.$$"
  [ ! -e "$blazn_recovery_candidate" ] || blazn_die "stale recovery owner candidate exists"
  {
    printf 'pid=%s\n' "$$"
    printf 'start=%s\n' "$blazn_recovery_start"
  } > "$blazn_recovery_candidate" || blazn_die "could not write recovery owner candidate"
  chmod 0600 "$blazn_recovery_candidate"
  if ! ln "$blazn_recovery_candidate" "$blazn_recovery_claim" 2>/dev/null; then
    rm -f "$blazn_recovery_candidate"
    blazn_die "another installer claimed lifecycle recovery"
  fi
  rm -f "$blazn_recovery_candidate"
  blazn_recovery_candidate=""

  if [ "${BLAZN_ALLOW_INSECURE_TEST_ORIGIN:-}" = "1" ] && [ -n "${BLAZN_TEST_RECOVERY_PAUSE_FILE:-}" ]; then
    : > "${BLAZN_TEST_RECOVERY_PAUSE_FILE}.ready"
    while [ -e "$BLAZN_TEST_RECOVERY_PAUSE_FILE" ]; do sleep 1; done
  fi

  if ! ln "$blazn_lock_file" "$blazn_recovery_fence" 2>/dev/null; then
    blazn_die "could not fence the stale lifecycle lock"
  fi
  [ "$blazn_recovery_fence" -ef "$blazn_lock_file" ] || \
    blazn_die "lifecycle recovery fence lost the stale lock identity"

  blazn_owner_pid=$(blazn_receipt_value "$blazn_recovery_fence" pid 2>/dev/null || true)
  blazn_owner_start=$(blazn_receipt_value "$blazn_recovery_fence" start 2>/dev/null || true)
  if [ -n "$blazn_owner_pid" ] && [ -n "$blazn_owner_start" ] && kill -0 "$blazn_owner_pid" 2>/dev/null; then
    blazn_live_start=$(blazn_process_start "$blazn_owner_pid" 2>/dev/null || true)
    if [ "$blazn_live_start" = "$blazn_owner_start" ]; then
      rm -f "$blazn_recovery_fence" "$blazn_recovery_claim"
      blazn_die "another Blazn install or uninstall operation owns $blazn_lock_file"
    fi
  fi

  if [ -f "$blazn_lock_journal" ] && [ ! -L "$blazn_lock_journal" ]; then
    blazn_stale_state=$(blazn_receipt_value "$blazn_lock_journal" state) || \
      blazn_die "stale lifecycle journal is invalid; manual reconciliation is required"
    blazn_stale_had_binary=$(blazn_receipt_value "$blazn_lock_journal" had_binary) || \
      blazn_die "stale lifecycle journal is missing binary state"
    blazn_stale_had_receipt=$(blazn_receipt_value "$blazn_lock_journal" had_receipt) || \
      blazn_die "stale lifecycle journal is missing receipt state"
    case "$blazn_stale_had_binary:$blazn_stale_had_receipt" in
      0:0|1:1) ;;
      *) blazn_die "stale lifecycle journal has inconsistent ownership state" ;;
    esac

    case "$blazn_stale_state" in
      preparing)
        if [ "$blazn_stale_had_binary" = "1" ]; then
          if [ -f "$blazn_backup_binary" ] && [ ! -L "$blazn_backup_binary" ]; then
            rm -f "$blazn_destination"
            mv "$blazn_backup_binary" "$blazn_destination" || blazn_die "could not restore stale binary backup"
          elif [ ! -f "$blazn_destination" ]; then
            blazn_die "stale transaction lost both the owned binary and its backup"
          fi
          if [ -f "$blazn_backup_receipt" ] && [ ! -L "$blazn_backup_receipt" ]; then
            rm -f "$blazn_receipt"
            mv "$blazn_backup_receipt" "$blazn_receipt" || blazn_die "could not restore stale receipt backup"
          elif [ ! -f "$blazn_receipt" ]; then
            blazn_die "stale transaction lost both the owned receipt and its backup"
          fi
        else
          rm -f "$blazn_destination" "$blazn_receipt"
        fi
        ;;
      committed)
        [ -f "$blazn_destination" ] && [ -f "$blazn_receipt" ] || \
          blazn_die "committed stale transaction is missing its installed pair"
        ;;
      uninstall_preparing)
        if [ -f "$blazn_destination" ]; then
          if [ -f "$blazn_receipt" ] && [ ! -e "$blazn_uninstall_receipt" ]; then
            : # Crash occurred before receipt staging; the owned pair is intact.
          elif [ ! -e "$blazn_receipt" ] && [ -f "$blazn_uninstall_receipt" ] && [ ! -L "$blazn_uninstall_receipt" ]; then
            mv "$blazn_uninstall_receipt" "$blazn_receipt" || blazn_die "could not restore stale uninstall receipt"
          else
            blazn_die "stale uninstall receipt state is ambiguous"
          fi
        else
          rm -f "$blazn_uninstall_receipt"
        fi
        ;;
      *) blazn_die "unknown stale lifecycle journal state: $blazn_stale_state" ;;
    esac
    rm -f "$blazn_stage_binary" "$blazn_stage_receipt" "$blazn_backup_binary" "$blazn_backup_receipt"
  elif [ -e "$blazn_stage_binary" ] || [ -e "$blazn_stage_receipt" ] || \
       [ -e "$blazn_backup_binary" ] || [ -e "$blazn_backup_receipt" ] || \
       [ -e "$blazn_uninstall_receipt" ]; then
    blazn_die "stale lifecycle lock has transaction files but no valid journal"
  fi

  [ "$blazn_recovery_fence" -ef "$blazn_lock_file" ] || \
    blazn_die "lifecycle lock changed during recovery"
  blazn_claim_pid=$(blazn_receipt_value "$blazn_recovery_claim" pid) || blazn_die "recovery owner claim is invalid"
  blazn_claim_start=$(blazn_receipt_value "$blazn_recovery_claim" start) || blazn_die "recovery owner claim is invalid"
  [ "$blazn_claim_pid" = "$$" ] && [ "$blazn_claim_start" = "$blazn_recovery_start" ] || \
    blazn_die "lifecycle recovery owner changed"
  rm -f "$blazn_lock_file" "$blazn_lock_journal" "$blazn_recovery_fence" "$blazn_recovery_claim"
}

blazn_test_checkpoint() {
  blazn_checkpoint=$1
  [ "${BLAZN_ALLOW_INSECURE_TEST_ORIGIN:-}" = "1" ] || return 0
  case "${BLAZN_TEST_FAIL_STEP:-}" in
    "$blazn_checkpoint") blazn_die "injected failure at $blazn_checkpoint" ;;
    "signal-$blazn_checkpoint") kill -TERM "$$" ;;
    "kill-$blazn_checkpoint") kill -KILL "$$" ;;
  esac
}

blazn_restore_file() {
  blazn_restore_kind=$1
  blazn_restore_source=$2
  blazn_restore_destination=$3
  if [ "${BLAZN_ALLOW_INSECURE_TEST_ORIGIN:-}" = "1" ] && [ "${BLAZN_TEST_FAIL_RESTORE:-}" = "$blazn_restore_kind" ]; then
    return 1
  fi
  mv "$blazn_restore_source" "$blazn_restore_destination"
}

blazn_cleanup() {
  [ "${blazn_cleanup_done:-0}" = "0" ] || return 0
  blazn_cleanup_done=1
  blazn_recovery_failed=0
  if [ "${blazn_install_in_progress:-0}" = "1" ]; then
    if [ "${blazn_new_binary_installed:-0}" = "1" ] && [ -n "${blazn_destination:-}" ]; then
      rm -f "$blazn_destination" 2>/dev/null || true
    fi
    if [ "${blazn_new_receipt_installed:-0}" = "1" ] && [ -n "${blazn_receipt:-}" ]; then
      rm -f "$blazn_receipt" 2>/dev/null || true
    fi
    if [ -n "${blazn_backup_binary:-}" ] && [ -f "$blazn_backup_binary" ]; then
      if ! blazn_restore_file binary "$blazn_backup_binary" "$blazn_destination" 2>/dev/null; then
        blazn_err "could not restore prior binary from $blazn_backup_binary"
        blazn_recovery_failed=1
      fi
    fi
    if [ -n "${blazn_backup_receipt:-}" ] && [ -f "$blazn_backup_receipt" ]; then
      if ! blazn_restore_file receipt "$blazn_backup_receipt" "$blazn_receipt" 2>/dev/null; then
        blazn_err "could not restore prior receipt from $blazn_backup_receipt"
        blazn_recovery_failed=1
      fi
    fi
  fi
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
  if [ "${blazn_lock_owned:-0}" = "1" ] && [ -n "${blazn_lock_file:-}" ]; then
    if [ "$blazn_recovery_failed" = "1" ]; then
      blazn_err "preserving lifecycle lock and journal for recovery"
    else
      rm -f "${blazn_lock_file:-}" "${blazn_lock_journal:-}" 2>/dev/null || \
        blazn_err "could not remove lifecycle lock or journal"
    fi
    blazn_lock_owned=0
  fi
  if [ -n "${blazn_lock_candidate:-}" ]; then
    rm -f "$blazn_lock_candidate" 2>/dev/null || true
  fi
  if [ -n "${blazn_recovery_candidate:-}" ]; then
    rm -f "$blazn_recovery_candidate" 2>/dev/null || true
  fi
}

blazn_command_required curl
blazn_command_required tar
blazn_command_required ssh-keygen
blazn_command_required awk
blazn_command_required grep
blazn_command_required ln
blazn_command_required mktemp
blazn_command_required ps

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
trap blazn_cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
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
blazn_version_output=$("$blazn_tmp_dir/extract/blazn" version --output=json 2>/dev/null) || \
  blazn_die "downloaded blazn binary failed its version smoke test"
printf '%s\n' "$blazn_version_output" | grep -F '"version":"'"$blazn_version"'"' >/dev/null || \
  blazn_die "downloaded blazn binary version does not match $blazn_version"

blazn_install_dir=${BLAZN_INSTALL_DIR:-${HOME:?HOME is required}/.local/bin}
case "$blazn_install_dir" in
  ''|/) blazn_die "unsafe installation directory" ;;
esac
mkdir -p "$blazn_install_dir" || blazn_die "could not create $blazn_install_dir"
[ -d "$blazn_install_dir" ] && [ ! -L "$blazn_install_dir" ] || \
  blazn_die "installation destination must be a real directory, not a symbolic link"

blazn_destination="$blazn_install_dir/blazn"
blazn_receipt="$blazn_install_dir/.blazn-install-receipt"
blazn_lock_file="$blazn_install_dir/.blazn-install.lock"
blazn_lock_journal="$blazn_install_dir/.blazn-install.journal"
blazn_recovery_claim="$blazn_install_dir/.blazn-install.recovery"
blazn_recovery_fence="$blazn_install_dir/.blazn-install.recovery-fence"
blazn_stage_binary="$blazn_install_dir/.blazn.new"
blazn_stage_receipt="$blazn_install_dir/.blazn-receipt.new"
blazn_backup_binary="$blazn_install_dir/.blazn.backup"
blazn_backup_receipt="$blazn_install_dir/.blazn-receipt.backup"
blazn_uninstall_receipt="$blazn_install_dir/.blazn-uninstall-receipt"
blazn_acquire_lock
[ ! -L "$blazn_destination" ] || blazn_die "refusing to replace a symbolic-link destination"
[ ! -L "$blazn_receipt" ] || blazn_die "refusing to replace a symbolic-link receipt"

if [ -e "$blazn_destination" ]; then
  [ -f "$blazn_destination" ] && [ ! -L "$blazn_destination" ] || \
    blazn_die "existing installation path is not a regular file"
  [ -f "$blazn_receipt" ] && [ ! -L "$blazn_receipt" ] || \
    blazn_die "existing blazn is not owned by a valid direct-install receipt; use its package manager or choose another install directory"
  blazn_installed_checksum=$(blazn_sha256 "$blazn_destination")
  blazn_receipt_version=$(blazn_receipt_value "$blazn_receipt" version) || \
    blazn_die "existing installation receipt has an invalid or duplicate version"
  blazn_receipt_checksum=$(blazn_receipt_value "$blazn_receipt" binary_sha256) || \
    blazn_die "existing installation receipt has an invalid or duplicate binary checksum"
  [ "$blazn_installed_checksum" = "$blazn_receipt_checksum" ] || \
    blazn_die "existing blazn differs from its receipt; refusing to replace an unowned or modified binary"
  if [ "$blazn_installed_checksum" = "$blazn_binary_checksum" ] && \
     [ "$blazn_receipt_version" = "$blazn_version" ]; then
    printf 'blazn %s is already installed at %s\n' "$blazn_version" "$blazn_destination"
    blazn_configure_path
    exit 0
  fi
elif [ -e "$blazn_receipt" ]; then
  blazn_die "installation receipt exists without its owned binary; reconcile it before installing"
fi

[ ! -e "$blazn_stage_binary" ] && [ ! -e "$blazn_stage_receipt" ] && \
[ ! -e "$blazn_backup_binary" ] && [ ! -e "$blazn_backup_receipt" ] || \
  blazn_die "unreconciled installation transaction files already exist"
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

blazn_install_in_progress=1
blazn_new_binary_installed=0
blazn_new_receipt_installed=0
blazn_backup_binary=""
blazn_backup_receipt=""

if [ -e "$blazn_destination" ]; then blazn_had_binary=1; else blazn_had_binary=0; fi
if [ -e "$blazn_receipt" ]; then blazn_had_receipt=1; else blazn_had_receipt=0; fi
[ "$blazn_had_binary" = "$blazn_had_receipt" ] || blazn_die "owned binary and receipt are inconsistent"
blazn_backup_binary="$blazn_install_dir/.blazn.backup"
blazn_backup_receipt="$blazn_install_dir/.blazn-receipt.backup"
blazn_write_journal preparing

if [ -e "$blazn_destination" ]; then
  [ -f "$blazn_destination" ] && [ ! -L "$blazn_destination" ] || \
    blazn_die "existing installation path is not a regular file"
  mv "$blazn_destination" "$blazn_backup_binary" || blazn_die "could not stage the existing blazn binary"
fi
if [ -e "$blazn_receipt" ]; then
  [ -f "$blazn_receipt" ] && [ ! -L "$blazn_receipt" ] || \
    blazn_die "existing receipt path is not a regular file"
  mv "$blazn_receipt" "$blazn_backup_receipt" || blazn_die "could not stage the existing receipt"
fi
blazn_test_checkpoint after-backup

if ! mv "$blazn_stage_binary" "$blazn_destination"; then
  blazn_die "could not atomically install blazn"
fi
blazn_new_binary_installed=1
blazn_stage_binary=""
blazn_test_checkpoint after-binary-install
if ! mv "$blazn_stage_receipt" "$blazn_receipt"; then
  blazn_die "blazn was installed, but its receipt could not be written"
fi
blazn_new_receipt_installed=1
blazn_stage_receipt=""
blazn_test_checkpoint after-receipt-install
blazn_write_journal committed

# The new binary/receipt pair is now the committed installation. Cleanup of
# rollback copies must never make a committed install roll back.
blazn_install_in_progress=0
blazn_new_binary_installed=0
blazn_new_receipt_installed=0

if [ -n "$blazn_backup_binary" ]; then
  rm -f "$blazn_backup_binary" || blazn_err "could not remove old binary backup $blazn_backup_binary"
  blazn_backup_binary=""
fi
if [ -n "$blazn_backup_receipt" ]; then
  rm -f "$blazn_backup_receipt" || blazn_err "could not remove old receipt backup $blazn_backup_receipt"
  blazn_backup_receipt=""
fi

printf 'Installed blazn %s to %s\n' "$blazn_version" "$blazn_destination"
blazn_configure_path
