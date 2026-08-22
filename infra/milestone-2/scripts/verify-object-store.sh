#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$#" -eq 1 ] || die "usage: verify-object-store.sh RUN_ID"
run_id=$1
case "$run_id" in
  ''|*[!a-zA-Z0-9._-]*) die "run ID contains unsupported characters" ;;
esac
require_command docker
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"

work=${TMPDIR:-/tmp}/blazn-object-test-$run_id-$$
[ ! -e "$work" ] || die "object-test path already exists"
umask 077
mkdir -p -- "$work/input" "$work/output"
printf 'blazn-object-fixture-v1\nrun=%s\n' "$run_id" >"$work/input/payload.txt"
expected=$(sha256_file "$work/input/payload.txt")

cleanup() {
  rm -f -- "$work/input/payload.txt" "$work/output/payload.txt"
  rmdir -- "$work/input" "$work/output" "$work" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

docker compose -f "$ROOT_DIR/compose.yaml" --profile tools run --rm \
  -v "$work:/fixture" object-client \
  'access=$(cat /run/secrets/s3_runtime_access_key); secret=$(cat /run/secrets/s3_runtime_secret_key); mc alias set blazn http://object:9000 "$access" "$secret" >/dev/null; prefix="$1/object-verification/$2"; mc cp /fixture/input/payload.txt "blazn/$prefix/payload.txt" >/dev/null; mc cp "blazn/$prefix/payload.txt" /fixture/output/payload.txt >/dev/null; mc rm --recursive --force "blazn/$prefix" >/dev/null; mc ls --recursive "blazn/$prefix" >/tmp/residue 2>/dev/null || true; [ ! -s /tmp/residue ]' \
  -- "${S3_BUCKET:-blazn-poc}" "$run_id"

actual=$(sha256_file "$work/output/payload.txt")
[ "$actual" = "$expected" ] || die "downloaded object digest mismatch"
printf '{"status":"ok","runId":"%s","sha256":"%s","residue":false}\n' "$run_id" "$actual"
