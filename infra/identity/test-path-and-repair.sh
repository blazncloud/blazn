#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || { printf 'root is required\n' >&2; exit 77; }
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$script_dir/lib.sh"
test_root=$(mktemp -d /tmp/blazn-identity-disposable.repair.XXXXXX)
symlink_root=/tmp/blazn-identity-disposable.symlink-test
cleanup() { rm -rf -- "$test_root" "$symlink_root"; }
trap cleanup EXIT HUP INT TERM

expect_fail() { if "$@" >/dev/null 2>&1; then printf 'unsafe case unexpectedly passed: %s\n' "$*" >&2; exit 1; fi; }
expect_fail sh -c ". '$script_dir/lib.sh'; identity_validate_path / data"
expect_fail sh -c ". '$script_dir/lib.sh'; identity_validate_path '/tmp/blazn-identity-disposable.x/data/../secrets' data"
expect_fail sh -c ". '$script_dir/lib.sh'; identity_reject_overlap '/var/lib/blazn/identity' '/var/lib/blazn/identity/nested'"
mkdir -m 700 "$test_root/actual"; ln -s "$test_root/actual" "$symlink_root"
expect_fail sh -c ". '$script_dir/lib.sh'; identity_validate_path '$symlink_root/data' data"

mkdir -m 700 "$test_root/data" "$test_root/secrets" "$test_root/bin" "$test_root/archive" "$test_root/volume"
env_file=$test_root/identity.env
cat > "$env_file" <<EOF
BLAZN_IDENTITY_DATA_ROOT=$test_root/data
BLAZN_IDENTITY_SECRETS_ROOT=$test_root/secrets
ZITADEL_POSTGRES_IMAGE=postgres@sha256:1111111111111111111111111111111111111111111111111111111111111111
ZITADEL_TRAEFIK_IMAGE=traefik@sha256:2222222222222222222222222222222222222222222222222222222222222222
ZITADEL_IMAGE=ghcr.io/zitadel/zitadel@sha256:3333333333333333333333333333333333333333333333333333333333333333
ZITADEL_LOGIN_IMAGE=ghcr.io/zitadel/zitadel-login@sha256:4444444444444444444444444444444444444444444444444444444444444444
ZITADEL_BACKUP_IMAGE=alpine@sha256:5555555555555555555555555555555555555555555555555555555555555555
EOF
chmod 600 "$env_file"
cat > "$test_root/bin/docker" <<'EOF'
#!/bin/sh
set -eu
case "$1:$2" in
  volume:rm|volume:create) exit 0 ;;
  run:*) for archive_name do :; done; rm -rf -- "$FAKE_VOLUME"/*; tar -C "$FAKE_VOLUME" -xpf "$FAKE_ARCHIVE_DIR/$archive_name"; test -s "$FAKE_VOLUME/login-client.pat" ;;
  *) exit 1 ;;
esac
EOF
chmod 700 "$test_root/bin/docker"
printf previous > "$test_root/volume/login-client.pat"; tar -C "$test_root/volume" -cpf "$test_root/archive/previous.tar" .
printf forward > "$test_root/volume/login-client.pat"; tar -C "$test_root/volume" -cpf "$test_root/archive/forward.tar" .
chmod 600 "$test_root/archive"/*.tar
export PATH=$test_root/bin:$PATH FAKE_VOLUME=$test_root/volume FAKE_ARCHIVE_DIR=$test_root/archive
previous_digest=sha256:$(sha256sum "$test_root/archive/previous.tar" | awk '{print $1}')
forward_digest=sha256:$(sha256sum "$test_root/archive/forward.tar" | awk '{print $1}')
"$script_dir/repair-pat-volume.sh" "$env_file" "$test_root/archive/previous.tar" "$previous_digest" >/dev/null
[ "$(cat "$test_root/volume/login-client.pat")" = previous ]
"$script_dir/repair-pat-volume.sh" "$env_file" "$test_root/archive/forward.tar" "$forward_digest" >/dev/null
[ "$(cat "$test_root/volume/login-client.pat")" = forward ]
printf 'identity path and PAT rollback/forward repair qualification: ok\n'
