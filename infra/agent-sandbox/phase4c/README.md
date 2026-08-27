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
  fixes leader election to `agent-sandbox-system`, enables tracking-label
  Pod/Service caches, consumes externally bootstrapped webhook TLS, and adds a
  non-root/read-only/drop-all/seccomp/resource-bounded controller profile.
- The rendered `controller-boundary.yaml` grants create/update/delete only
  through Roles in `blazn-poc`, plus the exact leader Lease in the controller
  namespace. The long-lived controller cannot patch CRDs, webhooks, or Secrets.
- A pinned, restricted, one-shot kubectl Job receives exact `get/patch` access
  to the four named CRDs, installs the sealed CA bundle, and loses its Job,
  ClusterRole, and binding through UID-precondition deletes before readiness.
- A fail-closed ValidatingAdmissionPolicy denies Sandbox creation or update
  through either served v1alpha1 or v1beta1 API outside `blazn-poc`, without the fixed LocalQueue and tokenless service
  account, complete restricted PodSpec, reviewed transaction identity, exact
  authenticated creator, closed scheduling/resources, or runtime trust declaration. Updates are accepted
  only from the controller identity and cannot change spec, labels, or
  annotations. Both owned namespaces enforce the Restricted PSA profile.
- `inventory.sh` blocks installation if any Sandbox or Phase 4C-owned object
  already exists. This is required because the upstream informer reads are not
  namespace-scoped. Its read-only access remains a known upstream limitation.

The LocalQueue targets an already reviewed, Active ClusterQueue. Fixture
rendering selects only a live served `v1beta2` or `v1beta1` LocalQueue API,
preferring `v1beta2`; every other Kueue API surface fails closed. This change
does not create or edit ResourceFlavor, ClusterQueue, Kueue controller, Kueue
CRDs, or shared quota.

Kueue v0.14.x does not enable the Plain Pod integration by default. The
reviewed `upgrade-kueue-pod-integration.sh` transaction pins and copies into a
root-only sealed directory the exact v0.14.3
chart bytes, current Helm revision, rendered-manifest digest, manager-config
digest, and admitted Workload identities before enabling `pod` only for
`blazn-poc` and `blazn-poc-sandboxes` using `podOptions.namespaceSelector`.
The chart, sealed-config, patch, and reviewed prior-live-config digests are
pinned in `../versions.env`; `kueue-live-config-baseline.yaml` records the
reviewed live manager configuration, and executable tests prove the sealed
configuration is exactly that baseline plus the Pod integration.
A checksum-pinned patch to the sealed chart renders the same selector only into
the `mpod.kb.io` and `vpod.kb.io` Helm-managed webhook entries, so future Helm
operations retain the boundary while other framework selectors remain
unchanged. The same sealed patch pins the manager image to the reviewed
digest recorded as `LIVE_KUEUE_CONTROLLER_IMAGE`, keeping the pin
Helm-managed instead of relying on an out-of-band chart edit. A failed upgrade or post-check rolls
back to the exact prior Helm revision. The transaction is rooted under
`/var/lib/blazn/phase4c/kueue-pod-*`, journals `upgrade-intent` before Helm,
and resumes or rolls back after process or host interruption. It requires both
managed namespaces to be absent and compares every Workload UID before and
after the change, including pending Workloads. It must use the same serialized live
cluster lock and Phase 4C approval boundary as the canary.

## Runtime gate

`render-fixtures.sh` accepts a RuntimeClass only when its live handler equals
the reviewed expected handler. If no hardened RuntimeClass is qualified, it
requires the exact acknowledgement
`BLAZN_ORCHESTRATION_ONLY_ACK=approved-non-sensitive-phase4c-canary`, emits no
RuntimeClass, and labels the workload `orchestration-only`. That exception is
only for the pinned synthetic, non-sensitive canary. It is bound to the sealed
transaction ID and authenticated creator rather than trusting an object
annotation alone; it does not approve untrusted or cross-tenant work.

## Transaction and recovery boundary

`prepare-transaction.sh` accepts only the closed, fixed inventory and fixture
file sets. It copies them into a new root-owned `0700` directory, makes every
input `0400`, records every SHA-256, and emits one aggregate digest for separate
review. Canary and rollback accept only that directory and require the exact
reviewed digest. They never consume the caller's original paths.

