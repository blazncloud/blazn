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
CREATE ROLE unrelated_login LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '5555555555555555555555555555555555555555555555555555555555555555';
ALTER DATABASE blazn OWNER TO blazn_migration;
REVOKE ALL ON DATABASE blazn FROM PUBLIC;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE blazn TO blazn_migration, blazn_runtime, blazn_bootstrap;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime, blazn_bootstrap;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
SQL
for migration in 001_auth.sql 002_auth_role_grants.sql 003_workspaces.sql; do
  docker exec -i "$container" env PGPASSWORD=1111111111111111111111111111111111111111111111111111111111111111 psql -X -v ON_ERROR_STOP=1 -h 127.0.0.1 -U blazn_migration -d blazn <"$REPO_ROOT/services/control-api/migrations/$migration" >/dev/null
done
if docker exec -i "$container" psql -X -1 -v ON_ERROR_STOP=1 -U blazn_admin -d blazn <"$REPO_ROOT/services/control-api/migrations/004_nodes.sql" >"$tmp/out" 2>"$tmp/err"; then
  printf 'migration 004 unexpectedly passed without the broker role\n' >&2
  exit 1
fi
grep -F 'role "blazn_node_broker" does not exist' "$tmp/err" >/dev/null
[ "$(docker exec "$container" psql -X -U blazn_admin -d blazn -Atqc "select to_regclass('public.nodes') is null")" = t ] || { printf 'failed migration 004 left partial schema\n' >&2; exit 1; }

if docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn >"$tmp/out" 2>"$tmp/err" <<'SQL'; then
BEGIN;
CREATE ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
SELECT 1/0;
COMMIT;
SQL
  printf 'failing role-setup transaction unexpectedly passed\n' >&2; exit 1
fi
[ "$(docker exec "$container" psql -X -U blazn_admin -d blazn -Atqc "select count(*) from pg_roles where rolname='blazn_node_broker'")" = 0 ] || { printf 'failing role setup left a partial role\n' >&2; exit 1; }

setup_broker() {
  docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn >/dev/null <<'SQL'
BEGIN;
DO $block$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='blazn_node_broker') THEN EXECUTE 'CREATE ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS'; END IF; END $block$;
DO $preserve$ DECLARE database_row record; role_row record; BEGIN FOR database_row IN SELECT oid,datname FROM pg_database WHERE datallowconn LOOP FOR role_row IN SELECT oid,rolname FROM pg_roles WHERE rolcanlogin AND rolname <> 'blazn_node_broker' AND has_database_privilege(oid,database_row.oid,'CONNECT') LOOP EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I',database_row.datname,role_row.rolname); IF has_database_privilege(role_row.oid,database_row.oid,'TEMP') THEN EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO %I',database_row.datname,role_row.rolname); END IF; END LOOP; EXECUTE format('REVOKE CONNECT, TEMPORARY ON DATABASE %I FROM PUBLIC',database_row.datname); END LOOP; END $preserve$;
REVOKE ALL PRIVILEGES ON DATABASE blazn FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM blazn_node_broker;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC;
GRANT EXECUTE ON FUNCTION workspace_json_contains_secret_key(jsonb) TO blazn_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON TABLES FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON SEQUENCES FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM blazn_node_broker;
ALTER ROLE blazn_node_broker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '4444444444444444444444444444444444444444444444444444444444444444';
GRANT CONNECT ON DATABASE blazn TO blazn_node_broker;
GRANT USAGE ON SCHEMA public TO blazn_node_broker;
COMMIT;
SQL
}
setup_broker
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
[ "$(docker exec "$container" psql -X -U blazn_admin -d blazn -Atqc "select has_database_privilege('unrelated_login','postgres','CONNECT'),has_database_privilege('unrelated_login','template1','CONNECT'),has_database_privilege('unrelated_login','postgres','TEMP'),has_database_privilege('blazn_node_broker','postgres','CONNECT'),has_database_privilege('blazn_node_broker','template1','CONNECT'),has_database_privilege('blazn_node_broker','postgres','TEMP')")" = 't|t|t|f|f|f' ] || { printf 'database PUBLIC conversion changed unrelated access or retained broker access\n' >&2; exit 1; }
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'GRANT SELECT ON users TO blazn_node_broker' >/dev/null
setup_broker
verify pre-migration >/dev/null

