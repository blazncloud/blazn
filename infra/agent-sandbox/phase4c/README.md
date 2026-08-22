# Agent Sandbox Phase 4C live-canary preparation

This directory prepares, but does not perform, the first shared-cluster Agent
Sandbox canary. Nothing here has been applied to shared MicroK8s or ben1. A
separately approved operator must execute the live run under the authoritative
root-owned lock.

## Reviewed boundary

Agent Sandbox v0.5.6 has no namespace-watch option. Its manager needs
cluster-scoped `get/list/watch` for Sandboxes and the resources it observes.
Phase 4C therefore makes the narrower claim that **all controller mutations are
namespace-scoped**:

- `render-install.sh` verifies the upstream checksum, removes all six upstream
  RBAC documents, removes `--extensions`, pins the controller image digest,
  fixes leader election to `agent-sandbox-system`, and enables tracking-label
  Pod/Service caches.
- `controller-boundary.yaml` grants create/update/delete only through Roles in
  `blazn-poc` (plus webhook certificate and Lease access in the controller
  namespace). The only cluster mutation is an exact four-CRD CA-bundle update.
- A fail-closed ValidatingAdmissionPolicy denies Sandbox creation or update
  outside `blazn-poc`, without the fixed LocalQueue and tokenless service
  account, or without the runtime trust declaration.
- `inventory.sh` blocks installation if any Sandbox or Phase 4C-owned object
  already exists. This is required because the upstream informer reads are not
  namespace-scoped. Its read-only access remains a known upstream limitation.

The LocalQueue targets an already reviewed, Active ClusterQueue. This change
does not create or edit ResourceFlavor, ClusterQueue, Kueue controller, Kueue
CRDs, or shared quota.

## Runtime gate

`render-fixtures.sh` accepts a RuntimeClass only when its live handler equals
the reviewed expected handler. If no hardened RuntimeClass is qualified, it
requires the exact acknowledgement
`BLAZN_ORCHESTRATION_ONLY_ACK=approved-non-sensitive-phase4c-canary`, emits no
RuntimeClass, and labels the workload `orchestration-only`. That exception is
only for the pinned synthetic, non-sensitive canary; it does not approve
untrusted or cross-tenant work.

## Serialized live runbook

1. Outside the change window, run only `inventory.sh NEW_EVIDENCE_DIRECTORY`.
   Review the exact kube context, kube-system UID, Kubernetes/Kueue versions,
   existing ClusterQueue, CRDs, admission objects, and RuntimeClasses. Do not
   reuse or overwrite an evidence directory.
2. Render the checksum-locked controller bundle and fixtures. Review their
   hashes and use `kubectl diff` only after confirming whether admission-side
   dry-run is acceptable for the target. Do not run `kubectl apply` in prep.
3. Obtain the separate Phase 4C live-change approval. Set the exact reviewed
   context and kube-system UID, plus
   `BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary`.
4. On the control-plane host, execute `canary.sh` only through
   `sudo phase4c/with-live-lock.sh .../phase4c/canary.sh INSTALL FIXTURES EVIDENCE`.
   The launcher holds `/run/lock/blazn/live-cluster-mutation.lock` on inherited
   FD 9 for the complete foreground operation and atomically increments the
   root-owned fencing counter. Scripts reject a forged FD, unsafe metadata,
   stale token, context drift, or cluster-UID drift.
5. Review controller RBAC evidence, the denied outside-namespace server dry
   run, Ready/Running state, Kueue admission, exact 100m/64Mi reservation, and
   canary deletion. Stop on any mismatch; do not broaden RBAC or fall back to an
   unmanaged Pod.
6. In the same serialized window, run
   `rollback.sh INSTALL PREINSTALL_INVENTORY CANARY_EVIDENCE` through a fresh
   lock token.
   It stops the controller, removes the uniquely owned namespace and Phase 4C
   objects, proves every exact target absent, and byte-compares normalized
   CRD/admission/RuntimeClass/Kueue inventories to the preinstall snapshot.

An uncertain holder, stale lock, unexpected namespace content, preexisting
Sandbox, finalizer, or rollback difference requires reconciliation and human
review. Never force-delete, remove finalizers, reuse an old inventory, or take
a broader cleanup action.
