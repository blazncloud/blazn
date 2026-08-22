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

if [ "$mode" = post-migration ]; then
  expected=$(query "select string_agg(table_name || '=' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'SELECT') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'INSERT') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'UPDATE') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'DELETE') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'TRUNCATE') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'REFERENCES') || ',' ||
    has_table_privilege(current_user, format('public.%I', table_name), 'TRIGGER'), ';' order by table_name)
    from information_schema.tables where table_schema='public' and table_name like 'node_%' or table_schema='public' and table_name='nodes'")
  required='node_audit_events=false,false,false,false,false,false,false;node_capability_versions=false,false,false,false,false,false,false;node_enrollments=true,false,false,false,false,false,false;node_heartbeat_state=false,false,false,false,false,false,false;node_identities=false,false,false,false,false,false,false;node_install_plans=true,false,false,false,false,false,false;node_install_receipts=false,false,false,false,false,false,false;node_join_issuances=true,true,true,false,false,false,false;node_operation_events=false,false,false,false,false,false,false;node_operation_receipts=false,false,false,false,false,false,false;node_operations=false,false,false,false,false,false,false;nodes=true,false,false,false,false,false,false'
  [ "$expected" = "$required" ] || {
    printf 'node broker table privilege matrix differs from migration 004: %s\n' "$expected" >&2
    exit 1
  }
fi

printf 'node broker %s database verification passed\n' "$mode"
