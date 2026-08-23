#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || { printf 'root is required\n' >&2; exit 77; }
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
test_root=$(mktemp -d /tmp/blazn-identity-secret-test.XXXXXX)
cleanup() { rm -rf -- "$test_root"; }
trap cleanup EXIT HUP INT TERM

secrets=$test_root/secrets
"$script_dir/generate-secrets.sh" "$secrets" admin@example.test >/dev/null
[ "$(stat -c '%u:%a' "$secrets")" = '0:700' ]
if find "$secrets" -type f \( ! -user root -o -perm /077 -o -links +1 \) -print -quit | grep -q .; then printf 'generated secret metadata is unsafe\n' >&2; exit 1; fi
(cd "$secrets" && sha256sum ./* | sort) > "$test_root/before"
"$script_dir/generate-secrets.sh" "$secrets" admin@example.test >/dev/null
(cd "$secrets" && sha256sum ./* | sort) > "$test_root/after"
cmp "$test_root/before" "$test_root/after"

symlink_root=$test_root/symlink; mkdir -m 700 "$symlink_root"; ln -s /etc/passwd "$symlink_root/postgres-admin-password"
if "$script_dir/generate-secrets.sh" "$symlink_root" admin@example.test >/dev/null 2>&1; then printf 'symlinked secret was accepted\n' >&2; exit 1; fi
hardlink_root=$test_root/hardlink; mkdir -m 700 "$hardlink_root"; install -m 600 /dev/null "$hardlink_root/postgres-admin-password"; printf x > "$hardlink_root/postgres-admin-password"; ln "$hardlink_root/postgres-admin-password" "$test_root/second-link"
if "$script_dir/generate-secrets.sh" "$hardlink_root" admin@example.test >/dev/null 2>&1; then printf 'multiply-linked secret was accepted\n' >&2; exit 1; fi
printf 'identity secret generation qualification: ok\n'
