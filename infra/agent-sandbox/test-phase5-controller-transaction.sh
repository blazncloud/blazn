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
BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 \
BLAZN_KUBERNETES_API_CIDR=10.152.183.1/32 BLAZN_KUBERNETES_API_PORT=443 BLAZN_KUBERNETES_API_AUDIENCE=https://kubernetes.default.svc BLAZN_KUBERNETES_CLUSTER_ID=cluster-test \
BLAZN_BEN1_POSTGRES_CIDR=192.168.0.100/32 BLAZN_BEN1_POSTGRES_PORT=5432 \
BLAZN_ACCESS_SERVICE_CLUSTER_IP=10.152.183.207 BLAZN_ACCESS_SOURCE_CIDR=192.168.0.108/32 \
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
present() {
  if [ "$1" = anchor ] && [ -e "$FAKE_STATE/anchor-delete-pending" ]; then
    if [ -e "$FAKE_STATE/anchor-delete-observed" ]; then
      : >"$FAKE_STATE/deleted-anchor"
      for dependent in deployment service access-ingress serviceaccount role rolebinding clusterrole clusterrolebinding egress deny; do [ -e "$FAKE_STATE/user-$dependent" ] || : >"$FAKE_STATE/deleted-$dependent"; done
    else : >"$FAKE_STATE/anchor-delete-observed"
    fi
  fi
  [ -e "$FAKE_STATE/$1" ] && [ ! -e "$FAKE_STATE/deleted-$1" ]
}
emit_object() {
  object_key=$1
  case "$object_key" in
    serviceaccount) api=v1; kind=ServiceAccount; name=blazn-sandbox-controller; ns=blazn-poc-system; uid=44444444-4444-4444-8444-444444444444 ;;
    role) api=rbac.authorization.k8s.io/v1; kind=Role; name=blazn-sandbox-controller; ns=blazn-poc-sandboxes; uid=22222222-2222-4222-8222-222222222222 ;;
    clusterrole) api=rbac.authorization.k8s.io/v1; kind=ClusterRole; name=blazn-sandbox-controller-node-observer; ns=; uid=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa ;;
    deployment) api=apps/v1; kind=Deployment; name=blazn-sandbox-controller; ns=blazn-poc-system; uid=11111111-1111-4111-8111-111111111111 ;;
    service) api=v1; kind=Service; name=blazn-sandbox-access; ns=blazn-poc-system; uid=77777777-7777-4777-8777-777777777777 ;;
    deny) api=networking.k8s.io/v1; kind=NetworkPolicy; name=blazn-sandbox-controller-default-deny; ns=blazn-poc-system; uid=66666666-6666-4666-8666-666666666666 ;;
    access-ingress) api=networking.k8s.io/v1; kind=NetworkPolicy; name=blazn-sandbox-controller-access-ingress; ns=blazn-poc-system; uid=88888888-8888-4888-8888-888888888888 ;;
    egress) api=networking.k8s.io/v1; kind=NetworkPolicy; name=blazn-sandbox-controller-egress; ns=blazn-poc-system; uid=55555555-5555-4555-8555-555555555555 ;;
    rolebinding) api=rbac.authorization.k8s.io/v1; kind=RoleBinding; name=blazn-sandbox-controller; ns=blazn-poc-sandboxes; uid=33333333-3333-4333-8333-333333333333 ;;
    clusterrolebinding) api=rbac.authorization.k8s.io/v1; kind=ClusterRoleBinding; name=blazn-sandbox-controller-node-observer; ns=; uid=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb ;;
    anchor) api=rbac.authorization.k8s.io/v1; kind=ClusterRole; name=blazn-phase5-anchor-99999999-9999-4999-8999-999999999999; ns=; uid=cccccccc-cccc-4ccc-8ccc-cccccccccccc ;;
    *) exit 1 ;;
  esac
  if [ -n "$ns" ]; then namespace_json=$(printf ',"namespace":"%s"' "$ns"); else namespace_json=; fi
  if [ -e "$FAKE_STATE/user-$object_key" ]; then uid=00000000-0000-4000-8000-000000000000; owner_uid=00000000-0000-4000-8000-000000000000; else owner_uid=cccccccc-cccc-4ccc-8ccc-cccccccccccc; fi
  if [ "${EMIT_LIVE:-0}" = 1 ] && [ "${FAKE_SEMANTIC_DRIFT:-}" = "$object_key" ]; then drift_json=',"unexpected":{"mutated":true}'; else drift_json=; fi
  if [ "${EMIT_ADMISSION:-0}" = 1 ] && [ "${FAKE_ADMISSION_MUTATION:-}" = "$object_key" ]; then admission_json=',"rules":[{"apiGroups":["*"],"resources":["*"],"verbs":["*"]}]'; else admission_json=; fi
  if [ "$object_key" = deployment ]; then
    if [ -e "$FAKE_STATE/scaled1" ]; then resource_version=rv-2; available=True; replicas=1; else resource_version=rv-1; available=False; replicas=0; fi
    [ "${FAKE_UNAVAILABLE:-0}" = 1 ] && available=False
    spec_json=$(printf ',"spec":{"replicas":%s}' "$replicas")
    status_json=$(printf ',"status":{"conditions":[{"type":"Available","status":"%s"}]}' "$available")
  else resource_version=rv-1; spec_json=; status_json=; fi
  if [ "${EMIT_ADMISSION:-0}" = 1 ] && [ "${FAKE_EXPLICIT_MUTATION:-}" = "$object_key" ]; then explicit_json=',"reviewedField":"mutated"'; else explicit_json=; fi
  printf '{"apiVersion":"%s","kind":"%s","metadata":{"name":"%s"%s,"uid":"%s","resourceVersion":"%s","labels":{"blazn.dev/phase5-object":"%s"},"annotations":{"blazn.dev/phase5-transaction":"99999999-9999-4999-8999-999999999999"},"ownerReferences":[{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","name":"blazn-phase5-anchor-99999999-9999-4999-8999-999999999999","uid":"%s","controller":false,"blockOwnerDeletion":false}]}%s%s%s%s%s}\n' "$api" "$kind" "$name" "$namespace_json" "$uid" "$resource_version" "$object_key" "$owner_uid" "$drift_json" "$admission_json" "$explicit_json" "$spec_json" "$status_json"
}
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
  'create -f '*' -o json')
    : >"$FAKE_STATE/anchor"
    printf '{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"blazn-phase5-anchor-99999999-9999-4999-8999-999999999999","uid":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","annotations":{"blazn.dev/phase5-transaction":"99999999-9999-4999-8999-999999999999"}},"rules":[]}\n' ;;
  'apply --server-side --dry-run=server --field-manager blazn-phase5-controller -f '*' -l blazn.dev/phase5-object='*' -o json')
    object_key=$(printf '%s' "$*" | sed 's/.*phase5-object=\([^ ]*\).*/\1/')
    EMIT_ADMISSION=1 emit_object "$object_key" ;;
  'apply --dry-run=client --field-manager blazn-phase5-controller -f '*' -l blazn.dev/phase5-object='*' -o json')
    object_key=$(printf '%s' "$*" | sed 's/.*phase5-object=\([^ ]*\).*/\1/')
    emit_object "$object_key" ;;
  'apply --server-side --field-manager blazn-phase5-controller -f '*' -l blazn.dev/phase5-object='*' -o json')
    object_key=$(printf '%s' "$*" | sed 's/.*phase5-object=\([^ ]*\).*/\1/')
    : >"$FAKE_STATE/$object_key"
    [ "${FAKE_PARTIAL_APPLY:-0}" = 1 ] && [ "$object_key" = rolebinding ] && exit 1
    EMIT_ADMISSION=1 emit_object "$object_key" ;;
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
  'get clusterrole blazn-sandbox-controller-node-observer -o jsonpath={.metadata.uid}') printf 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa' ;;
  'get clusterrolebinding blazn-sandbox-controller-node-observer -o jsonpath={.metadata.uid}') printf 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb' ;;
  'get serviceaccount blazn-sandbox-controller -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '44444444-4444-4444-8444-444444444444' ;;
  'get service blazn-sandbox-access -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '77777777-7777-4777-8777-777777777777' ;;
  'get networkpolicy blazn-sandbox-controller-access-ingress -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '88888888-8888-4888-8888-888888888888' ;;
  'get networkpolicy blazn-sandbox-controller-egress -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '55555555-5555-4555-8555-555555555555' ;;
  'get networkpolicy blazn-sandbox-controller-default-deny -n blazn-poc-system -o jsonpath={.metadata.uid}') printf '66666666-6666-4666-8666-666666666666' ;;
  'get clusterrole blazn-phase5-anchor-99999999-9999-4999-8999-999999999999 -o jsonpath={.metadata.uid}') printf 'cccccccc-cccc-4ccc-8ccc-cccccccccccc' ;;
  'get clusterrole blazn-phase5-anchor-99999999-9999-4999-8999-999999999999 -o json')
    present anchor || exit 1
    if [ -e "$FAKE_STATE/user-anchor" ]; then anchor_uid=dddddddd-dddd-4ddd-8ddd-dddddddddddd; else anchor_uid=cccccccc-cccc-4ccc-8ccc-cccccccccccc; fi
    printf '{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"blazn-phase5-anchor-99999999-9999-4999-8999-999999999999","uid":"%s","annotations":{"blazn.dev/phase5-transaction":"99999999-9999-4999-8999-999999999999"}},"rules":[]}\n' "$anchor_uid" ;;
  'get '*' --ignore-not-found -o json')
    for object in deployment/blazn-sandbox-controller:deployment service/blazn-sandbox-access:service networkpolicy/blazn-sandbox-controller-access-ingress:access-ingress serviceaccount/blazn-sandbox-controller:serviceaccount role/blazn-sandbox-controller:role rolebinding/blazn-sandbox-controller:rolebinding clusterrole/blazn-sandbox-controller-node-observer:clusterrole clusterrolebinding/blazn-sandbox-controller-node-observer:clusterrolebinding networkpolicy/blazn-sandbox-controller-egress:egress networkpolicy/blazn-sandbox-controller-default-deny:deny clusterrole/blazn-phase5-anchor-99999999-9999-4999-8999-999999999999:anchor; do
      ref=${object%%:*}; key=${object#*:}
      case "$* " in "get ${ref%%/*} ${ref#*/} "*)
        [ ! -e "$FAKE_STATE/fail-get-$key" ] || exit 1
        present "$key" && { EMIT_LIVE=1 EMIT_ADMISSION=1 emit_object "$key"; }
        exit 0
        ;;
      esac
    done
    exit 1 ;;
  'get '*' -o json')
    for object in deployment/blazn-sandbox-controller:deployment:11111111-1111-4111-8111-111111111111 service/blazn-sandbox-access:service:77777777-7777-4777-8777-777777777777 networkpolicy/blazn-sandbox-controller-access-ingress:access-ingress:88888888-8888-4888-8888-888888888888 serviceaccount/blazn-sandbox-controller:serviceaccount:44444444-4444-4444-8444-444444444444 role/blazn-sandbox-controller:role:22222222-2222-4222-8222-222222222222 rolebinding/blazn-sandbox-controller:rolebinding:33333333-3333-4333-8333-333333333333 clusterrole/blazn-sandbox-controller-node-observer:clusterrole:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa clusterrolebinding/blazn-sandbox-controller-node-observer:clusterrolebinding:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb networkpolicy/blazn-sandbox-controller-egress:egress:55555555-5555-4555-8555-555555555555 networkpolicy/blazn-sandbox-controller-default-deny:deny:66666666-6666-4666-8666-666666666666; do
      ref=${object%%:*}; rest=${object#*:}; key=${rest%%:*}; uid=${rest#*:}
      case "$* " in "get ${ref%%/*} ${ref#*/} "*)
        [ ! -e "$FAKE_STATE/fail-get-$key" ] || exit 1
        present "$key" || exit 1
        EMIT_LIVE=1 EMIT_ADMISSION=1 emit_object "$key"
        exit 0
        ;;
      esac
    done
    exit 1 ;;
  'get '*' --ignore-not-found -o name')
    case "$*" in 'get clusterrole blazn-phase5-anchor-99999999-9999-4999-8999-999999999999 --ignore-not-found -o name') present anchor && printf 'clusterrole/blazn-phase5-anchor-99999999-9999-4999-8999-999999999999\n' || :; exit 0 ;; esac
    # Match on the exact "kind name" prefix so same-named objects of different
    # kinds are never conflated (real kubectl scopes by kind).
    for object in deployment/blazn-sandbox-controller:deployment service/blazn-sandbox-access:service networkpolicy/blazn-sandbox-controller-access-ingress:access-ingress serviceaccount/blazn-sandbox-controller:serviceaccount role/blazn-sandbox-controller:role rolebinding/blazn-sandbox-controller:rolebinding clusterrole/blazn-sandbox-controller-node-observer:clusterrole clusterrolebinding/blazn-sandbox-controller-node-observer:clusterrolebinding networkpolicy/blazn-sandbox-controller-egress:egress networkpolicy/blazn-sandbox-controller-default-deny:deny; do
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
    "/apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-sandbox-controller-node-observer": ("clusterrole", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
    "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-sandbox-controller-node-observer": ("clusterrolebinding", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
    "/apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-phase5-anchor-99999999-9999-4999-8999-999999999999": ("anchor", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
    "/api/v1/namespaces/blazn-poc-system/serviceaccounts/blazn-sandbox-controller": ("serviceaccount", "44444444-4444-4444-8444-444444444444"),
    "/apis/networking.k8s.io/v1/namespaces/blazn-poc-system/networkpolicies/blazn-sandbox-controller-default-deny": ("deny", "66666666-6666-4666-8666-666666666666"),
}
class Server(socketserver.UnixStreamServer): allow_reuse_address = True
class Handler(http.server.BaseHTTPRequestHandler):
    def do_PATCH(self):
        body = json.loads(self.rfile.read(int(self.headers.get("content-length", "0"))))
        current_rv = "rv-2" if os.path.exists(os.path.join(state, "scaled1")) else "rv-1"
        target_replicas = body[-1].get("value") if body else None
        expected = [
            {"op": "test", "path": "/metadata/uid", "value": "11111111-1111-4111-8111-111111111111"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": current_rv},
            {"op": "replace", "path": "/spec/replicas", "value": target_replicas},
        ]
        race = os.path.exists(os.path.join(state, "race-deployment")) if target_replicas == 1 else os.path.exists(os.path.join(state, "race-teardown-deployment"))
        if self.path.endswith("/deployments/blazn-sandbox-controller") and body == expected and target_replicas in (0, 1) and not race:
            if target_replicas == 1:
                open(os.path.join(state, "scaled1"), "w").close()
            else:
                open(os.path.join(state, "scaled0"), "w").close()
                try: os.unlink(os.path.join(state, "scaled1"))
                except FileNotFoundError: pass
            self.send_response(200)
        else:
            if race:
                open(os.path.join(state, "user-deployment"), "w").close()
            self.send_response(409)
        self.send_header("content-type", "application/json"); self.end_headers(); self.wfile.write(b"{}")
    def do_DELETE(self):
        body = json.loads(self.rfile.read(int(self.headers.get("content-length", "0"))))
        key, uid = targets.get(self.path, (None, None))
        with open(os.path.join(state, "delete-requests.log"), "a") as log:
            log.write(f"{self.path} {json.dumps(body)}\n")
        if key is not None and os.path.exists(os.path.join(state, f"arm-get-failure-after-delete-{key}")):
            open(os.path.join(state, f"fail-get-{key}"), "w").close()
        if key is not None and os.path.exists(os.path.join(state, f"fail-delete-{key}")):
            self.send_response(500)
        elif key is not None and os.path.exists(os.path.join(state, f"deleted-{key}")):
            self.send_response(404)
        elif key is not None and os.path.exists(os.path.join(state, f"user-{key}")):
            self.send_response(409)
        elif key is not None and body.get("preconditions", {}).get("uid") == uid:
            if key == "anchor" and os.path.exists(os.path.join(state, "defer-anchor-delete")):
                open(os.path.join(state, "anchor-delete-pending"), "w").close()
            else:
                open(os.path.join(state, f"deleted-{key}"), "w").close()
            if key == "anchor" and not os.path.exists(os.path.join(state, "defer-anchor-delete")):
                for dependent in ("deployment", "service", "access-ingress", "serviceaccount", "role", "rolebinding", "clusterrole", "clusterrolebinding", "egress", "deny"):
                    if os.path.exists(os.path.join(state, dependent)) and not os.path.exists(os.path.join(state, f"user-{dependent}")):
                        open(os.path.join(state, f"deleted-{dependent}"), "w").close()
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
expect_journal_length() { actual=$(jq -r 'length' "$transaction/owned-uids.json" 2>/dev/null || printf missing); [ "$actual" = "$1" ] || { printf 'expected UID journal length %s, got %s\n' "$1" "$actual" >&2; exit 1; }; }
expect_code() { [ "$last_code" -eq "$1" ] || { printf 'expected exit %s, got %s\n' "$1" "$last_code" >&2; cat "$tmp/last-err" >&2; exit 1; }; }
expect_delete_count() { actual=$(grep -c preconditions "$FAKE_STATE/delete-requests.log" 2>/dev/null || :); [ "$actual" = "$1" ] || { printf 'expected %s UID delete requests, got %s\n' "$1" "$actual" >&2; cat "$tmp/last-err" >&2; exit 1; }; }
expect_real_apply_count() { actual=$(grep -c 'apply --server-side --field-manager' "$FAKE_STATE/calls.log" 2>/dev/null || :); [ "$actual" = "$1" ] || { printf 'expected %s real server-side applies, got %s\n' "$1" "$actual" >&2; exit 1; }; }
expect_state_file() { [ -e "$FAKE_STATE/$1" ] || { printf 'expected fake state marker %s\n' "$1" >&2; cat "$tmp/last-err" >&2; exit 1; }; }
test_case() { printf 'controller transaction test: %s\n' "$1"; }

# T1: happy deploy, then idempotent re-run.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase complete
[ -e "$FAKE_STATE/scaled1" ]
jq -e 'length == 10' "$transaction/owned-uids.json" >/dev/null
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
grep -Fq 'already complete' "$tmp/last-out"

# T2: crash at each journal boundary, then resume to completion.
for boundary in sealed anchor-intent anchor-journaled baselined apply-intent applied scale-intent scaled complete; do
  test_case "T2 crash boundary $boundary"
  reset_state; new_transaction
  run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER="$boundary" BLAZN_PHASE4C_DISPOSABLE_TEST=true
  [ "$last_code" -eq 86 ] || { printf 'boundary %s: expected 86, got %s\n' "$boundary" "$last_code" >&2; cat "$tmp/last-err" >&2; exit 1; }
  expect_phase "$boundary"
  run_tool install-controller.sh
  [ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
  expect_phase complete
done

# T2b: a crash after anchor create but before its authoritative UID journal is
# fail-closed; an indistinguishable same-shape replacement is never adopted.
reset_state; new_transaction
test_case 'T2b unknown anchor install refusal'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=anchor-created BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
expect_phase anchor-intent
[ ! -e "$transaction/anchor.json" ]
: >"$FAKE_STATE/user-anchor"
run_tool install-controller.sh
expect_code 1
expect_message 'anchor exists without an authoritative journaled UID'
expect_phase anchor-intent

# T2c: teardown also refuses to delete the unknown server UID, leaving the
# harmless inert residue for explicit manual recovery.
reset_state; new_transaction
test_case 'T2c unknown anchor teardown refusal'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=anchor-created BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
: >"$FAKE_STATE/user-anchor"
run_tool teardown-controller.sh
expect_code 1
expect_message 'anchor exists without an authoritative journaled UID'
expect_phase anchor-intent
[ ! -e "$FAKE_STATE/delete-requests.log" ] || { printf 'unknown anchor teardown issued a delete\n' >&2; exit 1; }

# T6aa: foreground anchor deletion may complete asynchronously. Teardown polls
# the exact anchor/dependent identities to bounded completion across that lag.
reset_state; new_transaction
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=apply-executed BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
: >"$FAKE_STATE/defer-anchor-delete"
run_tool teardown-controller.sh BLAZN_CONTROLLER_GC_ATTEMPTS=5
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 1 ]

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
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 11 ]
grep -Fq '"uid": "11111111-1111-4111-8111-111111111111"' "$FAKE_STATE/delete-requests.log"
grep -Fq '"uid": "77777777-7777-4777-8777-777777777777"' "$FAKE_STATE/delete-requests.log"
grep -Fq '"uid": "88888888-8888-4888-8888-888888888888"' "$FAKE_STATE/delete-requests.log"
grep -Fq '"uid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"' "$FAKE_STATE/delete-requests.log"
grep -Fq '"uid": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"' "$FAKE_STATE/delete-requests.log"

# T6a: rollback intent is durable before scale/delete; a crash at that journal
# resumes and completes instead of treating the absent/pending anchor as fatal.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
run_tool teardown-controller.sh BLAZN_PHASE4C_FAIL_AFTER=rollback-intent BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase rollback-intent
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete

# T6ab: a Deployment replacement between teardown validation and scale-down
# fails the UID/resourceVersion patch; bindings still revoke and replacement is untouched.
reset_state; new_transaction
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
: >"$FAKE_STATE/race-teardown-deployment"
run_tool teardown-controller.sh BLAZN_CONTROLLER_GC_ATTEMPTS=2
[ "$last_code" -eq 1 ]
expect_phase rollback-intent
expect_message 'ambiguous replacement objects were left untouched; recovery is required'
[ -e "$FAKE_STATE/deleted-clusterrolebinding" ]
[ -e "$FAKE_STATE/deleted-rolebinding" ]
[ ! -e "$FAKE_STATE/scaled0" ]
[ ! -e "$FAKE_STATE/deleted-deployment" ]

# T6ac: a DELETE failure followed by an API discovery failure is never
# interpreted as confirmed absence or rollback completion.
reset_state; new_transaction
test_case 'T6ac post-delete discovery error remains recovery-required'
run_tool install-controller.sh
expect_code 0
: >"$FAKE_STATE/fail-delete-service"
: >"$FAKE_STATE/arm-get-failure-after-delete-service"
run_tool teardown-controller.sh BLAZN_CONTROLLER_GC_ATTEMPTS=2
expect_code 1
expect_phase rollback-intent
expect_message 'recovery is required'
[ -e "$FAKE_STATE/deleted-clusterrolebinding" ]
[ -e "$FAKE_STATE/deleted-rolebinding" ]

# T6ad: even after a successful UID-fenced DELETE, a GC observation error does
# not prove absence and cannot advance the journal to rollback-complete.
reset_state; new_transaction
test_case 'T6ad GC discovery error blocks rollback completion'
run_tool install-controller.sh
expect_code 0
: >"$FAKE_STATE/arm-get-failure-after-delete-service"
run_tool teardown-controller.sh BLAZN_CONTROLLER_GC_ATTEMPTS=2
expect_code 1
expect_phase rollback-intent
expect_message 'recovery is required'

# T6ae: an initial Deployment discovery error neither authorizes a blind scale
# nor prevents binding-first revocation, but it keeps recovery incomplete.
reset_state; new_transaction
test_case 'T6ae initial Deployment discovery error is fail-closed'
run_tool install-controller.sh
expect_code 0
: >"$FAKE_STATE/fail-get-deployment"
run_tool teardown-controller.sh BLAZN_CONTROLLER_GC_ATTEMPTS=2
expect_code 1
expect_phase rollback-intent
expect_message 'recovery is required'
[ -e "$FAKE_STATE/deleted-clusterrolebinding" ]
[ -e "$FAKE_STATE/deleted-rolebinding" ]

# T5b: a transaction stranded at 'scaled' can still be torn down (owned UIDs
# were recorded at apply-intent, before scaling).
reset_state; new_transaction
test_case 'T5b scaled unavailable transaction teardown'
run_tool install-controller.sh FAKE_UNAVAILABLE=1 BLAZN_CONTROLLER_AVAILABLE_ATTEMPTS=2
expect_code 1
expect_phase scaled
[ -f "$transaction/owned-uids.json" ]
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
expect_delete_count 11

# T5c: a crash after the fenced scale succeeds but before the scaled journal
# resumes from durable scale-intent and accepts the exact replicas=1 object.
reset_state; new_transaction
test_case 'T5c scale-executed crash resume'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=scale-executed BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
expect_phase scale-intent
[ -e "$FAKE_STATE/scaled1" ]
run_tool install-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase complete

# T5ca: transactions created before scale-intent existed may still be in
# applied with an already-scaled exact Deployment; migrate them safely.
reset_state; new_transaction
test_case 'T5ca legacy applied-plus-scaled resume'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=applied BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
expect_phase applied
: >"$FAKE_STATE/scaled1"
run_tool install-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase complete

# T5d: a resumed object whose user-controlled semantics drifted from the exact
# anchored manifest is rejected before any scale-up.
reset_state; new_transaction
test_case 'T5d semantic drift rejection'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=applied BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
expect_phase applied
run_tool install-controller.sh FAKE_SEMANTIC_DRIFT=role
[ "$last_code" -eq 1 ]
expect_message 'controller object semantics differ from sealed manifest: role'
expect_phase applied
[ ! -e "$FAKE_STATE/scaled1" ]

# T5e: an admission mutation present symmetrically in server dry-run and real
# apply is rejected against independent client/sealed intent before any apply.
reset_state; new_transaction
test_case 'T5e symmetric admission mutation rejection'
run_tool install-controller.sh FAKE_ADMISSION_MUTATION=role
expect_code 1
expect_message 'server-defaulted baseline contains an unapproved admission mutation: role'
expect_phase anchor-journaled
[ ! -e "$FAKE_STATE/serviceaccount" ]

# T5ee: reviewed explicit fields are never normalized away; a symmetric
# admission rewrite of such intent is rejected during baseline capture.
reset_state; new_transaction
test_case 'T5ee explicit intent mutation rejection'
run_tool install-controller.sh FAKE_EXPLICIT_MUTATION=deployment
expect_code 1
expect_message 'server-defaulted baseline contains an unapproved admission mutation: deployment'
expect_phase anchor-journaled
[ ! -e "$FAKE_STATE/serviceaccount" ]

# T5f: replacing the Deployment after exact validation but at the atomic patch
# causes the UID/resourceVersion tests to fail; the replacement is never run.
reset_state; new_transaction
test_case 'T5f atomic scale replacement race'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=applied BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
: >"$FAKE_STATE/race-deployment"
run_tool install-controller.sh
case "$last_code" in 1|22) ;; *) printf 'expected scale race exit 1 or 22, got %s\n' "$last_code" >&2; cat "$tmp/last-err" >&2; exit 1 ;; esac
expect_phase scale-intent
[ ! -e "$FAKE_STATE/scaled1" ]

