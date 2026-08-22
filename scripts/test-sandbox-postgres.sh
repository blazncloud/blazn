#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
node_image=node:22.19.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90
postgres_image=postgres:17.6@sha256:00bc86618629af00d2937fdc5a5d63db3ff8450acf52f0636ec813c7f4902929
runtime_password=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
migration_password=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
bootstrap_password=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
admin_password=sandbox-ci-admin
suffix=$$
network=blazn-sandbox-pg-$suffix
postgres=blazn-sandbox-pg-$suffix

command -v docker >/dev/null 2>&1 || { printf 'docker is required for the disposable sandbox PostgreSQL test\n' >&2; exit 1; }
case "$network:$postgres" in *[!a-z0-9:-]*) printf 'unsafe disposable PostgreSQL resource name\n' >&2; exit 1 ;; esac
cleanup() { docker rm -f -v "$postgres" >/dev/null 2>&1 || true; docker network rm "$network" >/dev/null 2>&1 || true; }
trap cleanup EXIT HUP INT TERM

docker network create "$network" >/dev/null
docker run -d --name "$postgres" --network "$network" -e POSTGRES_DB=blazn -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$admin_password" "$postgres_image" >/dev/null
ready=false
stable=0
for _attempt in $(seq 1 60); do
  if docker exec -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn -Atqc 'select 1' >/dev/null 2>&1; then
    stable=$((stable + 1)); if [ "$stable" -ge 3 ]; then ready=true; break; fi
  else stable=0
  fi
  sleep 1
done
[ "$ready" = true ] || { printf 'disposable PostgreSQL did not become ready\n' >&2; exit 1; }

docker exec -i -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn --set=runtime_password="$runtime_password" --set=migration_password="$migration_password" --set=bootstrap_password="$bootstrap_password" <<'SQL'
CREATE ROLE blazn_migration LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'migration_password';
CREATE ROLE blazn_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'runtime_password';
CREATE ROLE blazn_bootstrap LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD :'bootstrap_password';
CREATE ROLE blazn_node_broker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER DATABASE blazn OWNER TO blazn_migration;
REVOKE ALL ON DATABASE blazn FROM PUBLIC;
GRANT CONNECT ON DATABASE blazn TO blazn_runtime, blazn_bootstrap;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime, blazn_bootstrap, blazn_node_broker;
SQL

tar -C "$repo_root" -cf - services/control-api | docker run --rm -i --network "$network" \
  -e SANDBOX_MIGRATION_DATABASE_URL="postgresql://blazn_migration:$migration_password@$postgres:5432/blazn" \
  "$node_image" sh -euc '
    mkdir /work
    tar -xf - -C /work
    cd /work/services/control-api
    npm ci
    npm run build
    printf "%s\n" "$SANDBOX_MIGRATION_DATABASE_URL" >/tmp/migration-url
    MIGRATION_DATABASE_URL_FILE=/tmp/migration-url node dist/migrate.js
    MIGRATION_DATABASE_URL_FILE=/tmp/migration-url node dist/migrate.js
  '

docker exec -i -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn <<'SQL'
DO $$ BEGIN
  IF (SELECT count(*) FROM schema_migrations) <> 9 THEN RAISE EXCEPTION 'expected exactly nine applied migrations'; END IF;
END $$;

INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES
 ('10000000-0000-4000-8000-000000000001','sandbox-owner@example.invalid','Sandbox Owner','salt','hash'),
 ('10000000-0000-4000-8000-000000000002','sandbox-other@example.invalid','Sandbox Other','salt','hash');
INSERT INTO devices(id,user_id,name,platform,public_key) VALUES
 ('20000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','fixture','linux','public');
INSERT INTO sessions(id,user_id,device_id,token_hash,refresh_token_hash,access_expires_at,refresh_expires_at) VALUES
 ('30000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','20000000-0000-4000-8000-000000000001','access-hash','refresh-hash',now()+interval '5 minutes',now()+interval '1 hour');
INSERT INTO workspaces(id,slug,name,created_by) VALUES
 ('40000000-0000-4000-8000-000000000001','sandbox-one','Sandbox One','10000000-0000-4000-8000-000000000001'),
 ('40000000-0000-4000-8000-000000000002','sandbox-two','Sandbox Two','10000000-0000-4000-8000-000000000002');
INSERT INTO sandbox_templates(id,workspace_id,name,draft_spec,draft_digest,created_by) VALUES
 ('50000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','coding-small','{"version":"1"}',repeat('a',64),'10000000-0000-4000-8000-000000000001');
