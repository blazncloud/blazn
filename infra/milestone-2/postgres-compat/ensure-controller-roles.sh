#!/bin/sh
set -eu

password=$(cat /run/secrets/postgres_password)
case $password in
  *[!a-f0-9]*|'')
    printf 'PostgreSQL administrator password has an invalid format\n' >&2
    exit 1
    ;;
esac
[ "${#password}" -eq 64 ] || {
  printf 'PostgreSQL administrator password has an unexpected length\n' >&2
  exit 1
}

export PGPASSWORD="$password"
psql -X -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=database_name="$POSTGRES_DB" <<'SQL'
BEGIN;
DO $roles$
DECLARE role_name text;
DECLARE role_oid oid;
BEGIN
  FOREACH role_name IN ARRAY ARRAY['blazn_sandbox_controller','blazn_development_controller','blazn_agent_run_controller'] LOOP
    IF EXISTS (
      SELECT FROM pg_roles
      WHERE pg_roles.rolname=role_name
        AND ((rolcanlogin AND role_name NOT IN ('blazn_sandbox_controller','blazn_agent_run_controller')) OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
    ) THEN
      RAISE EXCEPTION 'controller role % has unsafe attributes', role_name;
    END IF;
    IF EXISTS (
      SELECT FROM pg_auth_members
      JOIN pg_roles member_role ON member_role.oid=pg_auth_members.member
      WHERE member_role.rolname=role_name
    ) THEN
      RAISE EXCEPTION 'controller role % inherits another role', role_name;
    END IF;
    IF EXISTS (
      SELECT FROM pg_database JOIN pg_roles ON pg_roles.oid=pg_database.datdba
      WHERE pg_roles.rolname=role_name
      UNION ALL
      SELECT FROM pg_namespace JOIN pg_roles ON pg_roles.oid=pg_namespace.nspowner
      WHERE pg_roles.rolname=role_name
      UNION ALL
      SELECT FROM pg_class JOIN pg_roles ON pg_roles.oid=pg_class.relowner
      WHERE pg_roles.rolname=role_name
      UNION ALL
      SELECT FROM pg_proc JOIN pg_roles ON pg_roles.oid=pg_proc.proowner
      WHERE pg_roles.rolname=role_name
      UNION ALL
      SELECT FROM pg_type JOIN pg_roles ON pg_roles.oid=pg_type.typowner
      WHERE pg_roles.rolname=role_name
    ) THEN
      RAISE EXCEPTION 'controller role % unexpectedly owns database objects', role_name;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE pg_roles.rolname=role_name) THEN
      IF role_name = 'blazn_sandbox_controller' THEN
        EXECUTE 'CREATE ROLE blazn_sandbox_controller LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS';
      ELSIF role_name = 'blazn_agent_run_controller' THEN
        EXECUTE 'CREATE ROLE blazn_agent_run_controller LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS';
      ELSE
        EXECUTE format(
          'CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS',
          role_name
        );
      END IF;
    END IF;
    SELECT oid INTO STRICT role_oid FROM pg_roles WHERE pg_roles.rolname=role_name;
    IF EXISTS (
      SELECT FROM pg_database database_row
      CROSS JOIN LATERAL aclexplode(COALESCE(database_row.datacl,acldefault('d',database_row.datdba))) privilege
      WHERE database_row.datallowconn AND privilege.grantee IN (role_oid,0)
        AND NOT (privilege.grantee=role_oid AND database_row.datname=current_database() AND privilege.privilege_type='CONNECT' AND NOT privilege.is_grantable)
    ) THEN
      RAISE EXCEPTION 'controller role % has an unexpected database privilege', role_name;
    END IF;
    IF EXISTS (
      SELECT FROM pg_namespace schema_row
      CROSS JOIN LATERAL aclexplode(COALESCE(schema_row.nspacl,acldefault('n',schema_row.nspowner))) privilege
      WHERE privilege.grantee IN (role_oid,0)
        AND NOT (
          privilege.privilege_type='USAGE' AND NOT privilege.is_grantable AND (
            (privilege.grantee=role_oid AND schema_row.nspname='public')
            OR (privilege.grantee=0 AND schema_row.nspname IN ('public','pg_catalog','information_schema'))
          )
        )
    ) THEN
      RAISE EXCEPTION 'controller role % has an unexpected schema privilege', role_name;
    END IF;
    IF EXISTS (
      SELECT FROM pg_class relation_row
      JOIN pg_namespace schema_row ON schema_row.oid=relation_row.relnamespace
      CROSS JOIN LATERAL aclexplode(COALESCE(relation_row.relacl,acldefault(CASE relation_row.relkind WHEN 'S' THEN 's'::"char" ELSE 'r'::"char" END,relation_row.relowner))) privilege
      WHERE privilege.grantee IN (role_oid,0)
        AND schema_row.nspname NOT IN ('pg_catalog','information_schema')
    ) THEN
      RAISE EXCEPTION 'controller role % has an unexpected relation privilege', role_name;
    END IF;
    IF EXISTS (
      SELECT FROM pg_proc function_row
      JOIN pg_namespace schema_row ON schema_row.oid=function_row.pronamespace
      JOIN pg_roles owner_role ON owner_role.oid=function_row.proowner
      CROSS JOIN LATERAL aclexplode(COALESCE(function_row.proacl,acldefault('f',function_row.proowner))) privilege
      WHERE (
        privilege.grantee=role_oid AND (
          schema_row.nspname<>'public' OR owner_role.rolname<>'blazn_migration' OR NOT function_row.prosecdef
          OR privilege.privilege_type<>'EXECUTE' OR privilege.is_grantable
          OR CASE role_name
            WHEN 'blazn_sandbox_controller' THEN (function_row.proname,replace(oidvectortypes(function_row.proargtypes),' ','')) NOT IN (
              ('sandbox_controller_claim','text,integer'),('sandbox_controller_claim_v2','text,integer'),('sandbox_controller_claim_v3','text,integer'),('sandbox_controller_claim_v4','text,integer'),('sandbox_controller_claim_v5','text,integer'),
              ('sandbox_controller_renew','uuid,text,uuid,integer'),('sandbox_controller_bind_backend','uuid,text,uuid,text,text,text'),
              ('sandbox_controller_bind_backend_v2','uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text'),
              ('sandbox_controller_bind_backend_v3','uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text'),
              ('sandbox_controller_bind_backend_v4','uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text'),
              ('sandbox_controller_retry','uuid,text,uuid,integer,text,text,uuid'),
              ('sandbox_controller_complete','uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid'),
              ('sandbox_controller_complete_v2','uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid'),
              ('sandbox_controller_complete_v3','uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid'),
              ('sandbox_controller_complete_v4','uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid'),
              ('sandbox_controller_complete_v5','uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid'),
              ('sandbox_controller_enqueue_expired','integer'),
              ('sandbox_controller_record_source_materialization_v1','uuid,text,uuid,text,text,text,text,text,jsonb,jsonb'),
              ('sandbox_controller_record_artifact_v1','uuid,text,uuid,text,text,text,text,text,text,text,text,bigint,text'),
              ('sandbox_controller_complete_artifact_export_v1','uuid,text,uuid,text,text[]'),
              ('sandbox_controller_consume_access_grant_v1','uuid,character,text'),
              ('sandbox_controller_record_agent_node_observation','uuid,text,text,text,text')
            )
            WHEN 'blazn_development_controller' THEN (function_row.proname,replace(oidvectortypes(function_row.proargtypes),' ','')) NOT IN (
              ('development_controller_claim','text,integer'),('development_controller_renew','uuid,text,uuid,integer'),('development_controller_resolve','uuid,text,uuid'),
              ('development_controller_finalize_v1','uuid,text,uuid,bigint,uuid,uuid,jsonb'),
              ('development_controller_store_artifact_v1','uuid,text,uuid,uuid,text,text,text,bytea'),
              ('development_controller_release_v1','uuid,text,uuid,integer,text'),
              ('development_controller_commit_execution_v1','uuid,text,uuid,bigint,uuid,uuid,jsonb,jsonb'),
              ('development_collector_bind_candidate_images_v1','uuid,uuid,bigint,text,text,text'),
              ('development_collector_prepare_sandbox_v1','uuid,bigint,text,text,text'),
              ('development_collector_prepare_bound_sandbox_v1','uuid,bigint,text,text,text'),
              ('development_collector_resolve_sandbox_v1','uuid,bigint,text,text'),
              ('development_collector_resolve_bound_sandbox_v1','uuid,bigint,text,text'),
              ('development_collector_mark_sandbox_ready_v1','uuid,bigint,text,text,uuid'),
              ('development_collector_authorize_execution_v1','uuid,bigint,text,text,uuid')
            )
            WHEN 'blazn_agent_run_controller' THEN (function_row.proname,replace(oidvectortypes(function_row.proargtypes),' ','')) NOT IN (
              ('agent_run_controller_claim','text,integer'),
              ('agent_run_controller_renew','uuid,text,uuid,integer'),
              ('agent_run_controller_bind_sandbox','uuid,text,uuid,bigint,uuid,uuid'),
              ('agent_run_controller_retry','uuid,text,uuid,integer,text'),
              ('agent_run_controller_finalize','uuid,text,uuid,bigint,text,text,uuid[],bigint,text[]')
            )
          END
        )
      ) OR (
        privilege.grantee=0 AND schema_row.nspname NOT IN ('pg_catalog','information_schema')
        AND NOT (
          privilege.privilege_type='EXECUTE' AND NOT privilege.is_grantable
          AND schema_row.nspname='public' AND (
            (
              owner_role.rolname='blazn_migration'
              AND function_row.proname='sandbox_enforce_successful_create_admission'
              AND oidvectortypes(function_row.proargtypes)='' AND function_row.prorettype='trigger'::regtype
              AND function_row.prosecdef
            ) OR (
              EXISTS (
                SELECT FROM pg_depend extension_member
                JOIN pg_extension extension_row ON extension_row.oid=extension_member.refobjid
                WHERE extension_member.classid='pg_proc'::regclass
                  AND extension_member.objid=function_row.oid
                  AND extension_member.deptype='e'
                  AND extension_row.extname='pgcrypto'
              )
              AND (function_row.proname,replace(oidvectortypes(function_row.proargtypes),' ','')) IN (
                ('armor','bytea'),('armor','bytea,text[],text[]'),('crypt','text,text'),('dearmor','text'),
                ('decrypt','bytea,bytea,text'),('decrypt_iv','bytea,bytea,bytea,text'),
                ('digest','bytea,text'),('digest','text,text'),('encrypt','bytea,bytea,text'),
                ('encrypt_iv','bytea,bytea,bytea,text'),('gen_random_bytes','integer'),('gen_random_uuid',''),
                ('gen_salt','text'),('gen_salt','text,integer'),('hmac','bytea,bytea,text'),('hmac','text,text,text'),
                ('pgp_armor_headers','text'),('pgp_key_id','bytea'),('pgp_pub_decrypt','bytea,bytea'),
                ('pgp_pub_decrypt','bytea,bytea,text'),('pgp_pub_decrypt','bytea,bytea,text,text'),
                ('pgp_pub_decrypt_bytea','bytea,bytea'),('pgp_pub_decrypt_bytea','bytea,bytea,text'),
                ('pgp_pub_decrypt_bytea','bytea,bytea,text,text'),('pgp_pub_encrypt','text,bytea'),
                ('pgp_pub_encrypt','text,bytea,text'),('pgp_pub_encrypt_bytea','bytea,bytea'),
                ('pgp_pub_encrypt_bytea','bytea,bytea,text'),('pgp_sym_decrypt','bytea,text'),
                ('pgp_sym_decrypt','bytea,text,text'),('pgp_sym_decrypt_bytea','bytea,text'),
                ('pgp_sym_decrypt_bytea','bytea,text,text'),('pgp_sym_encrypt','text,text'),
                ('pgp_sym_encrypt','text,text,text'),('pgp_sym_encrypt_bytea','bytea,text'),
                ('pgp_sym_encrypt_bytea','bytea,text,text')
              )
            )
          )
        )
      )
    ) THEN
      RAISE EXCEPTION 'controller role % has an unexpected function privilege', role_name;
    END IF;
    IF EXISTS (
      SELECT FROM pg_type type_row
      CROSS JOIN LATERAL aclexplode(COALESCE(type_row.typacl,acldefault('T',type_row.typowner))) privilege
      WHERE privilege.grantee=role_oid OR (
        privilege.grantee=0 AND (privilege.privilege_type<>'USAGE' OR privilege.is_grantable)
      )
    ) OR EXISTS (
      SELECT FROM pg_default_acl default_row
      CROSS JOIN LATERAL aclexplode(default_row.defaclacl) privilege
      WHERE privilege.grantee IN (role_oid,0)
    ) OR EXISTS (
      SELECT FROM pg_largeobject_metadata large_object_row
      CROSS JOIN LATERAL aclexplode(COALESCE(large_object_row.lomacl,acldefault('L',large_object_row.lomowner))) privilege
      WHERE privilege.grantee IN (role_oid,0)
    ) OR EXISTS (
      SELECT FROM pg_foreign_data_wrapper wrapper_row
      CROSS JOIN LATERAL aclexplode(COALESCE(wrapper_row.fdwacl,acldefault('F',wrapper_row.fdwowner))) privilege
      WHERE privilege.grantee IN (role_oid,0)
    ) OR EXISTS (
      SELECT FROM pg_foreign_server server_row
      CROSS JOIN LATERAL aclexplode(COALESCE(server_row.srvacl,acldefault('S',server_row.srvowner))) privilege
      WHERE privilege.grantee IN (role_oid,0)
    ) OR EXISTS (
      SELECT FROM pg_language language_row
      CROSS JOIN LATERAL aclexplode(COALESCE(language_row.lanacl,acldefault('l',language_row.lanowner))) privilege
      WHERE privilege.grantee=role_oid OR (
        privilege.grantee=0 AND (language_row.oid>=16384 OR privilege.privilege_type<>'USAGE' OR privilege.is_grantable)
      )
    ) OR EXISTS (
      SELECT FROM pg_tablespace tablespace_row
      CROSS JOIN LATERAL aclexplode(COALESCE(tablespace_row.spcacl,acldefault('t',tablespace_row.spcowner))) privilege
      WHERE privilege.grantee IN (role_oid,0)
    ) OR EXISTS (
      SELECT FROM pg_parameter_acl parameter_row
      CROSS JOIN LATERAL aclexplode(parameter_row.paracl) privilege
      WHERE privilege.grantee IN (role_oid,0)
    ) THEN
      RAISE EXCEPTION 'controller role % has an unexpected auxiliary privilege', role_name;
    END IF;
  END LOOP;