# T5g: a replacement after the atomic scale is rejected by exact UID/baseline
# validation and cannot make the transaction complete by reporting Available.
reset_state; new_transaction
test_case 'T5g post-scale replacement rejection'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=scaled BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
: >"$FAKE_STATE/user-deployment"
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_phase scaled

# T6b: a teardown that has already removed some objects resumes cleanly
# instead of aborting on a 404 for the already-deleted ones.
reset_state; new_transaction
test_case 'T6b partial teardown resume'
run_tool install-controller.sh
expect_code 0
# Pre-mark the Deployment and egress policy as already deleted, then tear down.
: >"$FAKE_STATE/deleted-deployment"; : >"$FAKE_STATE/deleted-egress"; rm -f "$FAKE_STATE/scaled1"; : >"$FAKE_STATE/scaled0"
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
# Nine still-present owned objects, including the anchor, were deleted.
expect_delete_count 9

# T6c: a resume that already removed only the same-named Role (its sibling
# ServiceAccount/RoleBinding/Deployment still present) skips the Role by kind,
# proving the absence pre-check is kind-scoped, not name-scoped.
reset_state; new_transaction
test_case 'T6c kind-scoped absence resume'
run_tool install-controller.sh
expect_code 0
: >"$FAKE_STATE/deleted-role"; rm -f "$FAKE_STATE/scaled1"; : >"$FAKE_STATE/scaled0"
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
expect_delete_count 10

