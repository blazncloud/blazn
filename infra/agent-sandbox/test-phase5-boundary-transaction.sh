#!/bin/sh
set -eu

# Executable proof for phase5-boundary install/rollback: crash injection at
# every journal boundary, fail-closed discovery, foreign-object protection,
# and UID-preconditioned rollback, against a fake kubectl state machine.
if [ "$(id -u)" -ne 0 ]; then exec sudo -n "$0" "$@"; fi
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
for required in jq python3 flock sha256sum; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-transaction.XXXXXX")
tx_root=/var/lib/blazn/phase5
tx_prefix=boundary-test$$
blazn_root_created=0
test_lock_owned=0
cleanup() {
  for owned in "$tx_root/$tx_prefix"*; do
    if [ -d "$owned" ]; then find "$owned" -mindepth 1 -xdev -delete; rmdir "$owned"; fi
  done
  if [ "$test_lock_owned" -eq 1 ] && [ -d /run/lock/blazn ]; then
    find /run/lock/blazn -xdev -type f -delete
    find /run/lock/blazn -xdev -depth -type d -empty -delete
  fi
  if [ "$blazn_root_created" -eq 1 ]; then
    rmdir "$tx_root" 2>/dev/null || :
    rmdir /var/lib/blazn 2>/dev/null || :
  fi
  find "$tmp" -mindepth 1 -xdev -delete
  rmdir "$tmp"
}
trap cleanup EXIT HUP INT TERM
[ ! -e /run/lock/blazn ] || { printf 'live lock path unexpectedly exists; refusing the disposable harness here\n' >&2; exit 1; }
test_lock_owned=1
if [ ! -d "$tx_root" ]; then blazn_root_created=1; install -d -o root -g root -m 0700 /var/lib/blazn "$tx_root"; fi

tx_uuid=99999999-9999-4999-8999-999999999999
BLAZN_PHASE5_TRANSACTION_ID=$tx_uuid BLAZN_EXISTING_CLUSTER_QUEUE=m1-light "$ROOT/phase5-boundary/render-boundary.sh" "$tmp/boundary.yaml" >/dev/null
boundary_sha=$(sha256sum "$tmp/boundary.yaml" | awk '{print $1}')

