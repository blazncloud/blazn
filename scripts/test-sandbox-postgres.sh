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
BEGIN;
INSERT INTO sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','1',convert_to('{"version":"1"}','UTF8'),'{"version":"1","variants":[{"name":"linux-amd64","architecture":"amd64"}],"repositories":[{"name":"source","destination":"/workspace/src/blazn"}],"artifacts":[{"name":"patch","path":"/workspace/artifacts/change.patch"}]}',repeat('b',64),'10000000-0000-4000-8000-000000000001');
INSERT INTO sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','linux-amd64','amd64','registry.invalid/poc@sha256:'||repeat('c',64),'registry.invalid/poc@sha256:'||repeat('d',64),'poc-linux-amd64-v1');
INSERT INTO sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','source','https://github.com/blazncloud/blazn.git','/workspace/src/blazn',true);
INSERT INTO sandbox_template_version_artifacts(version_id,workspace_id,template_id,name,path,media_type,required) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','patch','/workspace/artifacts/change.patch','text/plain',true);
INSERT INTO sandbox_template_version_status(version_id,workspace_id,template_id,status,changed_by) VALUES
 ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','published','10000000-0000-4000-8000-000000000001');
UPDATE sandbox_templates SET current_published_version_id='60000000-0000-4000-8000-000000000001' WHERE id='50000000-0000-4000-8000-000000000001';
COMMIT;

SET ROLE blazn_runtime;
BEGIN;
INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,artifact_contract_digest,isolation,approved_non_sensitive,expires_at) VALUES
 ('70000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','coding-small','1',repeat('b',64),'linux-amd64','registry.invalid/poc@sha256:'||repeat('c',64),'registry.invalid/poc@sha256:'||repeat('d',64),'amd64','direct','requested','ready','poc-local',repeat('9',64),'approved-non-sensitive-poc',true,now()+interval '15 minutes');
INSERT INTO sandbox_sources(sandbox_id,workspace_id,template_version_id,repository_name,commit) VALUES
 ('70000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','source',repeat('1',40));
INSERT INTO sandbox_artifact_contract_entries(sandbox_id,workspace_id,template_version_id,name,path,media_type,required) VALUES
 ('70000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','patch','/workspace/artifacts/change.patch','text/plain',true);
COMMIT;
INSERT INTO sandbox_access_grants(id,workspace_id,sandbox_id,user_id,session_id,scope,kind,token_hash,token_key_id,state,expires_at) VALUES
 ('80000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','30000000-0000-4000-8000-000000000001','sandbox.exec','exec',repeat('e',64),'sandbox-access-grant/v1','active',now()+interval '30 seconds');
DO $$ BEGIN
  IF NOT sandbox_consume_access_grant('80000000-0000-4000-8000-000000000001', repeat('e',64), 'exec', now()) THEN RAISE EXCEPTION 'atomic grant consume failed'; END IF;
  IF sandbox_consume_access_grant('80000000-0000-4000-8000-000000000001', repeat('e',64), 'exec', now()) THEN RAISE EXCEPTION 'consumed grant replay succeeded'; END IF;
END $$;
INSERT INTO sandbox_access_grants(id,workspace_id,sandbox_id,user_id,session_id,scope,kind,token_hash,token_key_id,state,expires_at) VALUES
 ('80000000-0000-4000-8000-000000000002','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','30000000-0000-4000-8000-000000000001','sandbox.download','download',repeat('d',64),'sandbox-access-grant/v1','active',now()+interval '30 seconds');
DO $$ BEGIN
  IF sandbox_revoke_access_grants('40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001',now()) <> 1 THEN RAISE EXCEPTION 'atomic grant revoke failed'; END IF;
END $$;