END
$roles$;
-- pgcrypto installs reviewed functions with PUBLIC EXECUTE even when the
-- migration owner has hardened its default privileges.  The validation above
-- accepts only the exact extension-owned signatures.  Normalize those legacy
-- defaults before the broker performs its migration-boundary verification,
-- while preserving only the digest overloads used by migration-owned
-- SECURITY DEFINER functions.
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC;
DO $pgcrypto$
DECLARE function_signature text;
DECLARE function_oid oid;
BEGIN
  FOREACH function_signature IN ARRAY ARRAY['public.digest(bytea,text)','public.digest(text,text)'] LOOP
    function_oid := to_regprocedure(function_signature);
    IF function_oid IS NULL THEN
      IF EXISTS (SELECT FROM pg_extension WHERE extname='pgcrypto') THEN
        RAISE EXCEPTION 'reviewed pgcrypto function % is missing', function_signature;
      END IF;
      CONTINUE;
    END IF;
    IF NOT EXISTS (
      SELECT FROM pg_depend extension_member
      JOIN pg_extension extension_row ON extension_row.oid=extension_member.refobjid
      WHERE extension_member.classid='pg_proc'::regclass
        AND extension_member.objid=function_oid
        AND extension_member.deptype='e'
        AND extension_row.extname='pgcrypto'
    ) THEN
      RAISE EXCEPTION 'reviewed pgcrypto function % is not extension owned', function_signature;
    END IF;
    EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO blazn_migration', function_signature);
  END LOOP;
END
$pgcrypto$;
GRANT CONNECT ON DATABASE :"database_name" TO blazn_sandbox_controller,blazn_development_controller,blazn_agent_run_controller;
GRANT USAGE ON SCHEMA public TO blazn_sandbox_controller,blazn_development_controller,blazn_agent_run_controller;
COMMIT;
SQL
