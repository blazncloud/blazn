#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ] || [ "$#" -ne 3 ]; then
  printf 'usage: sudo %s REVIEWED_ENV_FILE PAT_ARCHIVE EXPECTED_SHA256\n' "$0" >&2
  exit 64
fi
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$script_dir/lib.sh"
env_file=$1; archive=$2; expected=$3
"$script_dir/validate-environment.sh" "$env_file"
identity_validate_path "$archive" patarchive
identity_require_root_file "$archive"
printf '%s' "$expected" | grep -Eq '^sha256:[0-9a-f]{64}$' || identity_fail 'PAT archive checksum is invalid' 65
actual=sha256:$(sha256sum "$archive" | awk '{print $1}')
[ "$actual" = "$expected" ] || identity_fail 'PAT archive checksum mismatch' 65
if tar -tf "$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then identity_fail 'PAT archive contains an unsafe path'; fi
if tar -tvf "$archive" | awk 'substr($1,1,1)!="-" && substr($1,1,1)!="d" {found=1} END {exit !found}'; then identity_fail 'PAT archive contains a non-file entry'; fi
# The reviewed environment is explicit input.
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
docker volume rm -f blazn-identity_zitadel-bootstrap >/dev/null 2>&1 || true
docker volume create blazn-identity_zitadel-bootstrap >/dev/null
archive_dir=$(dirname -- "$archive"); archive_name=$(basename -- "$archive")
docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/restore \
  --mount type=bind,src="$archive_dir",dst=/backup,readonly "$ZITADEL_BACKUP_IMAGE" \
  sh -ceu 'tar -C /restore -xpf "/backup/$1"; test -s /restore/login-client.pat' sh "$archive_name"
printf 'PAT volume repair completed from verified snapshot\n'
