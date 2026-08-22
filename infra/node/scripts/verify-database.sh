#!/bin/sh
set -eu

mode=${1:-}
case "$mode" in
  pre-migration|post-migration) ;;
  *) printf 'usage: verify-database.sh pre-migration|post-migration\n' >&2; exit 2 ;;
esac

url_file=${NODE_BROKER_DATABASE_URL_FILE:-/run/secrets/node_broker_database_url}
if [ ! -f "$url_file" ] || [ -L "$url_file" ]; then
  printf 'node broker database URL file is unavailable or symlinked\n' >&2
  exit 1
fi
url=$(sed -n '1p' "$url_file")
case "$url" in
  postgresql://blazn_node_broker:????????????????????????????????????????????????????????????????@postgres:5432/blazn) ;;
  *) printf 'node broker database URL has an invalid fixed endpoint or credential form\n' >&2; exit 1 ;;
esac
password=${url#*://*:}
password=${password%%@*}
case "$password" in *[!a-f0-9]*) printf 'node broker password is not lowercase hexadecimal\n' >&2; exit 1 ;; esac

pgpass=${TMPDIR:-/tmp}/blazn-node-broker-pgpass-$$
cleanup() { rm -f -- "$pgpass"; }
trap cleanup EXIT HUP INT TERM
umask 077
printf 'postgres:5432:blazn:blazn_node_broker:%s\n' "$password" >"$pgpass"
export PGHOST=postgres PGPORT=5432 PGDATABASE=blazn PGUSER=blazn_node_broker PGPASSFILE="$pgpass"

query() {
  psql -X -v ON_ERROR_STOP=1 -Atqc "$1"
}
assert_empty() {
  sql=$1
  label=$2
  result=$(query "$sql")
  [ -z "$result" ] || { printf 'node broker has unexpected effective %s privileges: %s\n' "$label" "$result" >&2; exit 1; }
}

identity=$(query "select current_user, rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls from pg_roles where rolname = current_user")
[ "$identity" = 'blazn_node_broker|t|f|f|f|f|f' ] || {
  printf 'node broker role attributes are not least privilege\n' >&2
  exit 1
}
[ "$(query "select count(*) from pg_auth_members where member = (select oid from pg_roles where rolname = current_user)")" = 0 ] || {
  printf 'node broker role has unreviewed inherited memberships\n' >&2
  exit 1
}
[ "$(query "select has_database_privilege(current_user, current_database(), 'CONNECT')")" = t ] || {
  printf 'node broker lacks database CONNECT\n' >&2
  exit 1
}
[ "$(query "select has_database_privilege(current_user, current_database(), 'CREATE')")" = f ] || {
  printf 'node broker unexpectedly has database CREATE\n' >&2
  exit 1
}
[ "$(query "select has_database_privilege(current_user, current_database(), 'TEMP')")" = f ] || {
  printf 'node broker unexpectedly has database TEMP\n' >&2
  exit 1
}
[ "$(query "select has_schema_privilege(current_user, 'public', 'USAGE')")" = t ] || {
  printf 'node broker lacks schema USAGE\n' >&2
  exit 1
}
[ "$(query "select has_schema_privilege(current_user, 'public', 'CREATE')")" = f ] || {
  printf 'node broker unexpectedly has schema CREATE\n' >&2
  exit 1
}

assert_empty "select datname from pg_database where datallowconn and
  ((datname=current_database() and (not has_database_privilege(current_user,oid,'CONNECT') or has_database_privilege(current_user,oid,'CREATE') or has_database_privilege(current_user,oid,'TEMP')))
   or (datname<>current_database() and (has_database_privilege(current_user,oid,'CONNECT') or has_database_privilege(current_user,oid,'CREATE') or has_database_privilege(current_user,oid,'TEMP')))) order by datname" database
assert_empty "select nspname from pg_namespace where nspname !~ '^pg_' and nspname <> 'information_schema' and nspname <> 'public' and
  (has_schema_privilege(current_user, oid, 'USAGE') or has_schema_privilege(current_user, oid, 'CREATE')) order by nspname" schema
assert_empty "select n.nspname || '.' || c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace
  where n.nspname !~ '^pg_' and n.nspname <> 'information_schema' and c.relkind='S' and
  (has_sequence_privilege(current_user,c.oid,'USAGE') or has_sequence_privilege(current_user,c.oid,'SELECT') or has_sequence_privilege(current_user,c.oid,'UPDATE')) order by 1" sequence
