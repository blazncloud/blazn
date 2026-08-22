#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$NODE_ROOT/../.." && pwd)
command -v docker >/dev/null 2>&1 || { printf 'Node PostgreSQL privilege test skipped: docker unavailable\n'; exit 0; }

image=${POSTGRES_IMAGE:-postgres:17.6@sha256:00bc86618629af00d2937fdc5a5d63db3ff8450acf52f0636ec813c7f4902929}
case "$image" in *@sha256:????????????????????????????????????????????????????????????????) ;; *) printf 'POSTGRES_IMAGE must be immutable\n' >&2; exit 1 ;; esac
suffix=$$
network=blazn-node-pg-$suffix
container=blazn-node-postgres-$suffix
tmp=${TMPDIR:-/tmp}/blazn-node-pg-$suffix
mkdir "$tmp"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -f "$tmp/database-url" "$tmp/out" "$tmp/err"
  rmdir "$tmp" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

docker network create "$network" >/dev/null
docker run -d --name "$container" --network "$network" --network-alias postgres \
  -e POSTGRES_USER=blazn_admin -e POSTGRES_PASSWORD=test-admin-only -e POSTGRES_DB=blazn \
  --tmpfs /var/lib/postgresql/data:rw,noexec,nosuid,nodev "$image" >/dev/null
attempt=0
until docker exec "$container" pg_isready -U blazn_admin -d blazn >/dev/null 2>&1; do
  attempt=$((attempt + 1)); [ "$attempt" -lt 60 ] || { printf 'disposable PostgreSQL did not become ready\n' >&2; exit 1; }; sleep 1
done

docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn >/dev/null <<'SQL'
CREATE ROLE blazn_migration LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '1111111111111111111111111111111111111111111111111111111111111111';
CREATE ROLE blazn_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '2222222222222222222222222222222222222222222222222222222222222222';
CREATE ROLE blazn_bootstrap LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '3333333333333333333333333333333333333333333333333333333333333333';
REVOKE ALL ON DATABASE blazn FROM PUBLIC;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE blazn TO blazn_migration, blazn_runtime, blazn_bootstrap;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime, blazn_bootstrap;
SQL
for migration in 001_auth.sql 002_auth_role_grants.sql 003_workspaces.sql; do
  docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn <"$REPO_ROOT/services/control-api/migrations/$migration" >/dev/null
done
if docker exec -i "$container" psql -X -1 -v ON_ERROR_STOP=1 -U blazn_admin -d blazn <"$REPO_ROOT/services/control-api/migrations/004_nodes.sql" >"$tmp/out" 2>"$tmp/err"; then
  printf 'migration 004 unexpectedly passed without the broker role\n' >&2
  exit 1
fi
grep -F 'role "blazn_node_broker" does not exist' "$tmp/err" >/dev/null
[ "$(docker exec "$container" psql -X -U blazn_admin -d blazn -Atqc "select to_regclass('public.nodes') is null")" = t ] || { printf 'failed migration 004 left partial schema\n' >&2; exit 1; }

docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn >/dev/null <<'SQL'
CREATE ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '4444444444444444444444444444444444444444444444444444444444444444';
GRANT CONNECT ON DATABASE blazn TO blazn_node_broker;
GRANT USAGE ON SCHEMA public TO blazn_node_broker;
SQL
printf 'postgresql://blazn_node_broker:4444444444444444444444444444444444444444444444444444444444444444@postgres:5432/blazn\n' >"$tmp/database-url"
chmod 0444 "$tmp/database-url"
verify() {
  mode=$1
  docker run --rm --network "$network" \
    -e NODE_BROKER_DATABASE_URL_FILE=/run/secrets/node_broker_database_url \
    -v "$tmp/database-url:/run/secrets/node_broker_database_url:ro" \
    -v "$NODE_ROOT/scripts/verify-database.sh:/verify-database.sh:ro" \
    "$image" /bin/sh /verify-database.sh "$mode"
}
verify pre-migration >/dev/null
docker exec -i "$container" psql -X -1 -v ON_ERROR_STOP=1 -U blazn_admin -d blazn <"$REPO_ROOT/services/control-api/migrations/004_nodes.sql" >/dev/null
verify post-migration >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'GRANT SELECT ON node_identities TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'forbidden broker table grant unexpectedly passed\n' >&2; exit 1; fi
grep -F 'privilege matrix differs' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'REVOKE SELECT ON node_identities FROM blazn_node_broker; ALTER ROLE blazn_node_broker CREATEDB' >/dev/null
if verify pre-migration >"$tmp/out" 2>"$tmp/err"; then printf 'administrative broker attribute unexpectedly passed\n' >&2; exit 1; fi
grep -F 'role attributes are not least privilege' "$tmp/err" >/dev/null

printf 'Node broker disposable PostgreSQL positive/negative privilege matrix passed\n'