INSERT INTO sandbox_operations(id,workspace_id,sandbox_id,type,status,expected_sandbox_version,requested_by,idempotency_key,request_digest) VALUES
 ('90000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001','stop','pending',1,'10000000-0000-4000-8000-000000000001','stop-request-1',repeat('3',64)),
 ('90000000-0000-4000-8000-000000000002','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001','delete','pending',1,'10000000-0000-4000-8000-000000000001','delete-request-1',repeat('4',64));
INSERT INTO sandbox_events(id,operation_id,workspace_id,sandbox_id,sequence,type) VALUES
 ('91000000-0000-4000-8000-000000000001','90000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001',0,'sandbox.stop.requested');
DO $$ BEGIN
  BEGIN INSERT INTO sandbox_events(id,operation_id,workspace_id,sandbox_id,sequence,type) VALUES ('91000000-0000-4000-8000-000000000002','90000000-0000-4000-8000-000000000002','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001',0,'sandbox.delete.requested'); RAISE EXCEPTION 'sandbox-wide duplicate event sequence succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
END $$;
DO $$ BEGIN
  BEGIN INSERT INTO sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,status,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed) VALUES ('92000000-0000-4000-8000-000000000001','90000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','70000000-0000-4000-8000-000000000001','succeeded',false,true,true,true); RAISE EXCEPTION 'incomplete succeeded receipt was accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

DO $$ BEGIN
  BEGIN UPDATE sandbox_template_versions SET version='changed' WHERE id='60000000-0000-4000-8000-000000000001'; RAISE EXCEPTION 'runtime changed immutable version';
  EXCEPTION WHEN insufficient_privilege OR object_not_in_prerequisite_state THEN NULL; END;
END $$;

DO $$ BEGIN
  BEGIN INSERT INTO sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','duplicate-amd64','amd64','registry.invalid/poc@sha256:'||repeat('5',64),'registry.invalid/poc@sha256:'||repeat('6',64),'poc-linux-amd64-v1'); RAISE EXCEPTION 'duplicate architecture unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','dot-path','https://example.invalid/repo.git','/workspace/src/../escape',false); RAISE EXCEPTION 'repository dot segment unexpectedly succeeded';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','duplicate-path','https://example.invalid/repo.git','/workspace/src/blazn',false); RAISE EXCEPTION 'duplicate repository path unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','source','https://example.invalid/repo.git','/workspace/src/other',false); RAISE EXCEPTION 'duplicate repository name unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_template_version_artifacts(version_id,workspace_id,template_id,name,path,media_type,required) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','dot-artifact','/workspace/artifacts/./escape','text/plain',false); RAISE EXCEPTION 'artifact dot segment unexpectedly succeeded';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_template_version_artifacts(version_id,workspace_id,template_id,name,path,media_type,required) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','duplicate-path','/workspace/artifacts/change.patch','text/plain',false); RAISE EXCEPTION 'duplicate artifact path unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_template_version_artifacts(version_id,workspace_id,template_id,name,path,media_type,required) VALUES ('60000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','patch','/workspace/artifacts/other.patch','text/plain',false); RAISE EXCEPTION 'duplicate artifact name unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_sources(sandbox_id,workspace_id,template_version_id,repository_name,commit) VALUES ('70000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','source',repeat('7',40)); RAISE EXCEPTION 'duplicate source repository unexpectedly succeeded';
  EXCEPTION WHEN unique_violation THEN NULL; END;
  BEGIN INSERT INTO sandbox_sources(sandbox_id,workspace_id,template_version_id,repository_name,commit) VALUES ('70000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','unknown',repeat('7',40)); RAISE EXCEPTION 'unknown source repository unexpectedly succeeded';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;

DO $$ BEGIN
  BEGIN
    INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,artifact_contract_digest,isolation,approved_non_sensitive,expires_at) VALUES ('70000000-0000-4000-8000-000000000003','40000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','coding-small','1',repeat('b',64),'linux-amd64','registry.invalid/poc@sha256:'||repeat('c',64),'registry.invalid/poc@sha256:'||repeat('d',64),'amd64','direct','requested','ready','poc-local',repeat('9',64),'approved-non-sensitive-poc',true,now()+interval '15 minutes');
    EXECUTE 'SET CONSTRAINTS sandbox_create_children_complete IMMEDIATE';
    RAISE EXCEPTION 'missing source coverage unexpectedly succeeded';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

