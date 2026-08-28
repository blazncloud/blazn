#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || { printf 'runtime persistence installation must run as root\n' >&2; exit 1; }
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
binary=/usr/libexec/blazn/blazn-controller-relay
relay_unit=/etc/systemd/system/blazn-controller-relay.service
identity_unit=/etc/systemd/system/blazn-identity-ngrok.service
tmpfiles=/etc/tmpfiles.d/blazn-runtime.conf
for command in install systemctl systemd-analyze systemd-tmpfiles python3; do
  command -v "$command" >/dev/null 2>&1 || { printf '%s is required\n' "$command" >&2; exit 1; }
done
getent passwd blazn-ngrok >/dev/null 2>&1 || { printf 'blazn-ngrok user is unavailable\n' >&2; exit 1; }
getent group blazn-ngrok >/dev/null 2>&1 || { printf 'blazn-ngrok group is unavailable\n' >&2; exit 1; }
python3 -c 'import ast,pathlib,sys; ast.parse(pathlib.Path(sys.argv[1]).read_text())' "$ROOT/controller-relay.py"
verify_dir=$(mktemp -d "${TMPDIR:-/tmp}/blazn-runtime-units.XXXXXX")
cleanup() { find "$verify_dir" -xdev -type f -delete; rmdir "$verify_dir"; }
trap cleanup EXIT HUP INT TERM
for unit in blazn-controller-relay.service blazn-identity-ngrok.service; do
  sed 's|^ExecStart=.*|ExecStart=/usr/bin/true|' "$ROOT/$unit" >"$verify_dir/$unit"
done
printf '[Unit]\nDescription=Control plane requiring healthy runtime helpers\nWants=blazn-controller-relay.service blazn-identity-ngrok.service\nAfter=blazn-controller-relay.service blazn-identity-ngrok.service\n[Service]\nExecStart=/usr/bin/true\n' >"$verify_dir/blazn-control-plane.service"
systemd-analyze verify "$verify_dir/blazn-control-plane.service" "$verify_dir/blazn-controller-relay.service" "$verify_dir/blazn-identity-ngrok.service"
cleanup
trap - EXIT HUP INT TERM
install -d -o root -g root -m 0755 /usr/libexec/blazn
install -o root -g root -m 0755 "$ROOT/controller-relay.py" "$binary"
install -o root -g root -m 0644 "$ROOT/blazn-controller-relay.service" "$relay_unit"
install -o root -g root -m 0644 "$ROOT/blazn-identity-ngrok.service" "$identity_unit"
install -o root -g root -m 0644 "$ROOT/blazn-runtime.tmpfiles" "$tmpfiles"
systemd-tmpfiles --create "$tmpfiles"
systemctl stop blazn-controller-relay.service blazn-identity-ngrok.service
systemctl daemon-reload
systemctl enable --now blazn-controller-relay.service blazn-identity-ngrok.service
printf 'Blazn reboot-persistent runtime services installed\n'
