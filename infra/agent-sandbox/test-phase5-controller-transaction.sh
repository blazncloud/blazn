#!/bin/sh
set -eu

# Executable proof for phase5-controller install/teardown: crash injection at
# every journal boundary, fail-closed prerequisites, replicas gate, scale, and
# UID-preconditioned teardown, against a fake kubectl state machine.
if [ "$(id -u)" -ne 0 ]; then exec sudo -n "$0" "$@"; fi
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
CONTROLLER=$ROOT/phase5-controller
for required in jq python3 flock sha256sum; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-controller.XXXXXX")
tx_root=/var/lib/blazn/phase5
tx_prefix=controller-test$$
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
  if [ "$blazn_root_created" -eq 1 ]; then rmdir "$tx_root" 2>/dev/null || :; rmdir /var/lib/blazn 2>/dev/null || :; fi
  find "$tmp" -mindepth 1 -xdev -delete
  rmdir "$tmp"
}
trap cleanup EXIT HUP INT TERM
[ ! -e /run/lock/blazn ] || { printf 'live lock path unexpectedly exists; refusing the disposable harness here\n' >&2; exit 1; }
test_lock_owned=1
if [ ! -d "$tx_root" ]; then blazn_root_created=1; install -d -o root -g root -m 0700 /var/lib/blazn "$tx_root"; fi

# Render a real controller manifest with reviewed-shaped fake inputs.
digest=$(printf '%064d' 0 | tr '0' 'a')
BLAZN_CONTROLLER_IMAGE="registry.blaze.internal:5000/blazn/sandbox-controller@sha256:$digest" \
BLAZN_SANDBOX_IO_IMAGE="registry.blaze.internal:5000/blazn/sandbox-io@sha256:$(printf '%064d' 0 | tr '0' 'b')" \
BLAZN_DATABASE_URL_SECRET_NAME=blazn-controller-database-url BLAZN_DATABASE_URL_SECRET_KEY=database-url \
BLAZN_DATABASE_ENDPOINT_KIND=ip \
BLAZN_KUBERNETES_API_CIDR=10.152.183.1/32 BLAZN_KUBERNETES_API_PORT=443 BLAZN_KUBERNETES_API_AUDIENCE=https://kubernetes.default.svc \
BLAZN_BEN1_POSTGRES_CIDR=192.168.0.100/32 BLAZN_BEN1_POSTGRES_PORT=5432 \
BLAZN_ACCESS_SOURCE_CIDR=192.168.0.108/32 \
BLAZN_OBJECT_SECRET_NAME=blazn-controller-object BLAZN_OBJECT_ACCESS_KEY=access-key BLAZN_OBJECT_SECRET_KEY=secret-key \
BLAZN_OBJECT_CA_KEY=object-ca BLAZN_REGISTRY_PULL_SECRET_NAME=blazn-registry-pull \
BLAZN_OBJECT_ENDPOINT_CIDR=192.168.0.100/32 BLAZN_OBJECT_ENDPOINT_PORT=9000 BLAZN_OBJECT_REGION=us-east-1 BLAZN_OBJECT_BUCKET=blazn-sandbox-artifacts \
BLAZN_SOURCE_HOST=github.com BLAZN_SOURCE_CIDR=140.82.112.3/32 BLAZN_SOURCE_DNS_CIDR=10.152.183.10/32 \
  "$CONTROLLER/render-install.sh" "$tmp/controller.yaml" >/dev/null
controller_sha=$(sha256sum "$tmp/controller.yaml" | awk '{print $1}')