Every mutation has an fsynced intent/completion journal phase. Resume verifies
the unique transaction annotation and captures or rechecks every owned UID.
Deletes use Kubernetes `DeleteOptions.preconditions.uid` through a private
root-only Unix-socket proxy; a same-name replacement is never deleted. The
namespace scan rejects unowned durable objects before rollback. Disposable
tests crash after controller installation and resume the persisted phase, then
crash again with the canary Ready and prove direct rollback removes that canary
while its controller can still clear finalizers. They also inspect the exact
UID-precondition request and exercise pre-mutation rollback.

The Ready canary gate records a normalized JSON attestation of the generated
Pod, including immutable image identity, runtime resources, ownership,
placement, scheduling defaults, service account, and complete security
contexts. Static mutation fixtures prove selector, resource, owner, and imageID
drift fail closed. Inventory seals the complete normalized cluster admission
configuration, and rollback waits for and rechecks every sealed target before
declaring zero residue; lookup failures other than explicit NotFound abort.

## Serialized live runbook

1. Outside the change window, run only `inventory.sh NEW_EVIDENCE_DIRECTORY`.
   Review the exact kube context, kube-system UID, Kubernetes/Kueue versions,
   existing ClusterQueue, CRDs, admission objects, and RuntimeClasses. Do not
   reuse or overwrite an evidence directory.
2. Export one new UUID as `BLAZN_PHASE4C_TRANSACTION_ID`, then render the
   checksum-locked controller bundle and fixtures with that same value. Review their
   hashes and use `kubectl diff` only after confirming whether admission-side
   dry-run is acceptable for the target. Do not run `kubectl apply` in prep.
3. Run `prepare-transaction.sh INSTALL FIXTURES INVENTORY TRANSACTION` as root.
   Review its aggregate digest out of band, then set that exact value as
   `BLAZN_REVIEWED_INPUT_DIGEST`. Never approve a recomputed value during the
   mutation window.
4. Obtain the separate Phase 4C live-change approval. Set the exact reviewed
   context and kube-system UID, plus
   `BLAZN_PHASE4C_CHANGE_APPROVED=approved-phase4c-live-canary`.
5. Before any foundation apply, and while `blazn-poc` and
   `blazn-poc-sandboxes` are still absent, enable the Kueue Pod integration:
   verify the pulled chart against `LIVE_KUEUE_CHART_SHA256`, export a fresh
   `BLAZN_KUEUE_TRANSACTION_DIR` under `/var/lib/blazn/phase4c/kueue-pod-*`
   plus the reviewed `BLAZN_EXPECTED_KUEUE_REVISION`,
   `BLAZN_EXPECTED_KUEUE_MANIFEST_SHA256`, `BLAZN_EXPECTED_KUEUE_CONFIG_SHA256`,
   and `BLAZN_EXPECTED_WORKLOADS`, then run
   `sudo phase4c/with-live-lock.sh env ... phase4c/upgrade-kueue-pod-integration.sh KUEUE_CHART_TGZ`.
   The canary's Kueue admission gate depends on this transaction being
   `complete`; the transaction refuses to run once either namespace exists.
6. On the control-plane host, execute `canary.sh TRANSACTION` only through
   `sudo phase4c/with-live-lock.sh .../phase4c/canary.sh TRANSACTION`.
   The launcher holds `/run/lock/blazn/live-cluster-mutation.lock` on inherited
   FD 9 for the complete foreground operation, binds authority to that FD's
   device/inode rather than its replaceable pathname, and atomically increments the
   root-owned fencing counter. Scripts reject a forged FD, unsafe metadata,
   stale token, context drift, or cluster-UID drift.
7. Review controller RBAC evidence, bootstrap privilege removal, the denied outside-namespace server dry
   run, Ready/Running state, Kueue admission, exact 100m/64Mi reservation, and
   canary deletion. Stop on any mismatch; do not broaden RBAC or fall back to an
   unmanaged Pod.
8. After interruption, rerun `canary.sh TRANSACTION` under a fresh lock token;
   it resumes the durable phase. To unwind, run `rollback.sh TRANSACTION` under
   a fresh lock token.
   It stops the controller, removes the uniquely owned namespace and Phase 4C
   objects with recorded UID preconditions, proves every exact target absent, and byte-compares normalized
   CRD/admission/RuntimeClass/Kueue inventories to the preinstall snapshot.

Rollback also supports a foundation apply that stopped before the Sandbox CRD
existed by checking CRD discovery before namespaced Sandbox lookup. An
uncertain holder, stale lock, unexpected namespace content, preexisting
Sandbox, finalizer, or rollback difference requires reconciliation and human
review. Never force-delete, remove finalizers, reuse an old inventory, or take
a broader cleanup action.
