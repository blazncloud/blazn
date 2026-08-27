# Phase 5 Agent Sandbox installation

The production installation of the upstream Agent Sandbox v0.5.6 controller
into `agent-sandbox-system`, prepared for the Phase 5 boundary rather than
the Phase 4C canary.

`render-install-phase5.sh` renders three sealed inputs, each annotated with
the transaction identity:

- `install.yaml` through the reviewed `phase4c/render-install.sh`: the four
  CRDs, the digest-pinned controller with the restricted profile, no
  upstream cluster RBAC, no extensions, externally bootstrapped webhook TLS.
- `production-rbac.yaml`: the read-only observer ClusterRole/Binding and the
  leader-election Lease Role in the controller namespace. Mutation RBAC in
  `blazn-poc-sandboxes` is already owned by the boundary transaction.
- `bootstrap.yaml` through the reviewed `phase4c/bootstrap.yaml.in`: the
  webhook TLS Secret plus the single-use, UID-deleted CA-patch Job — with a
  production-lived certificate (`BLAZN_WEBHOOK_CERT_DAYS`, default 397)
  instead of the canary's two-day certificate.

`install-phase5.sh` is the journaled, crash-resumable, UID-fenced
transaction (`sealed → install-intent → install-applied → bootstrap-intent →
bootstrap-applied → bootstrap-complete → complete`). It requires the Phase 5
boundary to be installed and the Kueue Pod integration to be live, refuses
pre-existing namespaces/CRDs it does not own, verifies every sealed digest
on each resume, verifies the four CRDs trust the sealed CA, removes the
bootstrap privilege by recorded UID before completion, and records every
owned UID for rollback. `rollback-phase5.sh` refuses while any Sandbox
exists and deletes only the recorded identities before proving absence.

`../test-phase5-install-disposable.sh` rehearses the full target on a
disposable kind cluster: the patched pinned Kueue chart with the Pod
integration, queue fixtures, the boundary, a refused early install, a
mid-flight crash and resume of the real transaction, a real impersonated
adapter-shaped Sandbox that runs on the eligible node with its Kueue
Workload admitted at exactly the requested cpu/memory/ephemeral-storage,
full teardown, and rollback to zero residue.
