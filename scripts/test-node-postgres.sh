#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
postgres_image=postgres:17.6@sha256:00bc86618629af00d2937fdc5a5d63db3ff8450acf52f0636ec813c7f4902929
node_image=node:22.19.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90
admin_password=node-ci-admin
suffix=$$
network=blazn-node-pg-$suffix
postgres=blazn-node-pg-$suffix
node_runner=blazn-node-test-$suffix

command -v docker >/dev/null 2>&1 || {
  printf 'docker is required for the disposable Node PostgreSQL test\n' >&2
  exit 1
}
case "$network:$postgres:$node_runner" in
  *[!a-z0-9:-]*) printf 'unsafe disposable PostgreSQL resource name\n' >&2; exit 1 ;;
esac

cleanup() {
	if [ "$created_node_runner" = true ]; then
		docker rm -f "$node_runner" >/dev/null 2>&1 || true
	fi
	if [ "$created_postgres" = true ]; then
		docker rm -f "$postgres" >/dev/null 2>&1 || true
	fi
	if [ "$created_network" = true ]; then
		docker network rm "$network" >/dev/null 2>&1 || true
	fi
}
created_network=false
created_postgres=false
created_node_runner=false
trap cleanup EXIT HUP INT TERM

if docker network inspect "$network" >/dev/null 2>&1 || docker container inspect "$postgres" >/dev/null 2>&1 || docker container inspect "$node_runner" >/dev/null 2>&1; then
	printf 'refusing to reuse pre-existing disposable PostgreSQL resources\n' >&2
	exit 1
fi
docker network create "$network" >/dev/null
created_network=true
docker run -d --name "$postgres" --network "$network" \
  -e POSTGRES_DB=blazn -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$admin_password" \
  "$postgres_image" >/dev/null
created_postgres=true

ready=false
stable_checks=0
for _attempt in $(seq 1 60); do
  if docker exec -e PGPASSWORD="$admin_password" "$postgres" \
      psql -qAt -U postgres -d blazn -c 'SELECT 1' 2>/dev/null | grep -Fx 1 >/dev/null; then
    stable_checks=$((stable_checks + 1))
    if [ "$stable_checks" -ge 3 ]; then
      ready=true
      break
    fi
  else
    stable_checks=0
  fi
  sleep 1
done
[ "$ready" = true ] || { printf 'disposable PostgreSQL did not become ready\n' >&2; exit 1; }

psql_admin() {
  docker exec -i -e PGPASSWORD="$admin_password" "$postgres" psql -v ON_ERROR_STOP=1 -U postgres -d blazn
}

psql_admin <<'SQL'
CREATE ROLE blazn_migration NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE blazn_runtime NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE blazn_bootstrap NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE blazn_node_broker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER DATABASE blazn OWNER TO blazn_migration;
REVOKE ALL ON DATABASE blazn FROM PUBLIC;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO blazn_migration;
GRANT USAGE ON SCHEMA public TO blazn_runtime, blazn_bootstrap, blazn_node_broker;
GRANT CONNECT ON DATABASE blazn TO blazn_runtime;
SQL

