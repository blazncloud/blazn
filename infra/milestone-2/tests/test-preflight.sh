#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
tmp=${TMPDIR:-/tmp}/blazn-m2-preflight-$$
mkdir -p -- "$tmp/bin"
trap 'rm -rf -- "$tmp"' EXIT HUP INT TERM

cat >"$tmp/bin/stat" <<'EOF'
#!/bin/sh
case "$*" in
  */mnt*|*/backup*) [ "${FAKE_SAME_DEVICE:-0}" = 1 ] && printf '101\n' || printf '202\n' ;;
  *) printf '101\n' ;;
esac
EOF
cat >"$tmp/bin/df" <<'EOF'
#!/bin/sh
case "$1" in
  -PB1) printf 'Filesystem 1-blocks Used Available Capacity Mounted\nmock 100000000000 1 90000000000 1%% /\n' ;;
  -Pi) printf 'Filesystem Inodes IUsed IFree IUse%% Mounted\nmock 1000000 1 999999 1%% /\n' ;;
  *) exit 2 ;;
esac
EOF
cat >"$tmp/bin/ss" <<'EOF'
#!/bin/sh
if [ -n "${FAKE_LISTEN_PORT:-}" ]; then
  printf 'LISTEN 0 128 127.0.0.1:%s 0.0.0.0:*\n' "$FAKE_LISTEN_PORT"
fi
EOF
chmod +x "$tmp/bin/stat" "$tmp/bin/df" "$tmp/bin/ss"

base_env() {
  env \
    PATH="$tmp/bin:$PATH" \
    BLAZN_DATA_ROOT=/srv/frontro/blazn-poc/control-plane \
    BLAZN_BACKUP_ROOT=/mnt/blazn-poc-backup/control-plane \
    POSTGRES_IMAGE=postgres:17.6@sha256:0000000000000000000000000000000000000000000000000000000000000000 \
    MINIO_IMAGE=minio/minio:x@sha256:1111111111111111111111111111111111111111111111111111111111111111 \
    MINIO_MC_IMAGE=minio/mc:x@sha256:2222222222222222222222222222222222222222222222222222222222222222 \
    "$@"
}

result=$(base_env "$ROOT_DIR/scripts/preflight.sh" --plan)
printf '%s\n' "$result" | grep -F '"status":"ok"' >/dev/null
printf '%s\n' "$result" | grep -F '"separateFilesystem":true' >/dev/null

if FAKE_SAME_DEVICE=1 base_env "$ROOT_DIR/scripts/preflight.sh" --plan >"$tmp/out" 2>"$tmp/err"; then
  printf 'same-filesystem preflight unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'same filesystem' "$tmp/err" >/dev/null

if FAKE_LISTEN_PORT=59000 base_env "$ROOT_DIR/scripts/preflight.sh" --plan >"$tmp/out" 2>"$tmp/err"; then
  printf 'occupied-port preflight unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'TCP port is already in use: 59000' "$tmp/err" >/dev/null

if base_env BLAZN_BIND_ADDRESS=0.0.0.0 "$ROOT_DIR/scripts/preflight.sh" --plan >"$tmp/out" 2>"$tmp/err"; then
  printf 'public-bind preflight unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'must remain 127.0.0.1' "$tmp/err" >/dev/null

if base_env POSTGRES_IMAGE=postgres:17.6 "$ROOT_DIR/scripts/preflight.sh" --plan >"$tmp/out" 2>"$tmp/err"; then
  printf 'mutable-image preflight unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'immutable sha256 digest' "$tmp/err" >/dev/null

if base_env BLAZN_BACKUP_ROOT=/srv/frontro/blazn-poc/control-plane/backup "$ROOT_DIR/scripts/preflight.sh" --plan >"$tmp/out" 2>"$tmp/err"; then
  printf 'nested-backup preflight unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'backup root must not be inside' "$tmp/err" >/dev/null

printf 'preflight tests passed\n'
