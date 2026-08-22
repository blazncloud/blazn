#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then exec sudo -n "$0" "$@"; fi
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/phase4c/lib.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-transaction.XXXXXX")
cleanup() {
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || :; wait "$server_pid" 2>/dev/null || :; fi
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -type s -delete
  find "$tmp" -xdev -depth -type d -empty -delete
  if [ "${test_lock_owned:-0}" -eq 1 ]; then
    find /run/lock/blazn -xdev -type f -delete
    find /run/lock/blazn -xdev -depth -type d -empty -delete
  fi
}
trap cleanup EXIT HUP INT TERM
mkdir -m 0700 "$tmp/fixtures" "$tmp/pre"

printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  annotations:' '    blazn.dev/phase4c-transaction: 77777777-7777-4777-8777-777777777777' '  name: install' >"$tmp/install.yaml"
for file in blazn-poc.yaml bootstrap.yaml controller-boundary.yaml synthetic-canary.yaml; do printf 'fixture: %s\n' "$file" >"$tmp/fixtures/$file"; done
for file in api-resources.txt clusterqueues.json context creator-principal kube-system.uid phase4c-targets relevant-admission.txt relevant-crds.txt runtimeclasses.json version.json; do printf 'inventory: %s\n' "$file" >"$tmp/pre/$file"; done
printf 'disposable-phase4c-test\n' >"$tmp/pre/context"
printf '99999999-9999-4999-8999-999999999999\n' >"$tmp/pre/kube-system.uid"
printf 'phase4c-reviewer\n' >"$tmp/pre/creator-principal"
(
  cd "$tmp/pre"
  sha256sum api-resources.txt clusterqueues.json context creator-principal kube-system.uid phase4c-targets relevant-admission.txt relevant-crds.txt runtimeclasses.json version.json | LC_ALL=C sort >inventory.sha256
)
"$ROOT/phase4c/prepare-transaction.sh" "$tmp/install.yaml" "$tmp/fixtures" "$tmp/pre" "$tmp/transaction" >/dev/null
digest=$(cat "$tmp/transaction/input.digest")
BLAZN_REVIEWED_INPUT_DIGEST=$digest phase4c_verify_transaction "$tmp/transaction"

# Caller files are no longer authoritative after sealing; sealed bytes are.
printf 'caller changed\n' >"$tmp/install.yaml"
BLAZN_REVIEWED_INPUT_DIGEST=$digest phase4c_verify_transaction "$tmp/transaction"
chmod 0600 "$tmp/transaction/install.yaml"
printf 'sealed changed\n' >>"$tmp/transaction/install.yaml"
if BLAZN_REVIEWED_INPUT_DIGEST=$digest phase4c_verify_transaction "$tmp/transaction" 2>/dev/null; then printf 'tampered sealed input was accepted\n' >&2; exit 1; fi

# Prove every mutating boundary persists its phase before a disposable crash,
# so a fresh process can resume from the recorded value.
for journal_phase in foundation-intent foundation-applied controller-intent controller-applied bootstrap-intent bootstrap-ready bootstrap-complete controller-ready canary-intent canary-ready canary-clean rollback-intent rollback-complete; do
  BLAZN_PHASE4C_FAIL_AFTER=$journal_phase BLAZN_PHASE4C_DISPOSABLE_TEST=true phase4c_write_phase "$tmp/transaction" "$journal_phase" && exit 1 || code=$?
  [ "$code" -eq 86 ]
  [ "$(cat "$tmp/transaction/phase")" = "$journal_phase" ]
done

# Exercise the production UID-precondition JSON against a disposable private
# Unix socket and inspect the exact DeleteOptions body.
phase4c_proxy_socket=$tmp/api.sock
python3 - "$phase4c_proxy_socket" "$tmp/delete-body" <<'PY' &
import http.server, socketserver, sys
class Server(socketserver.UnixStreamServer): allow_reuse_address=True
class Handler(http.server.BaseHTTPRequestHandler):
    def do_DELETE(self):
        body=self.rfile.read(int(self.headers.get('content-length','0')))
        open(sys.argv[2],'wb').write(body)
        self.send_response(200); self.send_header('content-type','application/json'); self.end_headers(); self.wfile.write(b'{}')
    def log_message(self,*args): pass
Server(sys.argv[1],Handler).handle_request()
PY
server_pid=$!
attempt=0
while [ ! -S "$phase4c_proxy_socket" ]; do attempt=$((attempt+1)); [ "$attempt" -lt 50 ]; sleep 0.1; done
phase4c_delete_uid '/api/v1/namespaces/blazn-poc' '88888888-8888-4888-8888-888888888888'
wait "$server_pid"; server_pid=''
jq -e '.kind == "DeleteOptions" and .propagationPolicy == "Foreground" and .preconditions.uid == "88888888-8888-4888-8888-888888888888"' "$tmp/delete-body" >/dev/null

# A sealed (pre-mutation) transaction rolls back idempotently under the real
# inherited lock/fencing launcher and leaves no cluster command to perform.
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  annotations:' '    blazn.dev/phase4c-transaction: 77777777-7777-4777-8777-777777777777' '  name: install' >"$tmp/install.yaml"
"$ROOT/phase4c/prepare-transaction.sh" "$tmp/install.yaml" "$tmp/fixtures" "$tmp/pre" "$tmp/rollback-transaction" >/dev/null
rollback_digest=$(cat "$tmp/rollback-transaction/input.digest")
mkdir "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
case "$*" in
  'config current-context') printf 'disposable-phase4c-test' ;;
  *'get namespace kube-system'*) printf '99999999-9999-4999-8999-999999999999' ;;
  *) printf 'sealed rollback unexpectedly invoked kubectl: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod 0700 "$tmp/bin/kubectl"
[ ! -e /run/lock/blazn ] || { printf 'disposable lock path unexpectedly exists\n' >&2; exit 1; }
test_lock_owned=1
BLAZN_EXPECTED_CONTEXT=disposable-phase4c-test \
BLAZN_EXPECTED_KUBE_SYSTEM_UID=99999999-9999-4999-8999-999999999999 \
BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
BLAZN_REVIEWED_INPUT_DIGEST=$rollback_digest \
  "$ROOT/phase4c/with-live-lock.sh" env PATH="$tmp/bin:$PATH" \
    BLAZN_EXPECTED_CONTEXT=disposable-phase4c-test \
    BLAZN_EXPECTED_KUBE_SYSTEM_UID=99999999-9999-4999-8999-999999999999 \
    BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
    BLAZN_REVIEWED_INPUT_DIGEST="$rollback_digest" \
    "$ROOT/phase4c/rollback.sh" "$tmp/rollback-transaction"
[ "$(cat "$tmp/rollback-transaction/phase")" = rollback-complete ]

printf 'Phase 4C sealed-input, journal failpoint, UID-delete, and rollback transaction tests passed\n'