for migration in "$repo_root"/services/control-api/migrations/*.sql; do
  {
    printf '%s\n' 'SET ROLE blazn_migration;'
    sed -n '1,$p' "$migration"
  } | psql_admin >/dev/null
done

psql_admin <<'SQL'
SET ROLE blazn_migration;

DO $$
DECLARE table_count integer;
BEGIN
  SELECT count(*) INTO table_count FROM pg_tables
    WHERE schemaname = 'public' AND tablename = ANY (ARRAY[
      'nodes', 'node_enrollments', 'node_identities', 'node_capability_versions',
      'node_heartbeat_state', 'node_install_plans', 'node_install_receipts',
      'node_operation_receipts', 'node_operations', 'node_operation_events',
      'node_join_issuances', 'node_join_issuance_intents', 'node_audit_events'
    ]);
  IF table_count <> 13 THEN RAISE EXCEPTION 'Node table count is %, want 13', table_count; END IF;
END $$;

INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES
  ('11111111-1111-4111-8111-111111111111','node-a@example.test','Node A','test','test'),
  ('22222222-2222-4222-8222-222222222222','node-b@example.test','Node B','test','test');
INSERT INTO workspaces(id,slug,name,created_by) VALUES
  ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','node-a','Node A','11111111-1111-4111-8111-111111111111'),
  ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','node-b','Node B','22222222-2222-4222-8222-222222222222');
INSERT INTO nodes(id,workspace_id,name,kind,owner_user_id,machine_fingerprint,host_platform,host_architecture,lifecycle_state,trust_state,service_version) VALUES
  ('33333333-3333-4333-8333-333333333333','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','worker-a','shared','11111111-1111-4111-8111-111111111111',repeat('a',64),'linux','amd64','pending','unverified','v0.1.0'),
  ('44444444-4444-4444-8444-444444444444','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','worker-b','shared','22222222-2222-4222-8222-222222222222',repeat('b',64),'linux','amd64','pending','unverified','v0.1.0');

DO $$
BEGIN
  BEGIN
    UPDATE nodes SET kubernetes_cluster_id='cluster-only' WHERE id='33333333-3333-4333-8333-333333333333';
    RAISE EXCEPTION 'partial Kubernetes binding accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    UPDATE nodes SET agent_eligible=true WHERE id='33333333-3333-4333-8333-333333333333';
    RAISE EXCEPTION 'ineligible Node accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

INSERT INTO node_enrollments(id,workspace_id,requested_name,mode,expected_platform,expected_architecture,token_hash,token_key_id,idempotency_key,created_by,expires_at,plan_signing_key_id,plan_signing_public_key,plan_signing_key_fingerprint) VALUES
  ('55555555-5555-4555-8555-555555555555','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','worker-a','fresh','linux','amd64',repeat('c',64),'node-enrollment/v1','enroll-key-a','11111111-1111-4111-8111-111111111111',now()+interval '10 minutes','node-plan/v1',repeat('P',43),repeat('e',64)),
  ('66666666-6666-4666-8666-666666666666','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','worker-b','fresh','linux','amd64',repeat('d',64),'node-enrollment/v1','enroll-key-b','22222222-2222-4222-8222-222222222222',now()+interval '10 minutes','node-plan/v1',repeat('Q',43),repeat('d',64));

INSERT INTO node_identities(id,node_id,public_key_fingerprint,public_key,signing_key_id,generation,status,issued_at,expires_at) VALUES
  ('77777777-7777-4777-8777-777777777777','33333333-3333-4333-8333-333333333333',repeat('e',64),repeat('A',43),'node-identity/v1',1,'active',now(),now()+interval '1 hour'),
  ('77777777-2222-4222-8222-222222222222','44444444-4444-4444-8444-444444444444',repeat('d',64),repeat('B',43),'node-identity-b/v1',1,'revoked',now(),now()+interval '1 hour');
INSERT INTO node_capability_versions(id,node_id,version,digest,payload,observed_at) VALUES
  ('88888888-1111-4111-8111-111111111111','33333333-3333-4333-8333-333333333333',1,repeat('f',64),'{}',now()),
  ('88888888-2222-4222-8222-222222222222','44444444-4444-4444-8444-444444444444',1,repeat('c',64),'{}',now());

UPDATE nodes SET lifecycle_state='active', trust_state='verified', agent_eligible=true,
  current_identity_generation=1, current_identity_status='active', current_capability_version=1,
  kubernetes_cluster_id='cluster-a', kubernetes_node_name='worker-a',
  kubernetes_node_uid='uid-a', kubernetes_resource_version='1'
  WHERE id='33333333-3333-4333-8333-333333333333';

DO $$
BEGIN
  BEGIN
    INSERT INTO node_identities(id,node_id,public_key_fingerprint,public_key,signing_key_id,generation,status,issued_at,expires_at) VALUES
      ('88888888-8888-4888-8888-888888888888','44444444-4444-4444-8444-444444444444',repeat('f',64),'not-a-key','invalid/v1',2,'active',now(),now()+interval '1 hour');
    RAISE EXCEPTION 'invalid identity public key accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    UPDATE node_enrollments SET status='consumed', consumed_by_node_id='44444444-4444-4444-8444-444444444444',
      machine_binding=repeat('a',64), node_public_key=repeat('A',43), node_public_key_fingerprint=repeat('e',64),
      exchanged_at=now(), consumed_at=now() WHERE id='55555555-5555-4555-8555-555555555555';
    RAISE EXCEPTION 'cross-workspace enrollment consumption accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
  BEGIN
    UPDATE nodes SET lifecycle_state='active',trust_state='verified',agent_eligible=true,
      current_identity_generation=1,current_identity_status='active',current_capability_version=1,
      kubernetes_cluster_id='cluster-b',kubernetes_node_name='worker-b',kubernetes_node_uid='uid-b',kubernetes_resource_version='1'
      WHERE id='44444444-4444-4444-8444-444444444444';
    SET CONSTRAINTS ALL IMMEDIATE;
    RAISE EXCEPTION 'revoked current identity accepted for eligible Node';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
  BEGIN
    UPDATE node_identities SET status='revoked',revoked_at=now()
      WHERE id='77777777-7777-4777-8777-777777777777';
    SET CONSTRAINTS ALL IMMEDIATE;
    RAISE EXCEPTION 'active identity revocation without Node transition accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;

INSERT INTO node_install_plans(id,workspace_id,node_id,enrollment_id,approved_by,idempotency_key,plan_digest,signing_key_id,signature,canonical_plan,issued_at,expires_at,status) VALUES
  ('99999999-9999-4999-8999-999999999999','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','55555555-5555-4555-8555-555555555555','11111111-1111-4111-8111-111111111111','plan-key-a',repeat('1',64),'node-plan/v1',repeat('A',86),'{}',now(),now()+interval '5 minutes','issued'),
  ('aaaaaaaa-1111-4111-8111-111111111111','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','44444444-4444-4444-8444-444444444444','66666666-6666-4666-8666-666666666666','22222222-2222-4222-8222-222222222222','plan-key-b',repeat('2',64),'node-plan/v1',repeat('A',86),'{}',now(),now()+interval '5 minutes','issued');

INSERT INTO node_install_receipts(id,workspace_id,node_id,plan_id,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
  ('bbbbbbbb-1111-4111-8111-111111111111','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','99999999-9999-4999-8999-999999999999',repeat('3',64),'node_identity',1,repeat('e',64),'node-identity/v1',repeat('A',86),'{}');

DO $$
BEGIN
  BEGIN
    INSERT INTO node_install_receipts(id,workspace_id,node_id,plan_id,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
      ('cccccccc-1111-4111-8111-111111111111','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','aaaaaaaa-1111-4111-8111-111111111111',repeat('4',64),'node_identity',1,repeat('e',64),'node-identity/v1',repeat('A',86),'{}');
    RAISE EXCEPTION 'cross-bound install receipt accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
  BEGIN
    INSERT INTO node_install_receipts(id,workspace_id,node_id,plan_id,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
      ('cccccccc-2222-4222-8222-222222222222','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','99999999-9999-4999-8999-999999999999',repeat('d',64),'node_identity',99,repeat('e',64),'node-identity/v99',repeat('A',86),'{}');
    RAISE EXCEPTION 'unbound install receipt signer generation accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
  BEGIN
    INSERT INTO node_install_receipts(id,workspace_id,node_id,plan_id,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
      ('cccccccc-3333-4333-8333-333333333333','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','99999999-9999-4999-8999-999999999999',repeat('b',64),'node_identity',1,repeat('d',64),'node-identity/v1',repeat('A',86),'{}');
    RAISE EXCEPTION 'install receipt signer fingerprint mismatch accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
  BEGIN
    INSERT INTO node_install_receipts(id,workspace_id,node_id,plan_id,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
      ('cccccccc-4444-4444-8444-444444444444','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','99999999-9999-4999-8999-999999999999',repeat('a',64),'node_identity',1,repeat('e',64),'wrong-key/v1',repeat('A',86),'{}');
    RAISE EXCEPTION 'install receipt signer key mismatch accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;

INSERT INTO node_operations(id,workspace_id,node_id,type,status,expected_node_version,requested_by,idempotency_key,request_digest) VALUES
  ('dddddddd-1111-4111-8111-111111111111','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','pause','pending',1,'11111111-1111-4111-8111-111111111111','operation-key-a',repeat('5',64)),
  ('dddddddd-2222-4222-8222-222222222222','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','44444444-4444-4444-8444-444444444444','pause','pending',1,'22222222-2222-4222-8222-222222222222','operation-key-b',repeat('6',64));
INSERT INTO node_operation_receipts(id,operation_id,workspace_id,node_id,operation_type,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
  ('eeeeeeee-1111-4111-8111-111111111111','dddddddd-1111-4111-8111-111111111111','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','pause',repeat('6',64),'node_identity',1,repeat('e',64),'node-identity/v1',repeat('A',86),'{}');
UPDATE node_operations SET status='succeeded',completed_at=now(),receipt_id='eeeeeeee-1111-4111-8111-111111111111'
  WHERE id='dddddddd-1111-4111-8111-111111111111';

DO $$
BEGIN
  BEGIN
    INSERT INTO node_operation_receipts(id,operation_id,workspace_id,node_id,operation_type,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
      ('ffffffff-1111-4111-8111-111111111111','dddddddd-2222-4222-8222-222222222222','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','33333333-3333-4333-8333-333333333333','pause',repeat('7',64),'control_plane',NULL,repeat('c',64),'control-plane-receipt/v1',repeat('A',86),'{}');
    SET CONSTRAINTS ALL IMMEDIATE;
    RAISE EXCEPTION 'cross-bound operation receipt accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
  BEGIN
    UPDATE node_operations SET status='pending',completed_at=now(),receipt_id=NULL WHERE id='dddddddd-1111-4111-8111-111111111111';
    RAISE EXCEPTION 'nonterminal operation completion accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    UPDATE node_operations SET status='succeeded',completed_at=now() WHERE id='dddddddd-2222-4222-8222-222222222222';
    RAISE EXCEPTION 'terminal operation without signed receipt accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO node_operation_receipts(id,operation_id,workspace_id,node_id,operation_type,receipt_digest,signer_kind,identity_generation,signer_fingerprint,signing_key_id,signature,payload) VALUES
      ('ffffffff-3333-4333-8333-333333333333','dddddddd-2222-4222-8222-222222222222','bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','44444444-4444-4444-8444-444444444444','pause',repeat('d',64),'control_plane',1,repeat('c',64),'control-plane-receipt/v1',repeat('A',86),'{}');
    RAISE EXCEPTION 'control-plane receipt identity generation accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
END $$;

DO $$
BEGIN
  BEGIN
    INSERT INTO node_join_issuances(id,workspace_id,enrollment_id,plan_id,node_id,node_public_key_fingerprint,machine_fingerprint,credential_hash,credential_ciphertext,credential_key_id,idempotency_key,request_digest,issued_at,expires_at) VALUES
      ('ffffffff-2222-4222-8222-222222222222','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','66666666-6666-4666-8666-666666666666','99999999-9999-4999-8999-999999999999','33333333-3333-4333-8333-333333333333',repeat('e',64),repeat('a',64),repeat('a',64),decode(repeat('aa',29),'hex'),'node-join-credential/v1','join-cross',repeat('b',64),now(),now()+interval '5 minutes');
    RAISE EXCEPTION 'cross-bound join issuance accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;

RESET ROLE;
SET ROLE blazn_node_broker;
SELECT id FROM nodes WHERE id='33333333-3333-4333-8333-333333333333';
SELECT id FROM node_enrollments WHERE id='55555555-5555-4555-8555-555555555555';
SELECT id FROM node_install_plans WHERE id='99999999-9999-4999-8999-999999999999';
INSERT INTO node_join_issuances(id,workspace_id,enrollment_id,plan_id,node_id,node_public_key_fingerprint,machine_fingerprint,credential_hash,credential_ciphertext,credential_key_id,idempotency_key,request_digest,issued_at,expires_at) VALUES
  ('aaaaaaaa-2222-4222-8222-222222222222','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','55555555-5555-4555-8555-555555555555','99999999-9999-4999-8999-999999999999','33333333-3333-4333-8333-333333333333',repeat('e',64),repeat('a',64),repeat('8',64),decode(repeat('aa',29),'hex'),'node-join-credential/v1','join-key-a',repeat('9',64),now(),now()+interval '5 minutes');
RESET ROLE;

SET ROLE blazn_runtime;
DO $$
BEGIN
  BEGIN
    UPDATE node_join_issuances SET consumed_at=now()
      WHERE id='aaaaaaaa-2222-4222-8222-222222222222';
    RAISE EXCEPTION 'non-atomic runtime consumption accepted';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    UPDATE node_join_issuances SET consumed_at=now(),joined_node_uid='wrong-uid'
      WHERE id='aaaaaaaa-2222-4222-8222-222222222222';
    RAISE EXCEPTION 'consumed issuance UID mismatch accepted';
  EXCEPTION WHEN foreign_key_violation THEN NULL; END;
END $$;
UPDATE node_join_issuances SET consumed_at=now(),joined_node_uid='uid-a'
  WHERE id='aaaaaaaa-2222-4222-8222-222222222222';
RESET ROLE;

SELECT has_table_privilege('blazn_node_broker','nodes','SELECT') AS broker_nodes_read,
       has_table_privilege('blazn_node_broker','node_enrollments','SELECT') AS broker_enrollment_read,
       has_table_privilege('blazn_node_broker','node_install_plans','SELECT') AS broker_plan_read,
       has_table_privilege('blazn_node_broker','node_join_issuances','INSERT') AS broker_issue,
       has_column_privilege('blazn_node_broker','node_join_issuances','id','INSERT') AS broker_issue_id,
       has_column_privilege('blazn_node_broker','node_join_issuances','joined_node_uid','INSERT') AS broker_issue_joined_uid,
       has_table_privilege('blazn_node_broker','node_join_issuances','UPDATE') AS broker_issue_update,
       has_table_privilege('blazn_node_broker','nodes','UPDATE') AS broker_node_update,
       has_table_privilege('blazn_node_broker','users','SELECT') AS broker_user_read,
       has_table_privilege('blazn_runtime','node_join_issuances','INSERT') AS runtime_issue,
       has_table_privilege('blazn_runtime','node_join_issuances','SELECT') AS runtime_select_all,
       has_column_privilege('blazn_runtime','node_join_issuances','id','SELECT') AS runtime_select_id,
       has_column_privilege('blazn_runtime','node_join_issuances','credential_hash','SELECT') AS runtime_select_hash,
       has_column_privilege('blazn_runtime','node_join_issuances','credential_ciphertext','SELECT') AS runtime_select_ciphertext,
       has_column_privilege('blazn_runtime','node_join_issuances','credential_key_id','SELECT') AS runtime_select_key_id,
       has_column_privilege('blazn_runtime','node_join_issuances','consumed_at','UPDATE') AS runtime_consume_time,
       has_column_privilege('blazn_runtime','node_join_issuances','joined_node_uid','UPDATE') AS runtime_consume_uid,
       has_column_privilege('blazn_runtime','node_join_issuances','credential_ciphertext','UPDATE') AS runtime_ciphertext_update,
       octet_length(credential_ciphertext) >= 29 AS encrypted_credential_present,
       credential_key_id = 'node-join-credential/v1' AS credential_key_pinned,
       char_length(idempotency_key) >= 8 AS deterministic_retry_key_present,
       char_length(request_digest) = 64 AS request_digest_present
  FROM node_join_issuances WHERE id='aaaaaaaa-2222-4222-8222-222222222222';

DO $$
BEGIN
  IF NOT has_table_privilege('blazn_node_broker','nodes','SELECT')
    OR NOT has_table_privilege('blazn_node_broker','node_enrollments','SELECT')
    OR NOT has_table_privilege('blazn_node_broker','node_install_plans','SELECT')
    OR has_table_privilege('blazn_node_broker','node_join_issuances','INSERT')
    OR NOT has_column_privilege('blazn_node_broker','node_join_issuances','id','INSERT')
    OR has_column_privilege('blazn_node_broker','node_join_issuances','joined_node_uid','INSERT')
    OR has_table_privilege('blazn_node_broker','node_join_issuances','UPDATE')
    OR has_table_privilege('blazn_node_broker','nodes','UPDATE')
    OR has_table_privilege('blazn_node_broker','users','SELECT')
    OR has_table_privilege('blazn_runtime','node_join_issuances','INSERT')
    OR has_table_privilege('blazn_runtime','node_join_issuances','SELECT')
    OR NOT has_column_privilege('blazn_runtime','node_join_issuances','id','SELECT')
    OR has_column_privilege('blazn_runtime','node_join_issuances','credential_hash','SELECT')
    OR has_column_privilege('blazn_runtime','node_join_issuances','credential_ciphertext','SELECT')
    OR has_column_privilege('blazn_runtime','node_join_issuances','credential_key_id','SELECT')
    OR NOT has_column_privilege('blazn_runtime','node_join_issuances','consumed_at','UPDATE')
    OR NOT has_column_privilege('blazn_runtime','node_join_issuances','joined_node_uid','UPDATE')
    OR has_column_privilege('blazn_runtime','node_join_issuances','credential_ciphertext','UPDATE') THEN
    RAISE EXCEPTION 'Node broker/runtime privilege matrix is invalid';
  END IF;
END $$;

DO $$
DECLARE owner_name text; config text; acl text;
BEGIN
  SELECT r.rolname,array_to_string(p.proconfig,','),array_to_string(p.proacl,',')
    INTO owner_name,config,acl FROM pg_proc p JOIN pg_roles r ON r.oid=p.proowner
    WHERE p.oid='node_broker_lock_join_binding(uuid,uuid,uuid)'::regprocedure;
  IF owner_name <> 'blazn_migration'
    OR config NOT LIKE '%search_path=pg_catalog, public%'
    OR position('=X/' in coalesce(acl,'')) > 0
    OR NOT has_function_privilege('blazn_node_broker','node_broker_lock_join_binding(uuid,uuid,uuid)','EXECUTE')
    OR has_function_privilege('blazn_runtime','node_broker_lock_join_binding(uuid,uuid,uuid)','EXECUTE')
    OR has_function_privilege('blazn_bootstrap','node_broker_lock_join_binding(uuid,uuid,uuid)','EXECUTE') THEN
    RAISE EXCEPTION 'Node broker row-lock function ownership, search_path, or ACL is invalid';
  END IF;
END $$;
SQL

expect_denied() {
  statement=$1
  label=$2
  if printf '%s\n' "$statement" | psql_admin >/dev/null 2>&1; then
    printf '%s unexpectedly allowed\n' "$label" >&2
    exit 1
  fi
}
expect_denied "SET ROLE blazn_node_broker; UPDATE nodes SET name='changed';" broker_node_update
expect_denied "SET ROLE blazn_node_broker; SELECT * FROM users;" broker_user_read
expect_denied "SET ROLE blazn_node_broker; SELECT * FROM node_capability_versions;" broker_capability_read
expect_denied "SET ROLE blazn_node_broker; SELECT * FROM node_operations;" broker_operation_read
expect_denied "SET ROLE blazn_node_broker; UPDATE node_join_issuances SET credential_hash=repeat('f',64);" broker_issuance_update
expect_denied "SET ROLE blazn_node_broker; INSERT INTO node_join_issuances(joined_node_uid) VALUES('uid');" broker_issuance_unreviewed_insert
expect_denied "SET ROLE blazn_runtime; SELECT node_broker_lock_join_binding('55555555-5555-4555-8555-555555555555','99999999-9999-4999-8999-999999999999','33333333-3333-4333-8333-333333333333');" runtime_broker_lock
expect_denied "SET ROLE blazn_bootstrap; SELECT node_broker_lock_join_binding('55555555-5555-4555-8555-555555555555','99999999-9999-4999-8999-999999999999','33333333-3333-4333-8333-333333333333');" bootstrap_broker_lock
expect_denied "SET ROLE blazn_runtime; INSERT INTO node_join_issuances DEFAULT VALUES;" runtime_issue
expect_denied "SET ROLE blazn_runtime; SELECT credential_hash FROM node_join_issuances;" runtime_select_hash
expect_denied "SET ROLE blazn_runtime; SELECT credential_ciphertext FROM node_join_issuances;" runtime_select_ciphertext
expect_denied "SET ROLE blazn_runtime; SELECT credential_key_id FROM node_join_issuances;" runtime_select_key_id
expect_denied "SET ROLE blazn_runtime; SELECT * FROM node_join_issuances;" runtime_select_all
expect_denied "SET ROLE blazn_runtime; UPDATE node_join_issuances SET credential_ciphertext=decode(repeat('bb',29),'hex');" runtime_ciphertext_update
expect_denied "SET ROLE blazn_bootstrap; SELECT * FROM nodes;" bootstrap_node_read

runtime_password=node-runtime-ci
broker_password=node-broker-ci
psql_admin <<SQL
ALTER ROLE blazn_runtime LOGIN PASSWORD '$runtime_password';
ALTER ROLE blazn_node_broker LOGIN PASSWORD '$broker_password';
SQL
created_node_runner=true
docker create --name "$node_runner" --network "$network" --read-only \
  --tmpfs /work:rw,nosuid,nodev,size=256m,mode=0700,uid=1000,gid=1000 \
  --tmpfs /tmp:rw,nosuid,nodev,size=64m,mode=1777 \
  -v "$repo_root/services/control-api:/source:ro" -v "$repo_root:/repo:ro" -w /work \
  -e HOME=/work/home -e npm_config_cache=/work/.npm \
  -e NODE_TEST_ADMIN_DATABASE_URL="postgresql://postgres:$admin_password@$postgres:5432/blazn" \
  -e NODE_TEST_RUNTIME_DATABASE_URL="postgresql://blazn_runtime:$runtime_password@$postgres:5432/blazn" \
  -e NODE_TEST_BROKER_DATABASE_URL="postgresql://blazn_node_broker:$broker_password@$postgres:5432/blazn" \
  -e NODE_TEST_REPO_ROOT=/repo \
  "$node_image" sh -eu -c 'cp /source/package.json /source/package-lock.json /source/tsconfig.json /work/; cp -a /source/src /work/src; npm ci >/dev/null; node node_modules/typescript/bin/tsc -p tsconfig.json; node --test dist/node-store.integration.test.js dist/node-broker-store.integration.test.js' >/dev/null
docker start -a "$node_runner"

printf 'Node PostgreSQL 17.6 qualification passed\n'