DO $$ BEGIN
  BEGIN
    INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,artifact_contract_digest,isolation,approved_non_sensitive,expires_at) VALUES
     ('70000000-0000-4000-8000-000000000002','40000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000001','60000000-0000-4000-8000-000000000001','coding-small','1',repeat('b',64),'linux-amd64','registry.invalid/poc@sha256:'||repeat('c',64),'registry.invalid/poc@sha256:'||repeat('d',64),'amd64','direct','requested','ready','poc-local',repeat('9',64),'approved-non-sensitive-poc',true,now()+interval '15 minutes');
    RAISE EXCEPTION 'cross-workspace version binding unexpectedly succeeded';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;

DO $$ DECLARE secret_key text; BEGIN
  FOREACH secret_key IN ARRAY ARRAY['apiKey','api_key','private-key','clientSecret','client_secret','sessionToken','bearer-token','signing.key'] LOOP
    BEGIN INSERT INTO sandbox_templates(id,workspace_id,name,draft_spec,draft_digest,created_by) VALUES (gen_random_uuid(),'40000000-0000-4000-8000-000000000001','secret-'||lower(regexp_replace(secret_key,'[^a-zA-Z0-9]','','g')),jsonb_build_object('nested',jsonb_build_object(secret_key,'forbidden')),repeat('f',64),'10000000-0000-4000-8000-000000000001'); RAISE EXCEPTION 'secret-bearing JSON unexpectedly succeeded for %',secret_key;
    EXCEPTION WHEN check_violation THEN NULL; END;
  END LOOP;
END $$;

INSERT INTO sandbox_idempotency_receipts(principal_id,workspace_id,operation,idempotency_key,request_digest,response_status,response_body) VALUES ('10000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','sandbox.create','request-unique-1',repeat('1',64),202,'{"sandboxId":"70000000-0000-4000-8000-000000000001"}');
DO $$ BEGIN
  BEGIN INSERT INTO sandbox_idempotency_receipts(principal_id,workspace_id,operation,idempotency_key,request_digest,response_status,response_body) VALUES ('10000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001','sandbox.access_grant.create','grant-request-1',repeat('8',64),201,'{}'); RAISE EXCEPTION 'grant response entered idempotency receipts';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;
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
  BEGIN UPDATE sandbox_access_grants SET state='active', consumed_at=NULL WHERE id='80000000-0000-4000-8000-000000000001'; RAISE EXCEPTION 'consumed grant was revived';
  EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
  BEGIN UPDATE sandbox_access_grants SET revoked_at=NULL WHERE id='80000000-0000-4000-8000-000000000002'; RAISE EXCEPTION 'terminal grant timestamp was cleared';
  EXCEPTION WHEN object_not_in_prerequisite_state OR check_violation THEN NULL; END;
END $$;

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.role_table_grants WHERE grantee='blazn_runtime' AND table_name='sandbox_template_versions' AND privilege_type IN ('UPDATE','DELETE')) THEN RAISE EXCEPTION 'runtime can mutate immutable versions'; END IF;
  IF EXISTS (SELECT 1 FROM information_schema.role_table_grants WHERE grantee IN ('blazn_bootstrap','blazn_node_broker') AND table_name LIKE 'sandbox_%') THEN RAISE EXCEPTION 'untrusted role has sandbox table grants'; END IF;
  IF (SELECT token_hash FROM sandbox_access_grants WHERE id='80000000-0000-4000-8000-000000000001') <> repeat('e',64) THEN RAISE EXCEPTION 'grant token is not hash-only'; END IF;
END $$;
SQL
