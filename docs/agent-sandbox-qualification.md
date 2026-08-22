# Agent Sandbox qualification evidence

## Disposable spike result

Agent Sandbox v0.5.6 passed a clean-room compatibility spike with Kueue v0.19.2
on Kubernetes v1.36.1 in a single-node kind cluster on ben4. The existing ben4
MicroK8s installation was not used or changed.

- All four Agent Sandbox v1beta1 CRDs and eleven Kueue CRDs established.
- Checksum-verified upstream manifests were rewritten from their single expected
  version tags to the locked controller digests, and both running Pod image IDs
  matched those digests.
- Both controllers became Available; Agent Sandbox webhook and leader election
  started without errors.
- A direct synthetic Sandbox reached `Ready=True` with reason
  `DependenciesReady`.
- Its generated Pod carried the mandatory LocalQueue label and Kueue created an
  admitted Workload reserving exactly 100m CPU and 64Mi memory; the Pod phase
  was explicitly Running.
- Deleting the Sandbox removed its Pod and Kueue Workload despite Kueue's
  `resource-in-use` finalizer.
- Uninstall left zero matching Agent Sandbox/Kueue CRDs, cluster RBAC, or
  webhooks. Kueue checks also cover visibility APIServices and the fixture
  ClusterQueue/ResourceFlavor. Deleting the uniquely owned kind cluster left
  zero matching Docker containers, networks, or volumes.

## Live blockers

1. Upstream Pod-delete RBAC is cluster-wide and requires a reviewed live
   boundary before installation.
2. No hardened RuntimeClass has been qualified; this is orchestration isolation
   only.
3. Exact shared-cluster Kueue version/API skew must be inventoried during the
   separately approved live change.
4. SandboxTemplate, SandboxClaim, and SandboxWarmPool lifecycle belongs to
   Phase 5; Phase 4A proves only CRD/controller startup and direct Sandbox use.

## Narrow next PRs

1. Phase 4B: the namespace-scoped adapter now has direct Sandbox CRUD/watch,
   mandatory queue injection, a fail-closed RuntimeClass gate, receipted
   lifecycle cleanup, and artifact hooks. Live use remains blocked on Phase 4C.
2. Phase 4C: serialized shared-cluster CRD/controller/RBAC installation, one
   synthetic canary, admission/orphan evidence, and exact rollback comparison.

Phase 4C live-canary preparation is now specified in
[`infra/agent-sandbox/phase4c/README.md`](../infra/agent-sandbox/phase4c/README.md).
It is intentionally non-mutating until a separately approved serialized live
window. The upstream v0.5.6 manager has no namespace-watch flag, so the reviewed
boundary scopes all writes (rather than all reads), globally denies Sandbox
admission outside `blazn-poc`, and refuses installation when any Sandbox already
exists.

The executable source of truth is
[`infra/agent-sandbox/test-disposable.sh`](../infra/agent-sandbox/test-disposable.sh).
