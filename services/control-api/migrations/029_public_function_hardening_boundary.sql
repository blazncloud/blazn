-- Trusted pgcrypto functions are owned by the PostgreSQL bootstrap
-- administrator, while application functions are owned by blazn_migration.
-- Revoke every migration-owned PUBLIC function capability now, accept only the
-- exact pgcrypto defaults that the post-migration administrator job can
-- normalize, and reject every other external authority fail closed.
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public
  REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

DO $hardening$
DECLARE function_row record;
DECLARE reviewed_pgcrypto boolean;
BEGIN
  FOR function_row IN
    SELECT function_catalog.oid,function_catalog.oid::regprocedure AS signature,
      function_catalog.proname,replace(oidvectortypes(function_catalog.proargtypes),' ','') AS argument_types,
      function_catalog.proowner,owner_role.rolname AS owner_name,privilege.is_grantable
    FROM pg_proc function_catalog
    JOIN pg_namespace schema_catalog ON schema_catalog.oid=function_catalog.pronamespace
    JOIN pg_roles owner_role ON owner_role.oid=function_catalog.proowner
    CROSS JOIN LATERAL aclexplode(coalesce(function_catalog.proacl,acldefault('f',function_catalog.proowner))) privilege
    WHERE schema_catalog.nspname='public'
      AND privilege.grantee=0 AND privilege.privilege_type='EXECUTE'
  LOOP
    reviewed_pgcrypto := NOT function_row.is_grantable AND EXISTS (
      SELECT FROM pg_depend extension_member
      JOIN pg_extension extension_catalog ON extension_catalog.oid=extension_member.refobjid
      WHERE extension_member.classid='pg_proc'::regclass
        AND extension_member.objid=function_row.oid
        AND extension_member.deptype='e'
        AND extension_catalog.extname='pgcrypto'
    ) AND (function_row.proname,function_row.argument_types) IN (
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
    );
    IF function_row.owner_name=current_user THEN
      EXECUTE format('REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC',function_row.signature);
    ELSIF NOT reviewed_pgcrypto THEN
      RAISE EXCEPTION 'unreviewed external function % retains PUBLIC EXECUTE',function_row.signature;
    END IF;
  END LOOP;

  IF NOT has_function_privilege(current_user,'public.digest(bytea,text)','EXECUTE')
    OR NOT has_function_privilege(current_user,'public.digest(text,text)','EXECUTE') THEN
    RAISE EXCEPTION 'reviewed pgcrypto digest authority is unavailable';
  END IF;
END
$hardening$;
