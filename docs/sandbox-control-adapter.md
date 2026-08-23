# Sandbox control adapter contract

Phase 4B adds a narrow Kubernetes Agent Sandbox adapter. It talks directly to
`agents.x-k8s.io/v1beta1` and owns only `Sandbox` resources in
`blazn-poc-sandboxes`. It does not install CRDs, modify shared Kueue quota, use
administrator kubeconfig on behalf of callers, or fall back to unmanaged Pods.

## Fixed boundaries

- Every object and generated Pod is labeled with the Blazn sandbox, workspace,
  and owner identity. Reads, lists, watches, deletion, and finalization verify
  those labels again and present mismatches as outside the caller's boundary.
- Every Pod template carries `kueue.x-k8s.io/queue-name: blazn-poc`. A backend
  response that loses this label fails; the adapter never retries without it.
- Pods use `blazn-sandbox-runner`, disable service-account token automount, run
  non-root with RuntimeDefault seccomp, drop every Linux capability, forbid
  privilege escalation, and use a read-only root filesystem.
- Blazn's artifact cleanup finalizer remains until every required export has a
  content-addressed receipt at the deterministic
  `workspaces/<workspace>/sandboxes/<sandbox>/artifacts/<name>` key. Export
  failure or a mutated persisted export annotation leaves the finalizer in
  place. Durable orchestration state supplies the canonical requested export
  list and its SHA-256 as a delete/finalize precondition; mutable Kubernetes
  annotations are evidence to compare, never the source of authority.
- Delete uses exact UID and resourceVersion preconditions with foreground
  propagation. Finalizer removal uses the latest resourceVersion.

## Phase 5 backend admission slice

The first Phase 5 backend slice remains inside the adapter and is not a
controller deployment. Every material create input, including request and
tenant identity, pinned image, ordered command, architecture, RuntimeClass,
trust acknowledgement, CPU/memory/ephemeral-storage requests and limits,
expiry, and canonical artifact set, is bound into the internal
`sandboxes.blazn.dev/create-intent-digest` annotation. The persisted Sandbox
must preserve that digest and the exact rendered spec.

`EnsureCreated` performs a NotFound preflight and never adopts a pre-existing
same-name object without an exact UID precondition. It may resolve an ambiguous
POST once by reading the persisted object, but accepts it only when the object
has a concrete UID and resourceVersion and its full material spec and intent
digest match. A retry with a known UID refuses to create a replacement if that
UID is absent.

Sandbox UID and resourceVersion evidence must match the receipt's frozen
object-identity grammar, not merely be non-empty. If an authoritative create
response carries malformed identity, the adapter refuses both the receipt and
an unsafe compensating delete; exact valid UID/resourceVersion evidence is
required before cleanup can begin.

Admission observation requires exactly one admitted Kueue Workload and one Pod.
It verifies API versions, non-empty UIDs and resourceVersions, the Workload's
single controller owner as that exact Pod, the Pod's single controller owner as
that exact Sandbox UID, the complete tokenless Pod spec, fixed queue, and
workspace/owner/sandbox identity. Re-observation can be fenced by the complete
prior observation and rejects UID, resourceVersion, or API drift. Cleanup
absence scans namespace Pod and Workload collections without trusting mutable
labels and rejects the frozen identities or exact controller-owner orphans.

This slice does not wire `cmd/controller`, install or change Kubernetes
resources, publish an image, or claim Gate 4C or Gate 5. Those remain subsequent
stacked work and live qualification.

## Runtime trust

An untrusted workload requires a named RuntimeClass whose local capability is
qualified, hardened, and architecture-compatible. Creation also reads the live
`node.k8s.io/v1` RuntimeClass and requires its handler to match that qualified
capability. Missing or changed runtime state fails closed. Without a hardened
runtime, only explicitly approved non-sensitive POC input may use the ordinary
runtime, and its status carries this warning:

> Agent Sandbox orchestration is active. Hardened runtime isolation is not
> qualified on this cluster; use only approved non-sensitive POC workloads.

## State, errors, and evidence

The adapter normalizes Sandbox conditions to `pending`, `queued`, `starting`,
`ready`, `failed`, `stopping`, or `deleted`. Machine-readable failures and HTTP
status mappings are frozen in
[`sandbox-control-adapter-receipt.schema.json`](../packages/contracts/sandbox-control-adapter-receipt.schema.json).
Create, delete, and finalize receipts bind request, namespace, Kubernetes UID
and resourceVersion, workspace, owner, queue, runtime, state, sorted artifact
receipts, timestamp, and SHA-256 digest.

Fake-API tests cover the exact request paths and bodies, queue/runtime stripping,
identity isolation, list/watch/status, optimistic deletion, artifact failure,
finalizer order, and receipt tampering. The disposable Phase 4A kind test adds a
temporary Blazn namespace and LocalQueue, drives the adapter through a
cluster-local `kubectl proxy`, proves Ready/Kueue admission/watch/delete/export
finalization, and verifies zero Sandbox, Pod, Workload, namespace, queue, and
Docker residue before destroying the uniquely owned cluster.
