#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
python3 -c 'import ast,pathlib,sys; ast.parse(pathlib.Path(sys.argv[1]).read_text())' "$ROOT/controller-relay.py"
verify_dir=$(mktemp -d "${TMPDIR:-/tmp}/blazn-runtime-units.XXXXXX")
cleanup() { find "$verify_dir" -xdev -type f -delete; rmdir "$verify_dir"; }
trap cleanup EXIT HUP INT TERM
for unit in blazn-controller-relay.service blazn-identity-ngrok.service; do
  sed 's|^ExecStart=.*|ExecStart=/usr/bin/true|' "$ROOT/$unit" >"$verify_dir/$unit"
done
printf '[Unit]\nDescription=Static verification placeholder\n[Service]\nExecStart=/usr/bin/true\n' >"$verify_dir/blazn-control-plane.service"
systemd-analyze verify "$verify_dir/blazn-control-plane.service" "$verify_dir/blazn-controller-relay.service" "$verify_dir/blazn-identity-ngrok.service"
cleanup
trap - EXIT HUP INT TERM
grep -Fx 'd /run/lock/blazn 0700 root root -' "$ROOT/blazn-runtime.tmpfiles" >/dev/null
grep -F 'BLAZN_CONTROLLER_RELAY_BIND=192.168.0.100' "$ROOT/blazn-controller-relay.service" >/dev/null
grep -F '127.0.0.1:58081 --url https://auth.blazn.benpelo.com' "$ROOT/blazn-identity-ngrok.service" >/dev/null
grep -F 'User=blazn-ngrok' "$ROOT/blazn-identity-ngrok.service" >/dev/null
grep -F 'NoNewPrivileges=yes' "$ROOT/blazn-controller-relay.service" >/dev/null
grep -F 'NoNewPrivileges=yes' "$ROOT/blazn-identity-ngrok.service" >/dev/null
if grep -Ei '(token|password|secret)[=:][^[:space:]]+' "$ROOT"/*.service "$ROOT"/*.tmpfiles >/dev/null; then
  printf 'runtime persistence unit contains credential-like material\n' >&2
  exit 1
fi
printf 'runtime persistence static contract: ok\n'