FAKE_STATE=$tmp/state
mkdir -m 0700 "$tmp/bin" "$FAKE_STATE"
cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
printf 'kubectl %s\n' "$*" >>"$FAKE_STATE/calls.log"
present() { [ -e "$FAKE_STATE/$1" ] && [ ! -e "$FAKE_STATE/deleted-$1" ]; }
case "$*" in
  'config current-context') printf 'disposable-controller-test' ;;
  'get namespace kube-system -o jsonpath={.metadata.uid}') printf '99999999-9999-4999-8999-999999999999' ;;
  'get namespace blazn-poc-system') : ;;
  'get deployment agent-sandbox-controller -n agent-sandbox-system') present agent-sandbox-controller || { printf 'not found\n' >&2; exit 1; } ;;
  'get secret blazn-controller-database-url -n blazn-poc-system --ignore-not-found -o name') present db-secret && printf 'secret/blazn-controller-database-url\n' || : ;;
  'get secret blazn-controller-object -n blazn-poc-system --ignore-not-found -o name') present object-secret && printf 'secret/blazn-controller-object\n' || : ;;
  'get secret blazn-registry-pull -n blazn-poc-system --ignore-not-found -o name') present system-pull-secret && printf 'secret/blazn-registry-pull\n' || : ;;
  'get secret blazn-registry-pull -n blazn-poc-sandboxes --ignore-not-found -o name') present sandbox-pull-secret && printf 'secret/blazn-registry-pull\n' || : ;;
  'get deployment blazn-sandbox-controller -n blazn-poc-system --ignore-not-found -o name') present deployment && [ ! -e "$FAKE_STATE/scaled0" ] && printf 'deployment/blazn-sandbox-controller\n' || { present deployment && printf 'deployment/blazn-sandbox-controller\n' || :; } ;;
  'apply --server-side --field-manager blazn-phase5-controller -f '*) for object_key in deployment service access-ingress serviceaccount role rolebinding egress deny; do : >"$FAKE_STATE/$object_key"; done ;;
  'get deployment blazn-sandbox-controller -n blazn-poc-system -o jsonpath={.spec.replicas}') [ -e "$FAKE_STATE/scaled1" ] && printf '1' || printf '0' ;;
  'scale deployment blazn-sandbox-controller -n blazn-poc-system --replicas=1') : >"$FAKE_STATE/scaled1" ;;
  'scale deployment blazn-sandbox-controller -n blazn-poc-system --replicas=0') : >"$FAKE_STATE/scaled0"; rm -f "$FAKE_STATE/scaled1" ;;
  'get deployment blazn-sandbox-controller -n blazn-poc-system -o jsonpath={.status.conditions[?(@.type=="Available")].status}')
    [ "${FAKE_UNAVAILABLE:-0}" = 1 ] && printf 'False' || { [ -e "$FAKE_STATE/scaled1" ] && printf 'True' || printf 'False'; } ;;
  'get pods -n blazn-poc-system -o wide') : ;;
  'get pods -n blazn-poc-system -l app.kubernetes.io/name=blazn-sandbox-controller --no-headers') [ -e "$FAKE_STATE/scaled0" ] && : || { [ -e "$FAKE_STATE/scaled1" ] && printf 'blazn-sandbox-controller-x 1/1 Running\n' || :; } ;;
  'get deployment blazn-sandbox-controller -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '11111111-1111-4111-8111-111111111111' ;;
  'get role blazn-sandbox-controller -n blazn-poc-sandboxes -o jsonpath={.metadata.uid}') printf '22222222-2222-4222-8222-222222222222' ;;
  'get rolebinding blazn-sandbox-controller -n blazn-poc-sandboxes -o jsonpath={.metadata.uid}') printf '33333333-3333-4333-8333-333333333333' ;;
  'get serviceaccount blazn-sandbox-controller -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '44444444-4444-4444-8444-444444444444' ;;
  'get service blazn-sandbox-access -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '77777777-7777-4777-8777-777777777777' ;;
  'get networkpolicy blazn-sandbox-controller-access-ingress -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '88888888-8888-4888-8888-888888888888' ;;
  'get networkpolicy blazn-sandbox-controller-egress -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '55555555-5555-4555-8555-555555555555' ;;
  'get networkpolicy blazn-sandbox-controller-default-deny -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '66666666-6666-4666-8666-666666666666' ;;
  'get '*' --ignore-not-found -o name')
    # Match on the exact "kind name" prefix so same-named objects of different
    # kinds are never conflated (real kubectl scopes by kind).
    for object in deployment/blazn-sandbox-controller:deployment service/blazn-sandbox-access:service networkpolicy/blazn-sandbox-controller-access-ingress:access-ingress serviceaccount/blazn-sandbox-controller:serviceaccount role/blazn-sandbox-controller:role rolebinding/blazn-sandbox-controller:rolebinding networkpolicy/blazn-sandbox-controller-egress:egress networkpolicy/blazn-sandbox-controller-default-deny:deny; do
      ref=${object%%:*}; key=${object#*:}
      case "$* " in "get ${ref%%/*} ${ref#*/} "*) present "$key" && printf '%s\n' "$ref" || :; ;; esac
    done ;;
  'proxy --unix-socket='*)
	[ ! -e /proc/$$/fd/9 ] || { printf 'kubectl proxy inherited the live-cluster lock descriptor\n' >&2; exit 1; }
    socket=$(printf '%s' "$*" | sed 's/.*--unix-socket=\([^ ]*\).*/\1/')
    exec python3 - "$socket" "$FAKE_STATE" <<'PY'
