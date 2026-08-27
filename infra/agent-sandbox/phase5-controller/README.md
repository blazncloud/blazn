# Phase 5 Sandbox controller deployment boundary

This directory prepares a suspended deployment for the Blazn Sandbox
controller. It does not create namespaces, admission policy, Secrets, images,
or any live resource. `render-install.sh` only writes a new local manifest; it
contains no Kubernetes client and never applies its output.

## Image contract

`Dockerfile.sandbox-controller` cross-compiles the controller and its narrow
database-URL initializer for `linux/amd64` and `linux/arm64`. The build stage is
pinned to the multi-platform Go image digest. The final `scratch` image has no
shell or package manager, runs as numeric UID/GID 65532, and contains only the
two static binaries and the public CA bundle needed by PostgreSQL TLS.

The render accepts only an explicit
`registry/repository@sha256:<64 lowercase hex>` authority and rejects implicit
registries and tags in the image name. A portless registry must be a strict DNS
name (or `localhost`); a registry with a port uses the same DNS validation and
a canonical port in the range 1-65535. Image publication, registry access,
signature or provenance verification, and live image-pull proof remain separate
gates.

## Render contract

Render into a path that does not exist:

```sh
BLAZN_CONTROLLER_IMAGE='registry.example/blazn/sandbox-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
BLAZN_DATABASE_URL_SECRET_NAME='blazn-sandbox-controller-database-url' \
BLAZN_DATABASE_URL_SECRET_KEY='database-url' \
BLAZN_DATABASE_ENDPOINT_KIND='ip' \
BLAZN_KUBERNETES_API_CIDR='10.0.0.10/32' \
BLAZN_KUBERNETES_API_PORT='16443' \
BLAZN_KUBERNETES_API_AUDIENCE='https://kubernetes.default.svc' \
BLAZN_BEN1_POSTGRES_CIDR='10.0.0.11/32' \
BLAZN_BEN1_POSTGRES_PORT='5432' \
BLAZN_OBJECT_SECRET_NAME='blazn-sandbox-controller-object-credentials' \
BLAZN_OBJECT_ACCESS_KEY='access-key' \
BLAZN_OBJECT_SECRET_KEY='secret-key' \
BLAZN_OBJECT_CA_KEY='object-ca' \
BLAZN_REGISTRY_PULL_SECRET_NAME='blazn-registry-pull' \
BLAZN_OBJECT_ENDPOINT_CIDR='10.0.0.12/32' \
BLAZN_OBJECT_ENDPOINT_PORT='9443' \
BLAZN_OBJECT_REGION='us-test-1' \
BLAZN_OBJECT_BUCKET='blazn-artifacts' \
BLAZN_SOURCE_HOST='github.com' \
BLAZN_SOURCE_CIDR='140.82.112.4/32' \
BLAZN_SOURCE_DNS_CIDR='10.0.0.53/32' \
./infra/agent-sandbox/phase5-controller/render-install.sh ./controller-install.yaml
```

Every network destination must be an exact, usable IPv4 `/32`; the source
boundary accepts a comma-separated, duplicate-free set of at most 64 exact
addresses for a reviewed rotating frontend. Broad CIDRs, wildcard ports,
absent values, and mutable image tags fail closed. The
API host passed to the process is derived from the exact API `/32`, so it does
not need DNS. The database URL in the pre-existing Secret must name the exact
ben1 IP and port above when `BLAZN_DATABASE_ENDPOINT_KIND=ip`.

If the reviewed database URL instead uses a hostname, set
`BLAZN_DATABASE_ENDPOINT_KIND=hostname` and provide the exact resolver as
`BLAZN_DNS_CIDR=<resolver-ip>/32`. Only that mode emits TCP/UDP 53 egress. A DNS
CIDR is rejected in `ip` mode. The renderer cannot inspect or create the Secret
and intentionally never prints its contents.

## Runtime boundary

The output assumes the separately owned namespaces `blazn-poc-system` and
`blazn-poc-sandboxes` already exist. It creates a tokenless ServiceAccount and
uses only a 600-second projected API token with an explicit audience. Its Role
can create/delete/patch and read Sandboxes only in `blazn-poc-sandboxes`; Pod
access is get/list, Kueue Workload access is list-only, and `pods/exec` has only
the create/get connect verbs required by the WebSocket v5 handshake,
and NetworkPolicy access is create/delete/get/list there. The controller accepts
only its pinned helper command, verifies the exact Pod and Sandbox UIDs before
and after each WebSocket v5 exchange, creates an exact temporary DNS/HTTPS
source policy, and deletes it with UID/resource-version preconditions before
releasing the init gate. It has no separate Sandbox status
subresource grant, ClusterRole, or other cluster-scoped authority and no Secret,
Node, RuntimeClass, CRD, webhook, namespace, or wildcard authority. RuntimeClass
access may be added only in a separate PR that wires and qualifies an exact
runtime capability.

