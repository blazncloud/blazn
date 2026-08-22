#!/bin/sh
set -eu

if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
  printf 'usage: %s DIST_ROOT VERSION ALLOWED_SIGNERS FINGERPRINT [TEST_ROOT]\n' "$0" >&2
  exit 2
fi

dist_root=$1
version=$2
allowed_signers=$3
fingerprint=$4
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

owned_root=0
if [ "$#" -eq 5 ]; then
  test_root=$5
  mkdir -p "$test_root"
else
  test_root=$(mktemp -d "${TMPDIR:-/tmp}/blazn-e2e.XXXXXX")
  owned_root=1
fi

cleanup() {
  if [ "$owned_root" -eq 1 ]; then
    case "$test_root" in
      "${TMPDIR:-/tmp}"/blazn-e2e.*) rm -r -- "$test_root" ;;
      *) printf 'refusing to remove unexpected test root: %s\n' "$test_root" >&2 ;;
    esac
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

install_dir="$test_root/bin"
config_dir="$test_root/config"
mkdir -p "$install_dir" "$config_dir/blazn"
chmod 0700 "$config_dir/blazn"
printf 'preserve\n' > "$config_dir/blazn/keep"

install_once() {
  curl -fsSL "file://$repo_root/scripts/install.sh" |
    BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
    BLAZN_DIST_URL="file://$dist_root" \
    BLAZN_VERSION="$version" \
    BLAZN_INSTALL_DIR="$install_dir" \
    BLAZN_ALLOWED_SIGNERS="$allowed_signers" \
    BLAZN_SIGNING_FINGERPRINT="$fingerprint" \
      sh
}

install_once
PATH="$install_dir:$PATH" XDG_CONFIG_HOME="$config_dir" blazn version --output=json
PATH="$install_dir:$PATH" XDG_CONFIG_HOME="$config_dir" blazn doctor --output=json

first_checksum=$(shasum -a 256 "$install_dir/blazn" | awk '{print $1}')
install_once
second_checksum=$(shasum -a 256 "$install_dir/blazn" | awk '{print $1}')
[ "$first_checksum" = "$second_checksum" ] || {
  printf 'same-version reinstall changed the binary\n' >&2
  exit 1
}

PATH="$install_dir:$PATH" XDG_CONFIG_HOME="$config_dir" blazn uninstall --yes --output=json
[ ! -e "$install_dir/blazn" ] || { printf 'uninstall left the binary behind\n' >&2; exit 1; }
[ ! -e "$install_dir/.blazn-install-receipt" ] || { printf 'uninstall left the receipt behind\n' >&2; exit 1; }
[ "$(cat "$config_dir/blazn/keep")" = preserve ] || { printf 'uninstall changed configuration\n' >&2; exit 1; }

install_once
PATH="$install_dir:$PATH" XDG_CONFIG_HOME="$config_dir" blazn version --output=json >/dev/null

printf 'Milestone 1 curl install, idempotence, doctor, uninstall, and reinstall passed.\n'
