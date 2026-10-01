# Design: confine a node's credential to its own Node (NodeRestriction)

Status: proposed, not implemented. Plan item M0.9.

## Problem

Every Blazn node is a MicroK8s worker of the shared Frontro cluster and holds a
kubelet credential (`system:node:<name>`, group `system:nodes`). The cluster's
API servers run with `--authorization-mode=Node,RBAC` but without the
`NodeRestriction` admission plugin (`--enable-admission-plugins=EventRateLimit`).

Without that plugin, a node credential can change any Node object, and the
`node-restriction.kubernetes.io/` label prefix is not protected. Measured on
2026-10-01 with server-side dry runs (`dryRun=All`, nothing persisted),
impersonating a node identity on the Frontro cluster:

| Request by a node credential | Result today |
|---|---|
| Add a label to another node (a control-plane host) | accepted |
| Add a `NoExecute` taint to another node | accepted |
| Cordon another node | accepted |
| Set another workspace's `node-restriction.kubernetes.io/blazn-workspace` label on itself | accepted |
| Add `blazn.dev/sandbox-eligible=true` to itself | accepted |
| Change its own taints | accepted |
| Delete another node | denied (RBAC) |
| Read another node, list other nodes' Pods, read Secrets it does not mount | denied (Node authorizer) |

So a registered machine, which is controlled by whoever registered it, can:

1. **Evict Frontro production workloads** by tainting or cordoning any host.
2. **Receive another workspace's sandboxes** by relabelling itself. Sandbox
   placement by workspace (M4.3) relies on a label the node can rewrite.
3. **Lift its own quarantine** by removing `blazn.dev/bootstrap` or
   `blazn.dev/sandbox-node` and marking itself eligible.

The Node authorizer already blocks reads of other nodes, which is why
`kubectl label` and `kubectl taint` fail for a node identity (they read first).
A direct `PATCH` does not.

## What NodeRestriction enforces (Kubernetes 1.35)

For requests from a node identity, on Node objects
(`plugin/pkg/admission/noderestriction/admission.go`, `admitNode`):

- only its own Node may be created or updated;
- on update, `spec.taints` and `ownerReferences` may not change;
- labels under `node-restriction.kubernetes.io/` may never be set or changed;
- `kubernetes.io` / `k8s.io` labels are limited to the kubelet's own set
  (`kubernetes.io/hostname`, `…/arch`, `…/os`, and the
  `node.kubernetes.io/` and `kubelet.kubernetes.io/` prefixes);
- taints supplied at registration (`--register-with-taints`) are allowed, since
  the check is on update.

It also limits a node to Pods bound to itself and to token requests for those
Pods' service accounts.

That closes all three problems above, and it also breaks Blazn as built.

## What breaks

Every Node change the node agent makes uses the kubelet credential
(`/var/snap/microk8s/current/credentials/kubelet.config`).

| Step | Change | Where | Under NodeRestriction |
|---|---|---|---|
| Join | `blazn.dev/bootstrap` and `blazn.dev/sandbox-node` taints at registration | join program (`--register-with-taints`) | allowed |
| Install | `blazn.dev/node=true` label | `applyClusterMutation` | allowed |
| Install | `node.kubernetes.io/exclude-from-external-load-balancers=true` | `excludeFromExternalLoadBalancers` | allowed |
| Activation | workspace label | issuer `assign` (admin credential) | unaffected |
| Activation | remove `blazn.dev/bootstrap`, add `blazn.dev/sandbox-eligible` | `releaseNodeCapacityOnce` | **forbidden** (taints) |
| Uninstall, rollback, repair | re-add `blazn.dev/bootstrap`, remove eligibility | quarantine in `platform_helper.go` | **forbidden** (taints) |
| Uninstall | add `blazn.dev/retired:NoExecute`, set unschedulable | `markNodeRetired` | **forbidden** (taints); unschedulable alone is allowed |
| Retirement | delete the Node | issuer `retire` | unaffected |

Three transitions are affected, and all three are taint changes. The workspace
label and Node deletion already go through the issuer, which is the pattern to
extend.

Nothing else in the cluster depends on a node credential changing taints. The
Node objects' managed fields show Frontro's own labels and taints written by
administrators with `kubectl`, the kubelet writing only its standard labels,
and the only taint written by a node-side `kubectl patch` is Blazn's. The
shadow policy in the rollout verifies this before anything is enforced.

## Design

### 1. The issuer owns every taint transition

The node stops changing taints. The control plane asks the issuer, which runs
as root beside the cluster and already validates the Node's UID, its Blazn
marker and its role before acting.

| Issuer operation | Change, as one compare-and-swap on `resourceVersion` | Preconditions |
|---|---|---|
| `assign` (extended) | set the workspace label, remove `blazn.dev/bootstrap`, add the eligibility label, clear unschedulable | UID matches; Blazn worker; not a control-plane node; not retired; bootstrap taint present or already released for the same workspace |
| `quarantine` (new) | add `blazn.dev/bootstrap`, remove the eligibility label | UID matches; Blazn worker |
| `drain` (new) | add `blazn.dev/retired:NoExecute`, set unschedulable | UID matches; Blazn worker |
| `retire` (existing) | delete the Node | UID matches; retired taint present |

Each is idempotent: a repeat that finds the target state returns success
without writing.

Callers:

- **Activation.** `POST /v1/node-service/activations` already calls the broker's
  `node-assignments` after verifying the install receipt. With the extended
  `assign`, the node's capacity is released by the server in the same step. The
  node agent's `ReleaseNodeCapacity` becomes an observation: it waits until its
  Node shows the released state and records the binding. The signed activation
  grant stays as the node's local proof that the server authorized activation.
