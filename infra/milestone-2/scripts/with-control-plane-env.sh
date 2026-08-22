#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "control-plane environment loading must run as root"
[ "$#" -ge 1 ] || die "usage: with-control-plane-env.sh COMMAND [ARG...]"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600
env_size=$(wc -c <"$ENV_FILE" | tr -d ' ')
case $env_size in ''|*[!0-9]*) die "control-plane environment size is invalid" ;; esac
[ "$env_size" -le 32768 ] || die "control-plane environment is unexpectedly large"
if LC_ALL=C grep -Ev '^[A-Z][A-Z0-9_]*=("[a-zA-Z0-9 ._@:/+-]*"|[a-zA-Z0-9._@:/+-]*)[[:space:]]*$|^[[:space:]]*(#.*)?$' "$ENV_FILE" | grep . >/dev/null; then
  die "control-plane environment contains unsupported shell syntax"
fi
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a
exec "$@"
