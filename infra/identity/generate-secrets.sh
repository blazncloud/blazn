#!/bin/sh
set -eu

if [ "$#" -ne 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
  printf 'usage: %s ABSOLUTE_SECRETS_DIRECTORY INITIAL_ADMIN_EMAIL\n' "$0" >&2
  exit 64
fi
if [ "$(id -u)" -ne 0 ]; then printf 'secret generation must run as root\n' >&2; exit 77; fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=infra/identity/lib.sh
. "$script_dir/lib.sh"
openssl_bin=${OPENSSL_BIN:-/usr/bin/openssl}
[ -x "$openssl_bin" ] || { printf 'openssl is required at %s\n' "$openssl_bin" >&2; exit 69; }
secrets_root=$1
admin_email=$2
case "$secrets_root" in /*) ;; *) printf 'secrets directory must be absolute\n' >&2; exit 64 ;; esac
printf '%s' "$admin_email" | grep -Eq '^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,63}$' || { printf 'initial administrator email is invalid\n' >&2; exit 64; }

assert_secret_file() {
  target=$1
  if [ ! -f "$target" ] || [ -L "$target" ]; then
    printf 'secret is not a regular file: %s\n' "$target" >&2
    exit 73
  fi
  metadata=$(stat -c '%u:%a:%h' -- "$target")
  [ "$metadata" = '0:600:1' ] || { printf 'secret owner, mode, or link count is unsafe: %s (%s)\n' "$target" "$metadata" >&2; exit 73; }
  [ -s "$target" ] || { printf 'secret is empty: %s\n' "$target" >&2; exit 73; }
}

identity_validate_path "$secrets_root" secrets
umask 077
mkdir -p -- "$secrets_root"
if [ -L "$secrets_root" ] || [ ! -d "$secrets_root" ]; then
  printf 'secrets root is unsafe\n' >&2
  exit 73
fi
chown 0:0 -- "$secrets_root"; chmod 700 -- "$secrets_root"
[ "$(stat -c '%u:%a' -- "$secrets_root")" = '0:700' ] || { printf 'secrets root ownership is unsafe\n' >&2; exit 73; }

install_generated() {
  target=$1; generator=$2
  if [ -e "$target" ] || [ -L "$target" ]; then assert_secret_file "$target"; return; fi
  temporary=$(mktemp "$secrets_root/.secret.tmp.XXXXXX")
  trap 'test -z "${temporary:-}" || test ! -e "$temporary" || rm -- "$temporary"' EXIT HUP INT TERM
  case "$generator" in
    base64) "$openssl_bin" rand -base64 32 | tr -d '\n' > "$temporary" ;;
    base64url) "$openssl_bin" rand 32 | "$openssl_bin" base64 -A | tr '+/' '-_' | tr -d '=' > "$temporary" ;;
    masterkey) "$openssl_bin" rand -hex 16 > "$temporary" ;;
    admin) printf 'Bz1!' > "$temporary"; "$openssl_bin" rand -hex 16 >> "$temporary" ;;
    *) printf 'unknown secret generator\n' >&2; exit 70 ;;
  esac
  chown 0:0 -- "$temporary"; chmod 600 -- "$temporary"; assert_secret_file "$temporary"
  mv -- "$temporary" "$target"; temporary=; assert_secret_file "$target"
  sync -f "$target" 2>/dev/null || sync
  trap - EXIT HUP INT TERM
}

render_secret() {
  target=$1; template=$2; shift 2
  if [ -e "$target" ] || [ -L "$target" ]; then assert_secret_file "$target"; fi
  temporary=$(mktemp "$secrets_root/.render.tmp.XXXXXX")
  trap 'test -z "${temporary:-}" || test ! -e "$temporary" || rm -- "$temporary"' EXIT HUP INT TERM
  sed "$@" "$template" > "$temporary"
  chown 0:0 -- "$temporary"; chmod 600 -- "$temporary"; assert_secret_file "$temporary"
  mv -- "$temporary" "$target"; temporary=; assert_secret_file "$target"
  sync -f "$target" 2>/dev/null || sync
  trap - EXIT HUP INT TERM
}

postgres_password_file=$secrets_root/postgres-admin-password
zitadel_password_file=$secrets_root/zitadel-database-password
masterkey_file=$secrets_root/zitadel-masterkey
admin_password_file=$secrets_root/initial-admin-password
install_generated "$postgres_password_file" base64
install_generated "$zitadel_password_file" base64
install_generated "$masterkey_file" masterkey
install_generated "$admin_password_file" admin
install_generated "$secrets_root/zitadel-client-secret" base64url
install_generated "$secrets_root/oidc-cookie-key" base64url
install_generated "$secrets_root/provider-gate.pat" base64url

postgres_password=$(sed -n '1p' "$postgres_password_file")
zitadel_password=$(sed -n '1p' "$zitadel_password_file")
admin_password=$(sed -n '1p' "$admin_password_file")
render_secret "$secrets_root/zitadel-secrets.yaml" "$script_dir/zitadel-secrets.example.yaml" -e "s|REPLACE_POSTGRES_ADMIN_PASSWORD|$postgres_password|g" -e "s|REPLACE_ZITADEL_DATABASE_PASSWORD|$zitadel_password|g"
render_secret "$secrets_root/zitadel-steps.yaml" "$script_dir/zitadel-steps.example.yaml" -e "s|REPLACE_INITIAL_ADMIN_PASSWORD|$admin_password|g" -e "s|REPLACE_INITIAL_ADMIN_EMAIL|$admin_email|g"
sync -f "$secrets_root" 2>/dev/null || sync
printf 'Generated root-owned ZITADEL secrets. Preserve the master key and remove the initial administrator password after rotation.\n'
