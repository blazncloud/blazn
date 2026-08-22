#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
node_image=node:22.19.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90
postgres_image=postgres:17.6@sha256:00bc86618629af00d2937fdc5a5d63db3ff8450acf52f0636ec813c7f4902929
runtime_password=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
migration_password=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
bootstrap_password=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
admin_password=workspace-ci-admin
suffix=$$
network=blazn-workspace-pg-$suffix
postgres=blazn-workspace-pg-$suffix

command -v docker >/dev/null 2>&1 || {
  printf 'docker is required for the disposable workspace PostgreSQL test\n' >&2
  exit 1
}
case "$network:$postgres" in
  *[!a-z0-9:-]*) printf 'unsafe disposable PostgreSQL resource name\n' >&2; exit 1 ;;
esac

cleanup() {
  docker rm -f -v "$postgres" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

docker network create "$network" >/dev/null
docker run -d --name "$postgres" --network "$network" \
  -e POSTGRES_DB=blazn -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$admin_password" \
  "$postgres_image" >/dev/null

ready=false
stable=0
for _attempt in $(seq 1 60); do
  if docker exec -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn -Atqc 'select 1' >/dev/null 2>&1; then
    stable=$((stable + 1))
    if [ "$stable" -ge 3 ]; then ready=true; break; fi
  else
    stable=0
  fi
  sleep 1
done
[ "$ready" = true ] || { printf 'disposable PostgreSQL did not become ready\n' >&2; exit 1; }

docker exec -i -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn \
  --set=runtime_password="$runtime_password" --set=migration_password="$migration_password" --set=bootstrap_password="$bootstrap_password" <<'SQL'
CREATE ROLE blazn_migration LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'migration_password';
CREATE ROLE blazn_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'runtime_password';
CREATE ROLE blazn_bootstrap LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'bootstrap_password';
CREATE ROLE blazn_node_broker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER DATABASE blazn OWNER TO blazn_migration;
REVOKE ALL ON DATABASE blazn FROM PUBLIC;
GRANT CONNECT ON DATABASE blazn TO blazn_runtime;
GRANT CONNECT ON DATABASE blazn TO blazn_bootstrap;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime;
GRANT USAGE ON SCHEMA public TO blazn_bootstrap;
SQL

tar -C "$repo_root" -cf - services/control-api packages/contracts | docker run --rm -i --network "$network" \
  -e BLAZN_WORKSPACE_TEST_DATABASE_URL="postgresql://blazn_runtime:$runtime_password@$postgres:5432/blazn" \
  -e BLAZN_WORKSPACE_TEST_ADMIN_DATABASE_URL="postgresql://postgres:$admin_password@$postgres:5432/blazn" \
  -e WORKSPACE_MIGRATION_DATABASE_URL="postgresql://blazn_migration:$migration_password@$postgres:5432/blazn" \
  -e POC_BOOTSTRAP_DATABASE_URL="postgresql://blazn_bootstrap:$bootstrap_password@$postgres:5432/blazn" \
  "$node_image" sh -euc '
    mkdir /work
    tar -xf - -C /work
    cd /work/services/control-api
    npm ci
    npm run check
    npm run build
    printf "%s\n" "$WORKSPACE_MIGRATION_DATABASE_URL" >/tmp/migration-database-url
    MIGRATION_DATABASE_URL_FILE=/tmp/migration-database-url node dist/migrate.js
    node -e "const {Client}=require(\"pg\"); const c=new Client({connectionString:process.env.WORKSPACE_MIGRATION_DATABASE_URL}); c.connect().then(()=>c.query(\"INSERT INTO auth_rate_limits(key,window_start,count) VALUES(\$1,now(),1)\",[\"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\"])).then(()=>c.end())"
    node --test dist/workspace-store.integration.test.js
    printf "%s\n" "$POC_BOOTSTRAP_DATABASE_URL" >/tmp/bootstrap-database-url
    printf "%s\n" "$WORKSPACE_MIGRATION_DATABASE_URL" >/tmp/poc-cleanup-database-url
    printf "%s\n" poc-test-password-1234567890 >/tmp/poc-password
    printf "%s\n" "{\"login\":\"poc-second-ci@example.invalid\",\"displayName\":\"POC Second CI\"}" >/tmp/poc-profile.json
    printf "%s\n" "{\"workspaceIds\":[]}" >/tmp/poc-workspaces.json
    POC_IDENTITY_ACTION=provision POC_IDENTITY_DATABASE_URL_FILE=/tmp/bootstrap-database-url POC_IDENTITY_PASSWORD_FILE=/tmp/poc-password POC_IDENTITY_PROFILE_FILE=/tmp/poc-profile.json node dist/poc-identity.js >/tmp/poc-provision-1.json
    POC_IDENTITY_ACTION=provision POC_IDENTITY_DATABASE_URL_FILE=/tmp/bootstrap-database-url POC_IDENTITY_PASSWORD_FILE=/tmp/poc-password POC_IDENTITY_PROFILE_FILE=/tmp/poc-profile.json node dist/poc-identity.js >/tmp/poc-provision-2.json
    user_id=$(node -e "const fs=require(\"fs\"); const a=JSON.parse(fs.readFileSync(\"/tmp/poc-provision-1.json\")); const b=JSON.parse(fs.readFileSync(\"/tmp/poc-provision-2.json\")); if(a.userId!==b.userId||b.status!==\"existing\")process.exit(1); process.stdout.write(a.userId)")
    printf "%s\n" "$user_id" >/tmp/poc-user-id
    printf "%s\n" "{\"schemaVersion\":\"blazn.dev/poc-second-identity-cleanup/v1\",\"userId\":\"$user_id\",\"workspaceIds\":[]}" >/tmp/poc-cleanup-intent.json
    POC_IDENTITY_ACTION=cleanup POC_IDENTITY_DATABASE_URL_FILE=/tmp/poc-cleanup-database-url POC_IDENTITY_PASSWORD_FILE=/tmp/poc-password POC_IDENTITY_PROFILE_FILE=/tmp/poc-profile.json POC_IDENTITY_USER_ID_FILE=/tmp/poc-user-id POC_IDENTITY_WORKSPACES_FILE=/tmp/poc-workspaces.json POC_IDENTITY_CLEANUP_INTENT_FILE=/tmp/poc-cleanup-intent.json node dist/poc-identity.js >/tmp/poc-cleanup-1.json
    POC_IDENTITY_ACTION=cleanup POC_IDENTITY_DATABASE_URL_FILE=/tmp/poc-cleanup-database-url POC_IDENTITY_PASSWORD_FILE=/tmp/poc-password POC_IDENTITY_PROFILE_FILE=/tmp/poc-profile.json POC_IDENTITY_USER_ID_FILE=/tmp/poc-user-id POC_IDENTITY_WORKSPACES_FILE=/tmp/poc-workspaces.json POC_IDENTITY_CLEANUP_INTENT_FILE=/tmp/poc-cleanup-intent.json node dist/poc-identity.js >/tmp/poc-cleanup-2.json
    POC_IDENTITY_DATABASE_URL_FILE=/tmp/poc-cleanup-database-url POC_IDENTITY_CLEANUP_INTENT_FILE=/tmp/poc-cleanup-intent.json node dist/poc-identity-verify-cleanup.js >/tmp/poc-cleanup-verify.json
    node -e "const fs=require(\"fs\"); const a=JSON.parse(fs.readFileSync(\"/tmp/poc-cleanup-1.json\")); const b=JSON.parse(fs.readFileSync(\"/tmp/poc-cleanup-2.json\")); const c=JSON.parse(fs.readFileSync(\"/tmp/poc-cleanup-verify.json\")); if(a.status!==\"cleaned\"||b.status!==\"already-cleaned\"||c.status!==\"absent\")process.exit(1)"
    node -e "const {Client}=require(\"pg\"); const c=new Client({connectionString:process.env.WORKSPACE_MIGRATION_DATABASE_URL}); c.connect().then(()=>c.query(\"INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES(\$1,\$2,\$3,\$4,\$5)\",[process.argv[1],\"ambiguous@example.invalid\",\"Ambiguous\",\"salt\",\"hash\"])).then(()=>c.end())" "$user_id"
    if POC_IDENTITY_DATABASE_URL_FILE=/tmp/poc-cleanup-database-url POC_IDENTITY_CLEANUP_INTENT_FILE=/tmp/poc-cleanup-intent.json node dist/poc-identity-verify-cleanup.js >/tmp/poc-ambiguous.out 2>/tmp/poc-ambiguous.err; then exit 42; fi
    grep -F "ambiguous residual database state" /tmp/poc-ambiguous.err >/dev/null
    node -e "const {Client}=require(\"pg\"); const c=new Client({connectionString:process.env.WORKSPACE_MIGRATION_DATABASE_URL}); c.connect().then(()=>c.query(\"DELETE FROM users WHERE id=\$1\",[process.argv[1]])).then(()=>c.end())" "$user_id"
  '

rate_limit_count=$(docker exec -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn -Atqc \
  "select count(*) from auth_rate_limits where key='eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'")
[ "$rate_limit_count" = 1 ] || { printf 'hashed authentication rate-limit evidence was unexpectedly deleted by POC cleanup\n' >&2; exit 1; }