# T7: a pre-existing controller Deployment blocks a fresh transaction.
reset_state; : >"$FAKE_STATE/deployment"; new_transaction
test_case 'T7 pre-existing Deployment refusal'
run_tool install-controller.sh
expect_code 1
expect_message 'already exists before transaction'

# T7b: cluster-scoped names are preflighted too; a user-owned global object is
# never adopted, applied over, inventoried, or deleted.
reset_state; : >"$FAKE_STATE/clusterrole"; : >"$FAKE_STATE/user-clusterrole"; new_transaction
test_case 'T7b pre-existing cluster RBAC refusal'
run_tool install-controller.sh
expect_code 1
expect_message 'clusterrole/blazn-sandbox-controller-node-observer'
if grep -Fq 'apply --server-side' "$FAKE_STATE/calls.log"; then printf 'pre-existing ClusterRole must block apply\n' >&2; exit 1; fi

# T7c: a crash after apply but before UID capture never reconstructs ownership
# or reapplies dependents. Install reports recovery-required.
reset_state; new_transaction
test_case 'T7c unjournaled apply refusal'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=apply-executed BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
expect_phase apply-intent
expect_journal_length 0
run_tool install-controller.sh
expect_code 1
expect_message 'dependent controller object exists without a completed UID journal; recovery is required'
expect_real_apply_count 1

