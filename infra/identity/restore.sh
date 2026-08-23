#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ] || [ "$#" -ne 2 ]; then
  printf 'usage: sudo %s VERIFIED_BACKUP_DIRECTORY RESTORE_ENV_FILE\n' "$0" >&2
  exit 64
fi
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
backup_dir=$1; env_file=$2
[ -d "$backup_dir" ] && [ ! -L "$backup_dir" ] || { printf 'backup directory is unsafe\n' >&2; exit 73; }
(cd "$backup_dir" && sha256sum -c SHA256SUMS)
[ "$(cat "$backup_dir/format")" = 'blazn.identity.backup/v1' ] || { printf 'unsupported backup format\n' >&2; exit 65; }
"$script_dir/validate-environment.sh" "$env_file"
cmp -s "$env_file" "$backup_dir/identity.env" || { printf 'restore environment does not match the backed-up immutable image set\n' >&2; exit 65; }
for file in compose.yaml traefik.yaml traefik-routes.yaml zitadel-config.yaml; do
  cmp -s "$script_dir/$file" "$backup_dir/$file" || { printf 'checked-out identity definition differs from backup: %s\n' "$file" >&2; exit 65; }
done
for archive in secrets.tar zitadel-bootstrap.tar; do
  if tar -tf "$backup_dir/$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then printf 'backup archive contains an unsafe path: %s\n' "$archive" >&2; exit 73; fi
	if tar -tvf "$backup_dir/$archive" | awk 'substr($1,1,1)!="-" && substr($1,1,1)!="d" {found=1} END {exit !found}'; then printf 'backup archive contains a non-file entry: %s\n' "$archive" >&2; exit 73; fi
done
# The verified restore environment is explicit input.
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
case "$BLAZN_IDENTITY_DATA_ROOT:$BLAZN_IDENTITY_SECRETS_ROOT" in /*:/*) ;; *) exit 65 ;; esac

rollback_root=${BLAZN_IDENTITY_DATA_ROOT}.pre-restore.$(date -u +%Y%m%dT%H%M%SZ)
[ ! -e "$rollback_root" ] || { printf 'rollback path already exists\n' >&2; exit 73; }
docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" down -v
if [ -e "$BLAZN_IDENTITY_DATA_ROOT" ]; then mv -- "$BLAZN_IDENTITY_DATA_ROOT" "$rollback_root"; fi
mkdir -p -- "$BLAZN_IDENTITY_DATA_ROOT/postgres"; chown -R 999:999 -- "$BLAZN_IDENTITY_DATA_ROOT/postgres"; chmod 700 -- "$BLAZN_IDENTITY_DATA_ROOT/postgres"

secrets_parent=$(dirname -- "$BLAZN_IDENTITY_SECRETS_ROOT")
mkdir -p -- "$secrets_parent"
if [ -e "$BLAZN_IDENTITY_SECRETS_ROOT" ]; then mv -- "$BLAZN_IDENTITY_SECRETS_ROOT" "${BLAZN_IDENTITY_SECRETS_ROOT}.pre-restore.$(date -u +%Y%m%dT%H%M%SZ)"; fi
mkdir -m 700 -- "$BLAZN_IDENTITY_SECRETS_ROOT"; tar -C "$BLAZN_IDENTITY_SECRETS_ROOT" -xpf "$backup_dir/secrets.tar"
find "$BLAZN_IDENTITY_SECRETS_ROOT" -type f -exec chown 0:0 {} + -exec chmod 600 {} +

docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" up -d --wait postgres
docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" exec -T -u postgres postgres psql -v ON_ERROR_STOP=1 -U postgres -d postgres < "$backup_dir/postgres.sql"
docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/restore \
  --mount type=bind,src="$backup_dir",dst=/backup,readonly "$ZITADEL_BACKUP_IMAGE" \
  sh -ceu 'tar -C /restore -xpf /backup/zitadel-bootstrap.tar'
docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" up -d --wait
printf 'Identity restore completed. Pre-restore data remains recoverable at %s\n' "$rollback_root"