- **Uninstall.** A new node-authenticated endpoint,
  `POST /v1/node-service/drains` (node proof over `blazn-node-drain-v1`, bound
  to the active identity and Kubernetes binding), calls the issuer's `drain`.
  The node then waits for its Pods to go, runs `microk8s leave`, removes what it
  installed, and calls the existing retirement endpoint, which deletes the Node.
- **Rollback and repair.** The same pattern through
  `POST /v1/node-service/quarantines`.
- **Operators.** `quarantine` also gives the control plane something it lacks
  today: taking an untrusted or unhealthy node out of service without the
  node's cooperation.

This makes the control plane's decision authoritative. Today the grant is
checked only by the node's own root helper, so it constrains an honest node and
nothing else.

### 2. Eligibility moves under the protected prefix

`blazn.dev/sandbox-eligible` stays writable by the node even with the plugin
on. With the workspace label protected, a node can no longer attract another
workspace's sandboxes, but it could still mark itself eligible while
quarantined and receive its own workspace's sandboxes.

The eligibility label becomes
`node-restriction.kubernetes.io/blazn-sandbox-eligible`, written only by the
issuer. This changes the sandbox Pod's node selector, the phase-5 boundary
policy that pins that selector, and the `blazn-sandboxes` ResourceFlavor. Those
are M4 objects, so this is a second phase with a dual-label period: the issuer
writes both labels, then the selectors switch, then the old label is dropped.

### 3. Offline uninstall

With the API unreachable the node cannot be drained by the server. The agent
falls back to what a node may still do for itself: set itself unschedulable,
run `microk8s leave`, and clean up locally. Its Pods end with the kubelet; the
Node object and any Pod records are removed by the retirement call when the
uninstall is rerun online, as the cleanup journal already allows.

### 4. Compatibility

- Released CLIs up to poc.133 change taints themselves. Once the plugin is on,
  their activation and uninstall fail closed at the taint step, and the install
  rolls back. The API should refuse enrollment from a CLI older than the first
  release with this design, with a message that says to upgrade.
- The new CLI works whether or not the plugin is enabled, because it never
  attempts the forbidden writes. That allows the order below.
- Nodes already installed keep running. Their next uninstall needs the new CLI.

## Rollout

1. **Shadow policy on Frontro.** A ValidatingAdmissionPolicy that mirrors the
   Node rules for group `system:nodes` (other-node writes, taint changes,
   protected-label changes) with `validationActions: [Warn, Audit]`. The API
   server has no audit log, so the evidence is the metric
   `apiserver_validating_admission_policy_check_total` in the existing
   Prometheus. Run it for a week. Expected: violations only from Blazn nodes.
2. **Server release.** Issuer with `drain`, `quarantine` and the extended
   `assign`; broker and API endpoints. Backward compatible with old CLIs.
3. **CLI release** that uses the server path, and the minimum-version check.
4. **Qualify on the throwaway cluster with the plugin enabled:** install,
   sandbox lifecycle, uninstall, offline uninstall, a forced rollback, repair,
   and the dry-run table above, where every "accepted" must become "denied".
5. **Enable on Frontro**, one control-plane host at a time (`ben3`, `ben2`,
   then `ben1`): add `NodeRestriction` to `--enable-admission-plugins` in
   `/var/snap/microk8s/current/args/kube-apiserver`, restart `kubelite`, check
   the API and node readiness, then move on. All three must be done: a request
   that reaches an unchanged API server is not checked.
6. **Verify on Frontro:** the dry-run table, then one real install, sandbox and
   uninstall. Remove the shadow policy.
7. **Phase two:** the protected eligibility label.

Rollback of step 5 is removing the plugin from the arguments and restarting
`kubelite` on each host.

## Risks and open questions

- **Restarting `kubelite`** on a control-plane host restarts the API server,
  scheduler, controller manager and kubelet on that host. With three hosts and
  one at a time the API stays available, but workloads on that host see a
  kubelet restart. It needs a maintenance window.
- **MicroK8s itself.** Whether its cluster agent, join or leave paths write
  Nodes with a node credential is untested. Step 4 covers it; the shadow
  policy covers the running cluster.
- **Argument persistence.** The arguments file lives in the snap's data
  directory and should survive a snap refresh, but refreshes are held and this
  has not been tested.
- **A leftover label.** `ben3`, a Frontro control-plane host, still carries
  `blazn.dev/node=true` from the POC. The issuer refuses control-plane nodes
  by role label, and MicroK8s control-plane hosts here carry
  `node.kubernetes.io/microk8s-controlplane`, not `node-role.kubernetes.io/*`.
  The issuer's control-plane check should also treat that label as
  disqualifying, and the stale label should be removed.
- **Not covered by this plugin:** a node can still read the Secrets and
  ConfigMaps of Pods scheduled to it, and run anything as root on its own
  host. Those are bounded by placement (only its workspace's sandboxes) and by
  the hardened sandbox runtime (M4.6).

## Work breakdown

| Piece | Area |
|---|---|
| Shadow ValidatingAdmissionPolicy and a Prometheus query for it | hosting |
| Issuer: `drain`, `quarantine`, extended `assign`; reject `microk8s-controlplane` nodes | issuer |
| Broker and API: drains and quarantines endpoints; activation uses the extended `assign`; minimum CLI version | control API |
| Node agent: observe instead of patching; drain through the API; offline fallback | CLI |
| Qualification scripts: the dry-run table as an automated check | qualification |
| Enabling the plugin on the control-plane hosts | hosting, with a maintenance window |
| Protected eligibility label, selectors, boundary and ResourceFlavor | sandboxes (M4) |
