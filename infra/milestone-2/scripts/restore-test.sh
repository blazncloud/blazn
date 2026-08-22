#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$#" -eq 4 ] || die "usage: restore-test.sh BACKUP_DIRECTORY EMPTY_RESTORE_ROOT OWNERSHIP_RECEIPT NODE_KEY_INVENTORY"
backup=$1
target=$2
node_receipt=$3
node_inventory=$4
require_absolute_path BACKUP_DIRECTORY "$backup"
require_absolute_path EMPTY_RESTORE_ROOT "$target"
require_command realpath
assert_not_symlink_chain "$backup"
assert_not_symlink_chain "$target"
[ -d "$backup" ] || die "backup directory does not exist"
[ ! -e "$target" ] || die "restore root must not exist"

live_host=${BLAZN_LIVE_HOST:-ben1}
[ "$(hostname -s)" != "$live_host" ] || die "restore tests are forbidden on the live control-plane host"
restore_parent=/var/tmp/blazn-restore
if [ ! -d "$restore_parent" ] || [ -L "$restore_parent" ]; then
  die "restore parent must be a real directory: $restore_parent"
fi
canonical_parent=$(realpath -e "$restore_parent")
target_parent=$(realpath -e "$(dirname -- "$target")")
[ "$target_parent" = "$canonical_parent" ] || die "restore root must be a direct child of $canonical_parent"
target_name=$(basename -- "$target")
case "$target_name" in
  ''|.|..|*[!a-zA-Z0-9._-]*) die "restore root name contains unsupported characters" ;;
esac
[ "$target" = "$canonical_parent/$target_name" ] || die "restore root must use its canonical direct-child path"

require_command docker
require_command sha256sum
require_command jq
(
  cd "$backup"
  sha256sum -c SHA256SUMS
)
jq -e '
  .schemaVersion == "blazn.dev/control-plane-backup/v2" and
  (.fencingToken | type == "number" and . >= 1) and
  (.configDigest | test("^sha256:[a-f0-9]{64}$")) and
  (.controlApi.sourceDigest | test("^sha256:[a-f0-9]{64}$")) and
  (.controlApi.image | test("^blazn-control-api:source-[a-f0-9]{64}$")) and
  (.controlApi.imageId | test("^sha256:[a-f0-9]{64}$")) and
  (.secretDigests["workspace-invitation-hmac-v1"] | test("^sha256:[a-f0-9]{64}$"))' \
  "$backup/metadata.json" >/dev/null || die "backup rollback inventory is invalid"
"$SCRIPT_DIR/../../node/scripts/verify-backup-metadata.sh" "$backup/metadata.json" "$node_receipt" "$node_inventory" >/dev/null

image=${POSTGRES_IMAGE:-}
case "$image" in
  *@sha256:????????????????????????????????????????????????????????????????) ;;
  *) die "POSTGRES_IMAGE must use an immutable sha256 digest" ;;
esac

case "${BLAZN_RESTORE_CORRELATION_ID:-restore}" in
  *[!a-zA-Z0-9._-]*) die "restore correlation ID contains unsupported characters" ;;
esac
container=blazn-restore-${BLAZN_RESTORE_CORRELATION_ID:-restore}-$$
umask 077
mkdir -p -- "$target/postgres"

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

docker run -d --name "$container" \
  -e POSTGRES_USER=blazn_restore \
  -e POSTGRES_PASSWORD=restore-only-password \
  -e POSTGRES_DB=blazn_restore \
  -v "$target/postgres:/var/lib/postgresql/data" \
  "$image" >/dev/null

attempt=0
stable=0
while [ "$stable" -lt 3 ]; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 60 ] || die "isolated restore PostgreSQL did not become healthy"
  if docker exec "$container" psql -v ON_ERROR_STOP=1 -U blazn_restore -d blazn_restore -Atqc 'select 1' >/dev/null 2>&1; then
    stable=$((stable + 1))
  else
    stable=0
  fi
  sleep 1
done

docker exec -i "$container" pg_restore --exit-on-error --no-owner --no-privileges \
  -U blazn_restore -d blazn_restore <"$backup/postgres.dump"
docker exec "$container" psql -v ON_ERROR_STOP=1 -U blazn_restore -d blazn_restore \
  -Atqc "select current_database(), current_user" >"$target/restore-query.txt"
printf 'isolated restore passed; retained evidence: %s\n' "$target"