emit_rollback_sql() {
  cat <<'SQL'
BEGIN;
REASSIGN OWNED BY blazn_node_broker TO blazn_migration;
DROP OWNED BY blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON SCHEMA public FROM blazn_node_broker;
REVOKE ALL PRIVILEGES ON DATABASE blazn FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON TABLES FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE ALL ON SEQUENCES FROM blazn_node_broker;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE EXECUTE ON FUNCTIONS FROM blazn_node_broker;
DO $revoke$ DECLARE database_row record; BEGIN FOR database_row IN SELECT datname FROM pg_database LOOP EXECUTE format('REVOKE ALL PRIVILEGES ON DATABASE %I FROM blazn_node_broker',database_row.datname); END LOOP; END $revoke$;
DROP ROLE blazn_node_broker;
SQL
}
if { emit_rollback_sql; printf 'SELECT 1/0;\nCOMMIT;\n'; } | docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn >"$tmp/out" 2>"$tmp/err"; then printf 'failing rollback transaction unexpectedly passed\n' >&2; exit 1; fi
[ "$(docker exec "$container" psql -X -U blazn_admin -d blazn -Atqc "select count(*) from pg_roles where rolname='blazn_node_broker'")" = 1 ] || { printf 'interrupted rollback did not roll back DROP ROLE\n' >&2; exit 1; }
{ emit_rollback_sql; printf 'COMMIT;\n'; } | docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn >/dev/null
[ "$(docker exec "$container" psql -X -U blazn_admin -d blazn -Atqc "select count(*) from pg_roles where rolname='blazn_node_broker'")" = 0 ] || { printf 'completed rollback left the role\n' >&2; exit 1; }
setup_broker

for migration in 004_nodes.sql 005_node_broker_security.sql; do
  docker exec -i "$container" env PGPASSWORD=1111111111111111111111111111111111111111111111111111111111111111 psql -X -1 -v ON_ERROR_STOP=1 -h 127.0.0.1 -U blazn_migration -d blazn <"$REPO_ROOT/services/control-api/migrations/$migration" >/dev/null
done
verify post-migration >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'GRANT CONNECT ON DATABASE postgres TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'unexpected postgres database grant passed\n' >&2; exit 1; fi
grep -F 'effective database privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'REVOKE CONNECT ON DATABASE postgres FROM blazn_node_broker' >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'CREATE DATABASE broker_extra' >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'GRANT CONNECT ON DATABASE broker_extra TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'unexpected database grant passed\n' >&2; exit 1; fi
grep -F 'effective database privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'REVOKE CONNECT ON DATABASE broker_extra FROM blazn_node_broker' >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'DROP DATABASE broker_extra' >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'CREATE SCHEMA broker_extra; GRANT USAGE ON SCHEMA broker_extra TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'unexpected schema grant passed\n' >&2; exit 1; fi
grep -F 'effective schema privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'DROP SCHEMA broker_extra' >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'CREATE SEQUENCE broker_extra_sequence; GRANT USAGE ON SEQUENCE broker_extra_sequence TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'unexpected sequence grant passed\n' >&2; exit 1; fi
grep -F 'effective sequence privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'DROP SEQUENCE broker_extra_sequence' >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'GRANT EXECUTE ON FUNCTION workspace_json_contains_secret_key(jsonb) TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'unexpected function grant passed\n' >&2; exit 1; fi
grep -F 'effective function privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'REVOKE EXECUTE ON FUNCTION workspace_json_contains_secret_key(jsonb) FROM blazn_node_broker' >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public GRANT SELECT ON TABLES TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'unexpected default privilege passed\n' >&2; exit 1; fi
grep -F 'effective default privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public REVOKE SELECT ON TABLES FROM blazn_node_broker' >/dev/null

docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'GRANT SELECT ON node_identities TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'forbidden broker table grant unexpectedly passed\n' >&2; exit 1; fi
grep -F 'privilege matrix differs' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'REVOKE SELECT ON node_identities FROM blazn_node_broker; GRANT SELECT ON users TO blazn_node_broker' >/dev/null
if verify post-migration >"$tmp/out" 2>"$tmp/err"; then printf 'non-Node relation grant unexpectedly passed\n' >&2; exit 1; fi
grep -F 'unexpected effective relation privileges' "$tmp/err" >/dev/null
docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U blazn_admin -d blazn -c 'REVOKE SELECT ON users FROM blazn_node_broker; ALTER ROLE blazn_node_broker CREATEDB' >/dev/null
if verify pre-migration >"$tmp/out" 2>"$tmp/err"; then printf 'administrative broker attribute unexpectedly passed\n' >&2; exit 1; fi
grep -F 'role attributes are not least privilege' "$tmp/err" >/dev/null

printf 'Node broker disposable PostgreSQL positive/negative privilege matrix passed\n'
