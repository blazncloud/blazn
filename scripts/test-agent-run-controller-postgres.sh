#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
node_image=node:22.19.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90
postgres_image=postgres:17.6@sha256:00bc86618629af00d2937fdc5a5d63db3ff8450acf52f0636ec813c7f4902929
runtime_password=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
migration_password=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
controller_password=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
development_controller_password=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
sandbox_controller_password=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
admin_password=agent-run-controller-ci-admin
suffix=$$
network=blazn-agent-run-pg-$suffix
postgres=blazn-agent-run-pg-$suffix

command -v docker >/dev/null 2>&1 || { echo "docker is required for the disposable Agent Run controller PostgreSQL test" >&2; exit 1; }
case "$network:$postgres" in *[!a-z0-9:-]*) echo "unsafe disposable PostgreSQL resource name" >&2; exit 1 ;; esac
cleanup() {
  docker rm -f -v "$postgres" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

docker network create "$network" >/dev/null
docker run -d --name "$postgres" --network "$network" -e POSTGRES_DB=blazn -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$admin_password" "$postgres_image" >/dev/null
ready=false
stable=0
for _attempt in $(seq 1 60); do
  if docker exec -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn -Atqc 'select 1' >/dev/null 2>&1; then
    stable=$((stable + 1)); if [ "$stable" -ge 3 ]; then ready=true; break; fi
  else stable=0; fi
  sleep 1
done
[ "$ready" = true ] || { echo "disposable PostgreSQL did not become ready" >&2; exit 1; }

docker exec -i -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn --set=runtime_password="$runtime_password" --set=migration_password="$migration_password" --set=controller_password="$controller_password" --set=development_controller_password="$development_controller_password" --set=sandbox_controller_password="$sandbox_controller_password" <<'SQL'
CREATE ROLE blazn_migration LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'migration_password';
CREATE ROLE blazn_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'runtime_password';
CREATE ROLE blazn_bootstrap NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE blazn_node_broker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE blazn_sandbox_controller LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'sandbox_controller_password';
CREATE ROLE blazn_development_controller LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'development_controller_password';
CREATE ROLE blazn_agent_run_controller LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'controller_password';
ALTER DATABASE blazn OWNER TO blazn_migration;
REVOKE ALL ON DATABASE blazn FROM PUBLIC;
GRANT CONNECT ON DATABASE blazn TO blazn_runtime, blazn_sandbox_controller, blazn_development_controller, blazn_agent_run_controller;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime, blazn_sandbox_controller, blazn_development_controller, blazn_agent_run_controller;
SQL

tar -C "$repo_root" -cf - services/control-api packages/contracts | docker run --rm -i --network "$network" \
  -e BLAZN_AGENT_RUN_TEST_DATABASE_URL="postgresql://blazn_runtime:$runtime_password@$postgres:5432/blazn" \
  -e BLAZN_AGENT_RUN_TEST_ADMIN_DATABASE_URL="postgresql://postgres:$admin_password@$postgres:5432/blazn" \
  -e BLAZN_AGENT_RUN_CONTROLLER_TEST_DATABASE_URL="postgresql://blazn_agent_run_controller:$controller_password@$postgres:5432/blazn" \
  -e BLAZN_AGENT_RUN_DEVELOPMENT_CONTROLLER_TEST_DATABASE_URL="postgresql://blazn_development_controller:$development_controller_password@$postgres:5432/blazn" \
  -e BLAZN_AGENT_RUN_SANDBOX_CONTROLLER_TEST_DATABASE_URL="postgresql://blazn_sandbox_controller:$sandbox_controller_password@$postgres:5432/blazn" \
  -e MIGRATION_DATABASE_URL="postgresql://blazn_migration:$migration_password@$postgres:5432/blazn" \
  "$node_image" sh -euc '
    mkdir /work
    tar -xf - -C /work
    cd /work/services/control-api
    npm ci
    npm run check
    npm run build
    printf "%s\n" "$MIGRATION_DATABASE_URL" >/tmp/migration-database-url
    MIGRATION_DATABASE_URL_FILE=/tmp/migration-database-url node dist/migrate.js
    node --test dist/agent-run-controller-store.integration.test.js
  '
