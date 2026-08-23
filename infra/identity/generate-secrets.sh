#!/bin/sh
set -eu

if [ "$#" -ne 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
  printf 'usage: %s ABSOLUTE_SECRETS_DIRECTORY INITIAL_ADMIN_EMAIL\n' "$0" >&2
  exit 64
fi

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
OPENSSL_BIN=${OPENSSL_BIN:-/usr/bin/openssl}
if [ ! -x "$OPENSSL_BIN" ]; then
  printf 'openssl is required at %s (override with OPENSSL_BIN)\n' "$OPENSSL_BIN" >&2
  exit 69
fi
secrets_root=$1
admin_email=$2
case "$secrets_root" in
  /*) ;;
  *) printf 'secrets directory must be absolute\n' >&2; exit 64 ;;
esac
case "$admin_email" in
  *@*.*) ;;
  *) printf 'initial administrator email is invalid\n' >&2; exit 64 ;;
esac

umask 077
mkdir -p "$secrets_root"
chmod 700 "$secrets_root"
postgres_password_file=$secrets_root/postgres-admin-password
zitadel_password_file=$secrets_root/zitadel-database-password
masterkey_file=$secrets_root/zitadel-masterkey
admin_password_file=$secrets_root/initial-admin-password

generate_if_missing() {
  target=$1
  bytes=$2
  if [ ! -s "$target" ]; then
    temporary=${target}.tmp.$$
    "$OPENSSL_BIN" rand -base64 "$bytes" | tr -d '\n' > "$temporary"
    mv "$temporary" "$target"
  fi
}

generate_if_missing "$postgres_password_file" 32
generate_if_missing "$zitadel_password_file" 32
if [ ! -s "$masterkey_file" ]; then
  temporary=${masterkey_file}.tmp.$$
  LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 > "$temporary"
  mv "$temporary" "$masterkey_file"
fi
if [ ! -s "$admin_password_file" ]; then
  temporary=${admin_password_file}.tmp.$$
  printf 'Bz1!' > "$temporary"
  "$OPENSSL_BIN" rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 24 >> "$temporary"
  mv "$temporary" "$admin_password_file"
fi

postgres_password=$(sed -n '1p' "$postgres_password_file")
zitadel_password=$(sed -n '1p' "$zitadel_password_file")
admin_password=$(sed -n '1p' "$admin_password_file")

sed \
  -e "s|REPLACE_POSTGRES_ADMIN_PASSWORD|$postgres_password|g" \
  -e "s|REPLACE_ZITADEL_DATABASE_PASSWORD|$zitadel_password|g" \
  "$SCRIPT_DIR/zitadel-secrets.example.yaml" > "$secrets_root/zitadel-secrets.yaml"
sed \
  -e "s|REPLACE_INITIAL_ADMIN_PASSWORD|$admin_password|g" \
  -e "s|REPLACE_INITIAL_ADMIN_EMAIL|$admin_email|g" \
  "$SCRIPT_DIR/zitadel-steps.example.yaml" > "$secrets_root/zitadel-steps.yaml"

chmod 600 "$postgres_password_file" "$zitadel_password_file" "$masterkey_file" "$admin_password_file" "$secrets_root/zitadel-secrets.yaml" "$secrets_root/zitadel-steps.yaml"
printf 'Generated ZITADEL secrets in %s. Preserve the master key and remove the initial admin password after first-login rotation.\n' "$secrets_root"