INSERT INTO sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','1',convert_to('{"version":"1"}','UTF8'),'{"version":"1"}',repeat('b',64),'10000000-0000-4000-8000-000000000001');
INSERT INTO sandbox_template_version_status(version_id,workspace_id,template_id,status,changed_by) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','published','10000000-0000-4000-8000-000000000001');
UPDATE sandbox_templates SET current_published_version_id='60000000-0000-4000-8000-000000000001' WHERE id='50000000-0000-4000-8000-000000000001';

SET ROLE blazn_runtime;
INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,source_bindings,artifact_contract,isolation,approved_non_sensitive,expires_at) VALUES
 ('70000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','coding-small','1',repeat('b',64),'linux-amd64','registry.invalid/poc@sha256:'||repeat('c',64),'registry.invalid/poc@sha256:'||repeat('d',64),'amd64','direct','requested','ready','poc-local','[]','[]','approved-non-sensitive-poc',true,now()+interval '15 minutes');
INSERT INTO sandbox_access_grants(id,workspace_id,sandbox_id,user_id,session_id,scope,kind,token_hash,token_key_id,state,expires_at) VALUES
 ('80000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','30000000-0000-4000-8000-000000000001','sandbox.exec','exec',repeat('e',64),'sandbox-access-grant/v1','active',now()+interval '30 seconds');

DO $$ BEGIN
  BEGIN UPDATE sandbox_template_versions SET version='changed' WHERE id='60000000-0000-4000-8000-000000000001'; RAISE EXCEPTION 'runtime changed immutable version';
  EXCEPTION WHEN insufficient_privilege OR object_not_in_prerequisite_state THEN NULL; END;
END $$;

DO $$ BEGIN
  BEGIN
    INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,isolation,approved_non_sensitive,expires_at) VALUES
     ('70000000-0000-4000-8000-000000000002','40000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','coding-small','1',repeat('b',64),'linux-amd64','registry.invalid/poc@sha256:'||repeat('c',64),'registry.invalid/poc@sha256:'||repeat('d',64),'amd64','direct','requested','ready','poc-local','approved-non-sensitive-poc',true,now()+interval '15 minutes');
    RAISE EXCEPTION 'cross-workspace version binding unexpectedly succeeded';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;

DO $$ BEGIN
  BEGIN INSERT INTO sandbox_templates(id,workspace_id,name,draft_spec,draft_digest,created_by) VALUES ('50000000-0000-4000-8000-000000000002','40000000-0000-4000-8000-000000000001','secret-template','{"accessToken":"forbidden"}',repeat('f',64),'10000000-0000-4000-8000-000000000001'); RAISE EXCEPTION 'secret-bearing JSON unexpectedly succeeded';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

INSERT INTO sandbox_idempotency_receipts(principal_id,workspace_id,operation,idempotency_key,request_digest,response_status,response_body) VALUES ('10000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','sandbox.create','request-unique-1',repeat('1',64),202,'{"sandboxId":"70000000-0000-4000-8000-000000000001"}');
DO $$ BEGIN
  BEGIN INSERT INTO sandbox_idempotency_receipts(principal_id,workspace_id,operation,idempotency_key,request_digest,response_status,response_body) VALUES ('10000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','sandbox.create','request-unique-1',repeat('2',64),202,'{}'); RAISE EXCEPTION 'idempotency collision unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
END $$;
RESET ROLE;

DO $$ BEGIN
  BEGIN UPDATE sandbox_template_versions SET version='owner-change' WHERE id='60000000-0000-4000-8000-000000000001'; RAISE EXCEPTION 'table owner changed immutable version';
  EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
END $$;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.role_table_grants WHERE grantee='blazn_runtime' AND table_name='sandbox_template_versions' AND privilege_type IN ('UPDATE','DELETE')) THEN RAISE EXCEPTION 'runtime can mutate immutable versions'; END IF;
  IF EXISTS (SELECT 1 FROM information_schema.role_table_grants WHERE grantee IN ('blazn_bootstrap','blazn_node_broker') AND table_name LIKE 'sandbox_%') THEN RAISE EXCEPTION 'untrusted role has sandbox table grants'; END IF;
  IF (SELECT token_hash FROM sandbox_access_grants WHERE id='80000000-0000-4000-8000-000000000001') <> repeat('e',64) THEN RAISE EXCEPTION 'grant token is not hash-only'; END IF;
END $$;
SQL