FAKE_STATE=$tmp/state
mkdir -m 0700 "$tmp/bin" "$FAKE_STATE"
cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
printf 'kubectl %s\n' "$*" >>"$FAKE_STATE/calls.log"
exists() { [ -e "$FAKE_STATE/applied" ] && [ ! -e "$FAKE_STATE/deleted-$1" ]; }
emit_name() { if [ "${FAKE_NS_ERROR:-0}" = 1 ]; then printf 'fake discovery error\n' >&2; exit 1; fi; if exists "$1" || [ -e "$FAKE_STATE/foreign-$1" ]; then printf '%s\n' "$2"; fi; }
owned_json() {
  if [ -e "$FAKE_STATE/foreign-$1" ]; then printf '{"metadata":{"uid":"%s","annotations":{}}}' "$2"
  else printf '{"metadata":{"uid":"%s","annotations":{"blazn.dev/phase5-transaction":"99999999-9999-4999-8999-999999999999"}}}' "$2"; fi
}
case "$*" in
  'config current-context') printf 'disposable-phase5-test' ;;
  'get namespace kube-system -o jsonpath={.metadata.uid}') printf '99999999-9999-4999-8999-999999999999' ;;
  'version -o json') printf '{"serverVersion":{"major":"1","minor":"36"}}' ;;
  'get namespace blazn-poc-system --ignore-not-found -o name') emit_name ns-system namespace/blazn-poc-system ;;
  'get namespace blazn-poc-sandboxes --ignore-not-found -o name') emit_name ns-sandboxes namespace/blazn-poc-sandboxes ;;
  'get validatingadmissionpolicy blazn-sandbox-boundary --ignore-not-found -o name') emit_name vap validatingadmissionpolicy/blazn-sandbox-boundary ;;
  'get validatingadmissionpolicybinding blazn-sandbox-boundary --ignore-not-found -o name') emit_name vapb validatingadmissionpolicybinding/blazn-sandbox-boundary ;;
  'get clusterqueue.kueue.x-k8s.io m1-light -o jsonpath={.status.conditions[?(@.type=="Active")].status}')
    [ "${FAKE_CLUSTERQUEUE_INACTIVE:-0}" = 1 ] && printf 'False' || printf 'True' ;;
  'get crd localqueues.kueue.x-k8s.io -o json') printf '{"spec":{"versions":[{"name":"v1beta1","served":true}]}}' ;;
  'apply --server-side --field-manager blazn-phase5-boundary -f '*)
    [ "${FAKE_APPLY_FAIL:-0}" = 0 ] || { printf 'fake apply failure\n' >&2; exit 1; }
    : >"$FAKE_STATE/applied" ;;
  'get namespace blazn-poc-system -o json') owned_json ns-system 11111111-1111-4111-8111-111111111111 ;;
  'get namespace blazn-poc-sandboxes -o json') owned_json ns-sandboxes 22222222-2222-4222-8222-222222222222 ;;
  'get serviceaccount blazn-sandbox-runner -n blazn-poc-sandboxes -o json') owned_json sa 33333333-3333-4333-8333-333333333333 ;;
  'get localqueue.kueue.x-k8s.io blazn-poc -n blazn-poc-sandboxes -o json') owned_json lq 44444444-4444-4444-8444-444444444444 ;;
  'get role blazn-agent-sandbox-controller -n blazn-poc-sandboxes -o json') owned_json role 55555555-5555-4555-8555-555555555555 ;;
  'get rolebinding blazn-agent-sandbox-controller -n blazn-poc-sandboxes -o json') owned_json rb 66666666-6666-4666-8666-666666666666 ;;
  'get validatingadmissionpolicy blazn-sandbox-boundary -o json') owned_json vap 77777777-7777-4777-8777-777777777777 ;;
  'get validatingadmissionpolicybinding blazn-sandbox-boundary -o json') owned_json vapb 88888888-8888-4888-8888-888888888888 ;;
  'get serviceaccount blazn-sandbox-runner -n blazn-poc-sandboxes -o jsonpath={.automountServiceAccountToken}') printf 'false' ;;
  'get localqueue.kueue.x-k8s.io blazn-poc -n blazn-poc-sandboxes -o jsonpath={.spec.clusterQueue}') printf 'm1-light' ;;
  'get localqueue.kueue.x-k8s.io blazn-poc -n blazn-poc-sandboxes -o jsonpath={.status.conditions[?(@.type=="Active")].status}')
    [ "${FAKE_LOCALQUEUE_INACTIVE:-0}" = 1 ] && printf 'False' || printf 'True' ;;
  'get namespace blazn-poc-system -o jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}') printf 'restricted' ;;
  'get namespace blazn-poc-sandboxes -o jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}') printf 'restricted' ;;
  'get validatingadmissionpolicy blazn-sandbox-boundary -o jsonpath={.spec.failurePolicy}') printf 'Fail' ;;
  'get validatingadmissionpolicybinding blazn-sandbox-boundary -o jsonpath={.spec.validationActions[0]}') printf 'Deny' ;;
  'get pods -n blazn-poc-sandboxes --no-headers'|'get pods -n blazn-poc-system --no-headers')
    [ "${FAKE_PODS_REMAIN:-0}" = 1 ] && printf 'stray-pod 1/1 Running\n' || : ;;
  'get sandboxes.agents.x-k8s.io -n blazn-poc-sandboxes --no-headers'|'get sandboxes.agents.x-k8s.io -n blazn-poc-system --no-headers') : ;;
  'proxy --unix-socket='*)
    socket=${1#--unix-socket=}
    socket=$(printf '%s' "$*" | sed 's/.*--unix-socket=\([^ ]*\).*/\1/')
    exec python3 - "$socket" "$FAKE_STATE" <<'PY'
import http.server, json, os, socketserver, sys
socket_path, state = sys.argv[1], sys.argv[2]
targets = {
    "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/blazn-sandbox-boundary": ("vapb", "88888888-8888-4888-8888-888888888888"),
    "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/blazn-sandbox-boundary": ("vap", "77777777-7777-4777-8777-777777777777"),
    "/api/v1/namespaces/blazn-poc-sandboxes": ("ns-sandboxes", "22222222-2222-4222-8222-222222222222"),
    "/api/v1/namespaces/blazn-poc-system": ("ns-system", "11111111-1111-4111-8111-111111111111"),
}
class Server(socketserver.UnixStreamServer):
    allow_reuse_address = True
class Handler(http.server.BaseHTTPRequestHandler):
    def do_DELETE(self):
        body = json.loads(self.rfile.read(int(self.headers.get("content-length", "0"))))
        key, uid = targets.get(self.path, (None, None))
        ok = key is not None and body.get("preconditions", {}).get("uid") == uid and body.get("propagationPolicy") == "Foreground"
        with open(os.path.join(state, "delete-requests.log"), "a") as log:
            log.write(f"{self.path} {json.dumps(body)}\n")
        if ok:
            open(os.path.join(state, f"deleted-{key}"), "w").close()
            self.send_response(200)
        else:
            self.send_response(409)
        self.send_header("content-type", "application/json"); self.end_headers(); self.wfile.write(b"{}")
    def log_message(self, *args):
        pass
Server(socket_path, Handler).serve_forever()
PY
    ;;
  *) printf 'unexpected kubectl invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod 0700 "$tmp/bin/kubectl"

