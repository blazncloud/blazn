#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$#" -eq 2 ] || die "usage: restore-test.sh BACKUP_DIRECTORY EMPTY_RESTORE_ROOT"
backup=$1
target=$2
require_absolute_path BACKUP_DIRECTORY "$backup"
require_absolute_path EMPTY_RESTORE_ROOT "$target"
assert_not_symlink_chain "$backup"
assert_not_symlink_chain "$target"
[ -d "$backup" ] || die "backup directory does not exist"
[ ! -e "$target" ] || die "restore root must not exist"

live_host=${BLAZN_LIVE_HOST:-ben1}
[ "$(hostname -s)" != "$live_host" ] || die "restore tests are forbidden on the live control-plane host"
case "$target" in
  /var/tmp/blazn-restore/*) ;;
  *) die "restore root must be a unique child of /var/tmp/blazn-restore" ;;
esac

require_command docker
require_command sha256sum
(
  cd "$backup"
  sha256sum -c SHA256SUMS
)

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
until docker exec "$container" pg_isready -U blazn_restore -d blazn_restore >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 60 ] || die "isolated restore PostgreSQL did not become healthy"
  sleep 1
done

docker exec -i "$container" pg_restore --exit-on-error --no-owner --no-privileges \
  -U blazn_restore -d blazn_restore <"$backup/postgres.dump"
docker exec "$container" psql -v ON_ERROR_STOP=1 -U blazn_restore -d blazn_restore \
  -Atqc "select current_database(), current_user" >"$target/restore-query.txt"
printf 'isolated restore passed; retained evidence: %s\n' "$target"