# T7d: teardown from the same crash window foreground-deletes only the inert
# anchor; owner-reference GC removes its unjournaled dependents.
reset_state; new_transaction
test_case 'T7d unjournaled apply anchor-GC teardown'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=apply-executed BLAZN_PHASE4C_DISPOSABLE_TEST=true
expect_code 86
run_tool teardown-controller.sh
expect_code 0
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 1 ] || { printf 'apply-executed recovery did not delete only the authoritative anchor\n' >&2; exit 1; }
grep -Fq 'cccccccc-cccc-4ccc-8ccc-cccccccccccc' "$FAKE_STATE/delete-requests.log"

# T7e: if an object is replaced after apply but before UID capture, neither
# recovery path adopts it. Anchor GC removes actual dependents, leaves the
# same-annotation replacement without the anchor owner reference, and reports.
reset_state; new_transaction
test_case 'T7e replacement during unjournaled apply teardown'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=apply-executed BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase apply-intent
: >"$FAKE_STATE/user-serviceaccount"
run_tool teardown-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'ambiguous replacement objects were left untouched; recovery is required'
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 1 ]
[ ! -e "$FAKE_STATE/deleted-serviceaccount" ]
[ -e "$FAKE_STATE/deleted-anchor" ]
expect_phase rollback-intent
# A separate install-resume from the same crash shape also refuses adoption
# before issuing a second apply.
reset_state; new_transaction
test_case 'T7e install resume refuses replacement adoption'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=apply-executed BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
: >"$FAKE_STATE/user-serviceaccount"
run_tool install-controller.sh
expect_code 1
expect_message 'dependent controller object exists without a completed UID journal; recovery is required: serviceaccount/blazn-sandbox-controller'
expect_real_apply_count 1
[ ! -e "$FAKE_STATE/delete-requests.log" ]