import http.server, json, os, socketserver, sys
socket_path, state = sys.argv[1], sys.argv[2]
targets = {
    "/apis/apps/v1/namespaces/blazn-poc-system/deployments/blazn-sandbox-controller": ("deployment", "11111111-1111-4111-8111-111111111111"),
    "/api/v1/namespaces/blazn-poc-system/services/blazn-sandbox-access": ("service", "77777777-7777-4777-8777-777777777777"),
    "/apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-access-ingress": ("access-ingress", "88888888-8888-4888-8888-888888888888"),
    "/apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-egress": ("egress", "55555555-5555-4555-8555-555555555555"),
    "/apis/rbac.authorization.k8s.io/v1/namespaces/blazn-poc-sandboxes/rolebindings/blazn-sandbox-controller": ("rolebinding", "33333333-3333-4333-8333-333333333333"),
    "/apis/rbac.authorization.k8s.io/v1/namespaces/blazn-poc-sandboxes/roles/blazn-sandbox-controller": ("role", "22222222-2222-4222-8222-222222222222"),
    "/api/v1/namespaces/blazn-poc-system/serviceaccounts/blazn-sandbox-controller": ("serviceaccount", "44444444-4444-4444-8444-444444444444"),
    "/apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-default-deny": ("deny", "66666666-6666-4666-8666-666666666666"),
}
class Server(socketserver.UnixStreamServer): allow_reuse_address = True
class Handler(http.server.BaseHTTPRequestHandler):
    def do_DELETE(self):
        body = json.loads(self.rfile.read(int(self.headers.get("content-length", "0"))))
        key, uid = targets.get(self.path, (None, None))
        with open(os.path.join(state, "delete-requests.log"), "a") as log:
            log.write(f"{self.path} {json.dumps(body)}\n")
        if key is not None and os.path.exists(os.path.join(state, f"deleted-{key}")):
            self.send_response(404)
        elif key is not None and body.get("preconditions", {}).get("uid") == uid:
            open(os.path.join(state, f"deleted-{key}"), "w").close()
            self.send_response(200)
        else:
            self.send_response(409)
        self.send_header("content-type", "application/json"); self.end_headers(); self.wfile.write(b"{}")
    def log_message(self, *args): pass
Server(socket_path, Handler).serve_forever()
PY
    ;;
  *) printf 'unexpected kubectl invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod 0700 "$tmp/bin/kubectl"

reset_state() {
  find "$FAKE_STATE" -mindepth 1 -xdev -delete
  : >"$FAKE_STATE/agent-sandbox-controller"
  : >"$FAKE_STATE/db-secret"
  : >"$FAKE_STATE/object-secret"
  : >"$FAKE_STATE/system-pull-secret"
  : >"$FAKE_STATE/sandbox-pull-secret"
  : >"$FAKE_STATE/calls.log"
}
transaction_counter=0
new_transaction() { transaction_counter=$((transaction_counter + 1)); transaction=$tx_root/$tx_prefix-$transaction_counter; }
run_tool() {
  run_script=$1; shift
  if [ "$run_script" = install-controller.sh ]; then run_arg=$tmp/controller.yaml; else run_arg=''; fi
  set +e
  # shellcheck disable=SC2086
  "$ROOT/phase4c/with-live-lock.sh" env PATH="$tmp/bin:$PATH" FAKE_STATE="$FAKE_STATE" \
    BLAZN_EXPECTED_CONTEXT=disposable-controller-test \
    BLAZN_EXPECTED_KUBE_SYSTEM_UID=99999999-9999-4999-8999-999999999999 \
    BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary \
    BLAZN_CONTROLLER_TRANSACTION_DIR="$transaction" \
    BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 \
    BLAZN_EXPECTED_CONTROLLER_SHA256="$controller_sha" \
    BLAZN_DATABASE_URL_SECRET_NAME=blazn-controller-database-url \
    BLAZN_OBJECT_SECRET_NAME=blazn-controller-object \
    BLAZN_REGISTRY_PULL_SECRET_NAME=blazn-registry-pull \
    "$@" \
    "$CONTROLLER/$run_script" $run_arg >"$tmp/last-out" 2>"$tmp/last-err"
  last_code=$?
  set -e
}
expect_phase() { [ "$(cat "$transaction/phase")" = "$1" ] || { printf 'expected phase %s, got %s\n' "$1" "$(cat "$transaction/phase")" >&2; cat "$tmp/last-err" >&2; exit 1; }; }
expect_message() { grep -Fq -- "$1" "$tmp/last-err" || { printf 'missing expected message: %s\n' "$1" >&2; cat "$tmp/last-err" >&2; exit 1; }; }

