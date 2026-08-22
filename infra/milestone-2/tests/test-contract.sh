#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
compose=$ROOT_DIR/compose.yaml
ngrok=$ROOT_DIR/ngrok.example.yml

# The first four strings intentionally assert unexpanded Compose interpolation.
# shellcheck disable=SC2016
for expected in \
  '127.0.0.1}:${POSTGRES_PORT:-55432}:5432' \
  '127.0.0.1}:${S3_PORT:-59000}:9000' \
  '127.0.0.1}:${S3_CONSOLE_PORT:-59001}:9001' \
  '127.0.0.1}:${API_PORT:-58080}:8080' \
  'context: ../../services/control-api' \
  'DATABASE_URL_FILE: /run/secrets/postgres_url' \
  'BLAZN_BOOTSTRAP_SECRET_FILE: /run/secrets/bootstrap_secret' \
  'PUBLIC_URL: ${PUBLIC_URL:-http://127.0.0.1:58080}' \
  'S3_ENDPOINT: http://object:9000' \
  'S3_ACCESS_KEY_FILE: /run/secrets/s3_access_key' \
  'S3_SECRET_KEY_FILE: /run/secrets/s3_secret_key'; do
  grep -F "$expected" "$compose" >/dev/null || {
    printf 'compose contract is missing: %s\n' "$expected" >&2
    exit 1
  }
done

grep -F 'addr: http://127.0.0.1:58080' "$ngrok" >/dev/null
grep -F 'domain: blazn.benpelo.com' "$ngrok" >/dev/null
if grep -F '${NGROK_AUTHTOKEN}' "$ngrok" >/dev/null; then
  printf 'ngrok config incorrectly relies on shell interpolation\n' >&2
  exit 1
fi
if grep -E '^[[:space:]]+addr:' "$ngrok" | grep -Ev 'http://127\.0\.0\.1:58080$' >/dev/null; then
  printf 'ngrok config exposes a data-plane endpoint\n' >&2
  exit 1
fi

for script in "$ROOT_DIR"/scripts/*.sh "$ROOT_DIR"/tests/*.sh; do
  sh -n "$script"
done

printf 'infrastructure contract tests passed\n'