if [ "$mode" = post-migration ]; then
  assert_empty "select n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' from pg_proc p join pg_namespace n on n.oid=p.pronamespace
    where n.nspname !~ '^pg_' and n.nspname <> 'information_schema' and p.oid<>'node_broker_lock_join_binding(uuid,uuid,uuid)'::regprocedure and has_function_privilege(current_user,p.oid,'EXECUTE') order by 1" function
else
  assert_empty "select n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' from pg_proc p join pg_namespace n on n.oid=p.pronamespace
    where n.nspname !~ '^pg_' and n.nspname <> 'information_schema' and has_function_privilege(current_user,p.oid,'EXECUTE') order by 1" function
fi
assert_empty "select pg_get_userbyid(d.defaclrole) || ':' || coalesce(n.nspname,'*') || ':' || d.defaclobjtype::text || ':' || coalesce(r.rolname,'PUBLIC') || ':' || x.privilege_type
  from pg_default_acl d left join pg_namespace n on n.oid=d.defaclnamespace cross join lateral aclexplode(d.defaclacl) x left join pg_roles r on r.oid=x.grantee
  where (x.grantee=0 or x.grantee=(select oid from pg_roles where rolname=current_user)) order by 1" default

if [ "$mode" = post-migration ]; then
  expected=$(query "select string_agg(table_name || '=' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'SELECT') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'INSERT') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'UPDATE') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'DELETE') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'TRUNCATE') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'REFERENCES') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'TRIGGER'), ';' order by table_name)
    from (values
      ('nodes'), ('node_enrollments'), ('node_identities'), ('node_capability_versions'),
      ('node_heartbeat_state'), ('node_install_plans'), ('node_install_receipts'),
      ('node_operation_receipts'), ('node_operations'), ('node_operation_events'),
      ('node_join_issuances'), ('node_join_issuance_intents'), ('node_audit_events')) as expected_tables(table_name)")
  required='node_audit_events=false,false,false,false,false,false,false;node_capability_versions=false,false,false,false,false,false,false;node_enrollments=true,false,false,false,false,false,false;node_heartbeat_state=false,false,false,false,false,false,false;node_identities=false,false,false,false,false,false,false;node_install_plans=true,false,false,false,false,false,false;node_install_receipts=false,false,false,false,false,false,false;node_join_issuance_intents=true,false,false,false,false,false,false;node_join_issuances=true,false,false,false,false,false,false;node_operation_events=false,false,false,false,false,false,false;node_operation_receipts=false,false,false,false,false,false,false;node_operations=false,false,false,false,false,false,false;nodes=true,false,false,false,false,false,false'
  [ "$expected" = "$required" ] || {
    printf 'node broker table privilege matrix differs from migration 004: %s\n' "$expected" >&2
    exit 1
  }
  assert_empty "with expected(name) as (values ('nodes'),('node_enrollments'),('node_install_plans'),('node_join_issuances'),('node_join_issuance_intents'))
    select n.nspname || '.' || c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace left join expected e on e.name=c.relname
    where n.nspname !~ '^pg_' and n.nspname <> 'information_schema' and c.relkind in ('r','p','v','m','f') and e.name is null and
      (has_table_privilege(current_user,c.oid,'SELECT') or has_table_privilege(current_user,c.oid,'INSERT') or has_table_privilege(current_user,c.oid,'UPDATE') or
       has_table_privilege(current_user,c.oid,'DELETE') or has_table_privilege(current_user,c.oid,'TRUNCATE') or has_table_privilege(current_user,c.oid,'REFERENCES') or has_table_privilege(current_user,c.oid,'TRIGGER')) order by 1" relation
  [ "$(query "select has_function_privilege(current_user,'node_broker_lock_join_binding(uuid,uuid,uuid)','EXECUTE')")" = t ] || {
    printf 'node broker cannot execute its reviewed row-lock function\n' >&2
    exit 1
  }
else
  assert_empty "select n.nspname || '.' || c.relname from pg_class c join pg_namespace n on n.oid=c.relnamespace
    where n.nspname !~ '^pg_' and n.nspname <> 'information_schema' and c.relkind in ('r','p','v','m','f') and
      (has_table_privilege(current_user,c.oid,'SELECT') or has_table_privilege(current_user,c.oid,'INSERT') or has_table_privilege(current_user,c.oid,'UPDATE') or
       has_table_privilege(current_user,c.oid,'DELETE') or has_table_privilege(current_user,c.oid,'TRUNCATE') or has_table_privilege(current_user,c.oid,'REFERENCES') or has_table_privilege(current_user,c.oid,'TRIGGER')) order by 1" relation
fi

printf 'node broker %s database verification passed\n' "$mode"