# T1: happy deploy, then idempotent re-run.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase complete
[ -e "$FAKE_STATE/scaled1" ]
jq -e 'length == 8' "$transaction/owned-uids.json" >/dev/null
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
grep -Fq 'already complete' "$tmp/last-out"

# T2: crash at each journal boundary, then resume to completion.
for boundary in sealed apply-intent applied scaled complete; do
  reset_state; new_transaction
  run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER="$boundary" BLAZN_PHASE4C_DISPOSABLE_TEST=true
  [ "$last_code" -eq 86 ] || { printf 'boundary %s: expected 86, got %s\n' "$boundary" "$last_code" >&2; cat "$tmp/last-err" >&2; exit 1; }
  expect_phase "$boundary"
  run_tool install-controller.sh
  [ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
  expect_phase complete
done

# T3: a missing controller Secret blocks the deploy.
reset_state; rm -f "$FAKE_STATE/db-secret"; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'database Secret is not provisioned'
if grep -Fq 'apply --server-side' "$FAKE_STATE/calls.log"; then printf 'missing Secret must block apply\n' >&2; exit 1; fi

# T4: a missing Agent Sandbox controller blocks the deploy.
reset_state; rm -f "$FAKE_STATE/agent-sandbox-controller"; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'Agent Sandbox controller is not installed'

# T4b: either runtime namespace missing the pull Secret blocks before apply.
reset_state; rm -f "$FAKE_STATE/sandbox-pull-secret"; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'Sandbox registry pull Secret is not provisioned'
if grep -Fq 'apply --server-side' "$FAKE_STATE/calls.log"; then printf 'missing pull Secret must block apply\n' >&2; exit 1; fi

# T5: a controller that never becomes Available fails after scaling.
reset_state; new_transaction
run_tool install-controller.sh FAKE_UNAVAILABLE=1 BLAZN_CONTROLLER_AVAILABLE_ATTEMPTS=2
[ "$last_code" -eq 1 ]
expect_message 'never became Available'
expect_phase scaled

# T6: teardown scales to zero, deletes exactly the recorded UIDs, proves absence.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ -e "$FAKE_STATE/scaled0" ]
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 6 ]
grep -Fq '"uid": "11111111-1111-4111-8111-111111111111"' "$FAKE_STATE/delete-requests.log"

# T5b: a transaction stranded at 'scaled' can still be torn down (owned UIDs
# were recorded at apply-intent, before scaling).
reset_state; new_transaction
run_tool install-controller.sh FAKE_UNAVAILABLE=1 BLAZN_CONTROLLER_AVAILABLE_ATTEMPTS=2
[ "$last_code" -eq 1 ]
expect_phase scaled
[ -f "$transaction/owned-uids.json" ]
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 6 ]

# T5c: a resume after the scale succeeded but before its journal entry
# completes instead of failing on the sealed zero replicas.
reset_state; new_transaction
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=applied BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase applied
: >"$FAKE_STATE/scaled1"
run_tool install-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase complete

# T6b: a teardown that has already removed some objects resumes cleanly
# instead of aborting on a 404 for the already-deleted ones.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
# Pre-mark the Deployment and egress policy as already deleted, then tear down.
: >"$FAKE_STATE/deleted-deployment"; : >"$FAKE_STATE/deleted-egress"; rm -f "$FAKE_STATE/scaled1"; : >"$FAKE_STATE/scaled0"
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
# Only the four still-present objects were issued a precondition delete.
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 4 ]

# T6c: a resume that already removed only the same-named Role (its sibling
# ServiceAccount/RoleBinding/Deployment still present) skips the Role by kind,
# proving the absence pre-check is kind-scoped, not name-scoped.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
: >"$FAKE_STATE/deleted-role"; rm -f "$FAKE_STATE/scaled1"; : >"$FAKE_STATE/scaled0"
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 5 ]

# T7: a pre-existing controller Deployment blocks a fresh transaction.
reset_state; : >"$FAKE_STATE/deployment"; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'already exists'

# T8: path traversal outside the reviewed transaction root is rejected.
reset_state; transaction=$tx_root/controller-x/../$tx_prefix-evil
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'one clean segment'

printf 'Phase 5 controller deployment transaction proofs passed\n'