# T7f: a partial apply is recoverable through anchor GC without reconstructing
# or adopting dependent UIDs.
reset_state; new_transaction
test_case 'T7f partial apply anchor-GC recovery'
run_tool install-controller.sh FAKE_PARTIAL_APPLY=1
[ "$last_code" -eq 1 ]
expect_phase apply-intent
expect_journal_length 8
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 9 ]

# T7g: a crash after the first durable dependent journal deletes that exact UID
# and then foreground-deletes the anchor. Missing later journal keys are safe.
reset_state; new_transaction
test_case 'T7g first journal crash recovery'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=journal-serviceaccount BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase apply-intent
jq -e 'length == 1 and has("serviceaccount/blazn-sandbox-controller")' "$transaction/owned-uids.json" >/dev/null
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 2 ]

# T7h: a mid-sequence crash after apply but before that response is journaled
# deletes the four earlier exact UIDs; anchor GC removes the unjournaled Service.
reset_state; new_transaction
test_case 'T7h mid-apply crash recovery'
run_tool install-controller.sh BLAZN_PHASE4C_FAIL_AFTER=apply-executed-service BLAZN_PHASE4C_DISPOSABLE_TEST=true
[ "$last_code" -eq 86 ]
expect_phase apply-intent
jq -e 'length == 4 and (has("service/blazn-sandbox-access") | not)' "$transaction/owned-uids.json" >/dev/null
run_tool teardown-controller.sh
[ "$last_code" -eq 0 ] || { cat "$tmp/last-err" >&2; exit 1; }
expect_phase rollback-complete
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 5 ]

