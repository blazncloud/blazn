#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
WRAPPER=$ROOT_DIR/scripts/with-control-plane-env.sh
command -v sudo >/dev/null 2>&1 || { printf 'control-plane environment tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'control-plane environment tests skipped: passwordless sudo unavailable\n'; exit 0; }
top=${TMPDIR:-/tmp}/blazn-control-plane-env-test-$$
mkdir "$top"
cleanup() {
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

printf 'PUBLIC_URL=https://blazn.benpelo.com\nBLAZN_INITIAL_DISPLAY_NAME="Ben Pelo"\n' >"$top/good.env"
printf 'PUBLIC_URL=$(touch /tmp/should-not-run)\n' >"$top/bad.env"
chmod 0600 "$top"/*.env
sudo chown 0:0 "$top"/*.env
sudo env BLAZN_CONTROL_PLANE_ENV_FILE="$top/good.env" "$WRAPPER" sh -euc '[ "$PUBLIC_URL" = https://blazn.benpelo.com ] && [ "$BLAZN_INITIAL_DISPLAY_NAME" = "Ben Pelo" ]'
if sudo env BLAZN_CONTROL_PLANE_ENV_FILE="$top/bad.env" "$WRAPPER" true >"$top/out" 2>"$top/err"; then
  printf 'unsafe control-plane environment unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'unsupported shell syntax' "$top/err" >/dev/null
[ ! -e /tmp/should-not-run ]

trap - EXIT HUP INT TERM
cleanup
printf 'control-plane environment ownership and syntax tests passed\n'
