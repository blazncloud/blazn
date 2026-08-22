#!/bin/sh
set -eu

migration_url=$(cat /run/secrets/migration_database_url)
runtime_url=$(cat /run/secrets/runtime_database_url)
migration_password=${migration_url#*://*:}
migration_password=${migration_password%%@*}
runtime_password=${runtime_url#*://*:}
runtime_password=${runtime_password%%@*}

case "$migration_password:$runtime_password" in
  *[!a-f0-9:]*)
    printf 'database role passwords must use the generated lowercase hexadecimal form\n' >&2
    exit 1
    ;;
esac
[ "${#migration_password}" -eq 64 ] && [ "${#runtime_password}" -eq 64 ] || {
  printf 'database role passwords have an unexpected length\n' >&2
  exit 1
}

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=migration_password="$migration_password" \
  --set=runtime_password="$runtime_password" \
  --set=database_name="$POSTGRES_DB" <<'SQL'
CREATE ROLE blazn_migration
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
  PASSWORD :'migration_password';
CREATE ROLE blazn_runtime
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
  PASSWORD :'runtime_password';

ALTER DATABASE :"database_name" OWNER TO blazn_migration;
REVOKE ALL ON DATABASE :"database_name" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"database_name" TO blazn_runtime;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime;

ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO blazn_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO blazn_runtime;
SQL