reset_state() {
  find "$FAKE_STATE" -mindepth 1 -xdev -delete
  : >"$FAKE_STATE/calls.log"
}
transaction_counter=0
new_transaction() { transaction_counter=$((transaction_counter + 1)); transaction=$tx_root/$tx_prefix-$transaction_counter; }
run_tool() {
  run_script=$1; shift
  if [ "$run_script" = install-boundary.sh ]; then run_arg=$tmp/boundary.yaml; else run_arg=''; fi
  set +e
  # shellcheck disable=SC2086
  "$ROOT/phase4c/with-live-lock.sh" env PATH="$tmp/bin:$PATH" \
    FAKE_STATE="$FAKE_STATE" \
    BLAZN_EXPECTED_CONTEXT=disposable-phase5-test \
    BLAZN_EXPECTED_KUBE_SYSTEM_UID=99999999-9999-4999-8999-999999999999 \
    BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
    BLAZN_PHASE5_TRANSACTION_DIR="$transaction" \
    BLAZN_PHASE5_TRANSACTION_ID=$tx_uuid \
    BLAZN_EXPECTED_BOUNDARY_SHA256="$boundary_sha" \
    "$@" \
    "$ROOT/phase5-boundary/$run_script" $run_arg >"$tmp/last-out" 2>"$tmp/last-err"
  last_code=$?
  set -e
}
expect_phase() { [ "$(cat "$transaction/phase")" = "$1" ] || { printf 'expected phase %s, got %s\n' "$1" "$(cat "$transaction/phase")" >&2; cat "$tmp/last-err" >&2; exit 1; }; }
expect_message() { grep -Fq -- "$1" "$tmp/last-err" || { printf 'missing expected message: %s\n' "$1" >&2; cat "$tmp/last-err" >&2; exit 1; }; }

# T1: happy install, then idempotent re-run.
reset_state
new_transaction
run_tool install-boundary.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase complete
jq -e 'length == 8' "$transaction/owned-uids.json" >/dev/null
run_tool install-boundary.sh
[ "$last_code" -eq 0 ]
grep -Fq 'already complete' "$tmp/last-out"

# T2: crash at each journal boundary, then resume to completion.
for boundary in sealed apply-intent applied complete; do
  reset_state
  new_transaction
  run_tool install-boundary.sh BLAZN_PHASE4C_FAIL_AFTER="$boundary" BLAZN_PHASE4C_DISPOSABLE_TEST=true
  [ "$last_code" -eq 86 ] || { printf 'boundary %s: expected 86, got %s\n' "$boundary" "$last_code" >&2; cat "$tmp/last-err" >&2; exit 1; }
  expect_phase "$boundary"
  run_tool install-boundary.sh
  [ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
  expect_phase complete
done

# T3: discovery failure fails closed before any mutation.
reset_state
new_transaction
run_tool install-boundary.sh FAKE_NS_ERROR=1
[ "$last_code" -eq 1 ]
expect_message 'could not be verified'
expect_phase sealed
if grep -Fq 'apply --server-side' "$FAKE_STATE/calls.log"; then printf 'discovery failure must block apply\n' >&2; exit 1; fi

# T4: a foreign namespace blocks a resumed apply.
reset_state
new_transaction
run_tool install-boundary.sh BLAZN_PHASE4C_FAIL_AFTER=apply-intent BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
touch "$FAKE_STATE/foreign-ns-system"
: >"$FAKE_STATE/calls.log"
run_tool install-boundary.sh
[ "$last_code" -eq 1 ]
expect_message 'exists without this transaction identity'
if grep -Fq 'apply --server-side' "$FAKE_STATE/calls.log"; then printf 'foreign namespace must block apply\n' >&2; exit 1; fi

# T5: an inactive ClusterQueue blocks installation.
reset_state
new_transaction
run_tool install-boundary.sh FAKE_CLUSTERQUEUE_INACTIVE=1
[ "$last_code" -eq 1 ]
expect_message 'is not Active'

# T6: rollback of a never-applied transaction completes without mutation.
reset_state
new_transaction
run_tool install-boundary.sh BLAZN_PHASE4C_FAIL_AFTER=sealed BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
: >"$FAKE_STATE/calls.log"
run_tool rollback-boundary.sh
[ "$last_code" -eq 0 ]
expect_phase rollback-complete
if grep -Eq 'proxy|delete' "$FAKE_STATE/calls.log"; then printf 'pre-apply rollback must not mutate\n' >&2; exit 1; fi

# T7: full rollback deletes exactly the owned identities with UID preconditions.
reset_state
new_transaction
run_tool install-boundary.sh
[ "$last_code" -eq 0 ]
run_tool rollback-boundary.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 4 ]
grep -Fq '"uid": "22222222-2222-4222-8222-222222222222"' "$FAKE_STATE/delete-requests.log"

# T8: rollback refuses while Pods remain in an owned namespace.
reset_state
new_transaction
run_tool install-boundary.sh
[ "$last_code" -eq 0 ]
rm -f "$FAKE_STATE/delete-requests.log"
run_tool rollback-boundary.sh FAKE_PODS_REMAIN=1
[ "$last_code" -eq 1 ]
expect_message 'still runs Pods'
if [ -e "$FAKE_STATE/deleted-ns-sandboxes" ]; then printf 'occupied namespace must not be deleted\n' >&2; exit 1; fi

# T9: path traversal outside the reviewed transaction root is rejected.
reset_state
transaction=$tx_root/boundary-x/../$tx_prefix-evil
run_tool install-boundary.sh
[ "$last_code" -eq 1 ]
expect_message 'one clean segment'

printf 'Phase 5 boundary transaction proofs passed\n'