# T7i: a missing journaled anchor does not block independent binding-first UID
# cleanup. The transaction remains recovery-required and is not completed.
reset_state; new_transaction
test_case 'T7i missing anchor independent cleanup'
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
: >"$FAKE_STATE/deleted-anchor"
run_tool teardown-controller.sh
[ "$last_code" -eq 1 ]
expect_phase rollback-intent
expect_message 'ambiguous replacement objects were left untouched; recovery is required'
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 10 ]
grep -Fq 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb' "$FAKE_STATE/delete-requests.log"
grep -Fq '33333333-3333-4333-8333-333333333333' "$FAKE_STATE/delete-requests.log"

# T7j: a same-name replacement of the anchor is likewise untouched while all
# journaled dependent UIDs are cleaned.
reset_state; new_transaction
test_case 'T7j replacement anchor independent cleanup'
run_tool install-controller.sh
[ "$last_code" -eq 0 ]
: >"$FAKE_STATE/user-anchor"
run_tool teardown-controller.sh
[ "$last_code" -eq 1 ]
expect_phase rollback-intent
[ "$(grep -c 'preconditions' "$FAKE_STATE/delete-requests.log")" -eq 10 ]
[ ! -e "$FAKE_STATE/deleted-anchor" ]

# T8: path traversal outside the reviewed transaction root is rejected.
reset_state; transaction=$tx_root/controller-x/../$tx_prefix-evil
run_tool install-controller.sh
[ "$last_code" -eq 1 ]
expect_message 'one clean segment'

printf 'Phase 5 controller deployment transaction proofs passed\n'
