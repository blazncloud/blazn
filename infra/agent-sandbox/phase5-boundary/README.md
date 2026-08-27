# Phase 5 boundary

This directory prepares, and proves in disposable clusters, the production
Sandbox foundation the Phase 5 controller deploys into. Nothing here is
applied to the shared cluster except through the sealed, journaled
`install-boundary.sh` transaction under the authoritative live-cluster lock.

The rendered boundary contains exactly eight documents:

- Namespaces `blazn-poc-system` and `blazn-poc-sandboxes`, both enforcing the
  Restricted Pod Security profile and carrying the transaction identity.
- The tokenless `blazn-sandbox-runner` ServiceAccount in
  `blazn-poc-sandboxes`.
- LocalQueue `blazn-poc` in `blazn-poc-sandboxes`, served through
  `kueue.x-k8s.io/v1beta1` and targeting the reviewed existing ClusterQueue.
- A namespace-scoped Role and RoleBinding granting the upstream
  `agent-sandbox-controller` ServiceAccount its mutations only inside
  `blazn-poc-sandboxes`. No ClusterRole or ClusterRoleBinding is added.
- The fail-closed `blazn-sandbox-boundary` ValidatingAdmissionPolicy and its
  Deny binding, scoped by namespace selector to `blazn-poc-sandboxes` so the
  Phase 4C canary policy in `blazn-poc` is untouched.

The admission policy admits exactly the Sandbox shape rendered by
`internal/sandboxcontrol/adapter.go`: canonical UUID names, the managed,
workspace, owner, and sandbox-id labels mirrored onto the podTemplate with
the `blazn-poc` queue label, the trust, expiry, and intent-digest
annotations, the tokenless runner identity, Never restart, no RuntimeClass,
exactly the sandbox-eligibility and architecture node selector, no host
namespaces or scheduling overrides, the reviewed 65532 pod security context,
emptyDir-only reviewed volume names, one digest-pinned `main` container with
a bounded argv and fully declared bounded resources, the two digest-pinned
`/blazn-sandbox-io` helpers, and no environment, args, ports, or probes.
Creation is restricted to
`system:serviceaccount:blazn-poc-system:blazn-sandbox-controller`; updates
are restricted to that identity and the upstream controller with spec,
labels, and Blazn annotations immutable. The upstream controller alone may
maintain its reserved `agents.x-k8s.io/pod-name` annotation.

For an in-place policy or RBAC update, render the complete successor boundary
with a new transaction UUID and run `upgrade-boundary.sh` under
`phase4c/with-live-lock.sh`, naming the completed prior transaction directory.
The upgrade verifies every prior UID, preserves namespaces and Secrets, and
records the successor before marking the prior journal superseded.

`good-sandbox.py` is the executable statement of that contract: it renders
the adapter-exact object plus twenty-two reviewed mutations, and
`test-phase5-boundary-disposable.sh` proves on a disposable kind cluster
that the good object is admitted while every mutation is denied by the rule
that owns it, plus a permitted-but-wrong creator identity, update fencing,
and status-subresource fencing through impersonated requests. `test-phase5-boundary-transaction.sh` proves the
install and rollback transactions resume from a crash at every journal
boundary, fail closed on discovery errors, refuse foreign objects and
occupied namespaces, and roll back only recorded UIDs through the private
UID-precondition proxy.

## Serialized live runbook

1. Export one new UUID as `BLAZN_PHASE5_TRANSACTION_ID` and render with
   `BLAZN_EXISTING_CLUSTER_QUEUE=m1-light render-boundary.sh OUTPUT`.
2. Review the rendered manifest and record its SHA-256 out of band as
   `BLAZN_EXPECTED_BOUNDARY_SHA256`.
3. With the Phase 4C approval and identity variables set, run
   `sudo phase4c/with-live-lock.sh env ... phase5-boundary/install-boundary.sh OUTPUT`
   with `BLAZN_PHASE5_TRANSACTION_DIR=/var/lib/blazn/phase5/boundary-<uuid>`.
   The transaction requires the Kueue Pod integration to already be complete,
   both namespaces absent, the ClusterQueue Active, and Kubernetes 1.30+.
4. After interruption, rerun under a fresh lock token; the durable phase
   resumes. To unwind, run `rollback-boundary.sh` under a fresh lock token;
   it refuses namespaces that still hold Pods or Sandboxes and deletes only
   the recorded UIDs before proving absence.
