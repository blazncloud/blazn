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

The render accepts only `repository@sha256:<64 lowercase hex>` and rejects a
tag in the image name. Image publication, registry access, signature or
provenance verification, and live image-pull proof remain separate gates.

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
./infra/agent-sandbox/phase5-controller/render-install.sh ./controller-install.yaml
```

Both network destinations must be exact, usable IPv4 `/32` values; broad
CIDRs, wildcard ports, absent values, and mutable image tags fail closed. The
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
and Kueue Workload access is list-only there. The sole cluster-scoped grant is
`get` on RuntimeClasses. There is no Secret, Node, CRD, webhook, namespace, or
wildcard authority.

The database URL Secret is projected read-only for kubelet. A same-UID init
binary copies its value without logging it into a memory-backed private
`emptyDir` as a UID-65532, mode-0600, single-link regular file. That exact file
shape is required by the controller's strict reader. The main container sees
the private volume read-only. Both containers use a read-only root filesystem,
non-root identity, RuntimeDefault seccomp, drop all capabilities, deny privilege
escalation, and fixed CPU/memory requests and limits. Host namespaces and host
mounts are absent. Default-deny NetworkPolicies admit only the rendered API,
PostgreSQL, and optional resolver endpoints.

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
