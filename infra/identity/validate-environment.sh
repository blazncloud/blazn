#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  printf 'usage: %s OWNER_ONLY_ENV_FILE\n' "$0" >&2
  exit 64
fi
env_file=$1
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/identity/lib.sh
. "$script_dir/lib.sh"
identity_require_root_file "$env_file"
case "$(stat -c '%u:%a:%h' -- "$env_file")" in 0:400:1|0:600:1) ;; *) identity_fail "identity environment file must be mode 0400 or 0600" ;; esac

for name in ZITADEL_POSTGRES_IMAGE ZITADEL_TRAEFIK_IMAGE ZITADEL_IMAGE ZITADEL_LOGIN_IMAGE ZITADEL_BACKUP_IMAGE; do
	[ "$(grep -c "^${name}=" "$env_file")" -eq 1 ] || { printf '%s must occur exactly once\n' "$name" >&2; exit 65; }
  value=$(sed -n "s/^${name}=//p" "$env_file")
  printf '%s' "$value" | grep -Eq '^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$' || {
    printf '%s must be a reviewed immutable repository@sha256 digest\n' "$name" >&2
    exit 65
  }
done

data_root=$(sed -n 's/^BLAZN_IDENTITY_DATA_ROOT=//p' "$env_file")
secrets_root=$(sed -n 's/^BLAZN_IDENTITY_SECRETS_ROOT=//p' "$env_file")
if [ "$(grep -c '^BLAZN_IDENTITY_DATA_ROOT=' "$env_file")" -ne 1 ] || [ "$(grep -c '^BLAZN_IDENTITY_SECRETS_ROOT=' "$env_file")" -ne 1 ]; then
  printf 'identity roots must occur exactly once\n' >&2
  exit 65
fi
case "$data_root:$secrets_root" in /*:/*) ;; *) printf 'identity data and secret roots must be absolute\n' >&2; exit 65 ;; esac
identity_validate_path "$data_root" data
identity_validate_path "$secrets_root" secrets
identity_reject_overlap "$data_root" "$secrets_root"
printf 'identity environment contract: ok\n'
