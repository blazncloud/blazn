#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ] || [ "$#" -ne 2 ]; then
  printf 'usage: sudo %s OWNER_ONLY_ENV_FILE NEW_BACKUP_DIRECTORY\n' "$0" >&2
  exit 64
fi
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
env_file=$1; backup_dir=$2
"$script_dir/validate-environment.sh" "$env_file"
case "$backup_dir" in /*) ;; *) printf 'backup directory must be absolute\n' >&2; exit 64 ;; esac
[ ! -e "$backup_dir" ] && [ ! -L "$backup_dir" ] || { printf 'backup target must not exist\n' >&2; exit 73; }
umask 077; mkdir -m 700 -- "$backup_dir"
# The root-owned operator file is the explicit input.
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
docker compose --env-file "$env_file" -f "$script_dir/compose.yaml" exec -T -u postgres postgres pg_dumpall --clean --if-exists -U postgres > "$backup_dir/postgres.sql"
if find "$BLAZN_IDENTITY_SECRETS_ROOT" -mindepth 1 \( ! -type f -o ! -user root -o -perm /077 -o -links +1 \) -print -quit | grep -q .; then
  printf 'identity secret tree contains an unsafe entry\n' >&2; exit 73
fi
tar -C "$BLAZN_IDENTITY_SECRETS_ROOT" -cpf "$backup_dir/secrets.tar" .
docker run --rm --mount type=volume,src=blazn-identity_zitadel-bootstrap,dst=/source,readonly \
  --mount type=bind,src="$backup_dir",dst=/backup "$ZITADEL_BACKUP_IMAGE" \
  sh -ceu 'tar -C /source -cpf /backup/zitadel-bootstrap.tar .'
cp -- "$env_file" "$backup_dir/identity.env"
for file in compose.yaml traefik.yaml traefik-routes.yaml zitadel-config.yaml; do cp -- "$script_dir/$file" "$backup_dir/$file"; done
printf 'blazn.identity.backup/v1\n' > "$backup_dir/format"
printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$backup_dir/created-at"
(cd "$backup_dir" && sha256sum format created-at identity.env compose.yaml traefik.yaml traefik-routes.yaml zitadel-config.yaml postgres.sql secrets.tar zitadel-bootstrap.tar > SHA256SUMS)
chmod 600 "$backup_dir"/*; sync -f "$backup_dir" 2>/dev/null || sync
printf 'Identity backup completed at %s\n' "$backup_dir"