The database URL Secret, separately owned object credential Secret, and public Kubernetes CA are projected read-only for
kubelet. A same-UID init binary copies the normalized database URL and exact,
bounded CA contents plus the two normalized object credential values without logging them into a memory-backed private
`emptyDir` as UID-65532, mode-0600, single-link regular files. Those exact file
shapes are required by the controller's strict readers. The main container sees
the private volume read-only. Both containers use a read-only root filesystem,
non-root identity, RuntimeDefault seccomp, drop all capabilities, deny privilege
escalation, and fixed CPU/memory requests and limits. Host namespaces and host
mounts are absent. Default-deny NetworkPolicies admit only the rendered API,
PostgreSQL, object-store, and optional resolver endpoints.

## Suspension and live blockers

The Deployment is intentionally rendered with `replicas: 0`. Rendering or
reviewing this bundle does not authorize image publication, namespace changes,
Secret creation, admission changes, scaling, or a live database/Kubernetes
connection.

Phase 4C is not compatible with this controller deployment: it admits only the
named synthetic `phase4c-canary` in namespace `blazn-poc`, while this controller
is hard-coded to manage `blazn-poc-sandboxes`. Before any scale-up, a later
reviewed change must install and qualify the target namespaces, matching
fail-closed admission policy, Agent Sandbox/Kueue controllers and CRDs, runner
identity, queue, and any RuntimeClass capability. Image publication and a
separate live-change approval must also be complete. Never work around that
incompatibility by broadening this RBAC or changing the controller namespace.

Run `make test-phase5-controller-deployment-static` for the non-mutating render
audit and `make test-phase5-controller-secret-init` for the initializer unit
tests. A real multi-platform image build, OCI inspection, publication, and
digest capture remain deferred to an eligible registry lane with Docker
Buildx; none is performed by this static preparation.

## Deployment (Phase 5)

The image, the rendered manifest, and the object Secret move in lockstep:
secret-init takes exactly the argument pairs its manifest passes (an old
image rejects a new manifest's ten arguments and vice versa, crash-looping
the init container), and the projected `BLAZN_OBJECT_CA_KEY` item requires
that key to exist in the object Secret (a missing key blocks the volume
mount). Always deploy in this order: re-provision the Secrets, then install
a manifest rendered against the image digest it names.

Once the boundary and the Agent Sandbox controller are installed and the
controller Secrets exist, `provision-controller-secrets.sh` creates the
`blazn-controller-database-url` and object-credential Secrets in
`blazn-poc-system` from root-only files, rewriting the database URL authority
to the reviewed reachable endpoint and never printing any secret value. The
controller database URL must authenticate as `blazn_sandbox_controller` (a
capability role that needs a login credential provisioned on the control-plane
database).

`provision-registry-pull-secret.sh` copies the separately owned Docker config
Secret into only `blazn-poc-system` and `blazn-poc-sandboxes`, without writing
its bytes to disk or stdout. The install transaction verifies that the pull
Secret exists in both namespaces before applying or scaling the controller.

`install-controller.sh` is a journaled, crash-resumable, UID-fenced
transaction (`sealed → apply-intent → applied → scaled → complete`): it
requires the boundary, the Agent Sandbox controller, and both Secrets, seals
the rendered controller manifest, verifies the reviewed digest and that it
starts at zero replicas, applies it, scales it to one, waits for
Availability, and records every owned UID. `teardown-controller.sh` scales to
zero, drains the Pods, deletes only the recorded controller identities (never
the shared namespaces or Secrets) by UID precondition, and proves absence.

`../test-phase5-controller-transaction.sh` proves crash-resume at every
journal boundary, fail-closed prerequisites (missing Secret, missing upstream
controller), the zero-replicas gate, the Available gate, exact
UID-preconditioned teardown, pre-existing-Deployment refusal, and path-root
confinement, against a fake kubectl state machine. The live reconcile of a
`blazn sandbox create` into a running Pod is proven against the control-plane
database at deploy time.
