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
- taints are **not** checked at creation. `--register-with-taints` works, and
  so does registering with no taints at all.

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
as root beside the cluster and validates the Node's UID, its Blazn marker and
that it is not a control-plane host before acting.

| Issuer operation | Change | Used for |
|---|---|---|
| `assign` (extended) | set the workspace label, remove `blazn.dev/bootstrap`, add the eligibility label, clear unschedulable | activation |
| `hold` (#271) | set or clear `blazn.dev/placement-hold=<paused\|quarantined\|draining\|offline>:NoSchedule` | operator pause, quarantine and resume; the API's offline reconciler |
| `rebootstrap` (new) | add `blazn.dev/bootstrap`, remove the eligibility label | a node-initiated rollback or repair returning to its pre-activation state |
| `drain` (new) | add `blazn.dev/retired:NoExecute`, set unschedulable | the start of uninstall |
| `retire` (existing) | delete the Node | the end of uninstall |

"Quarantine" means one thing: the operator hold from #271. The node-initiated
return to the bootstrap state has its own name.

Every operation is a JSON patch preconditioned on `resourceVersion` that adds
or removes only its own taint key and keeps every other taint, so none of them
can drop `blazn.dev/placement-hold`, another operation's taint, or a Frontro
taint. Each is idempotent: a repeat that finds the target state succeeds
without writing.

Callers:

- **Activation.** `POST /v1/node-service/activations` already calls the broker's
  `node-assignments` after verifying the install receipt. With the extended
  `assign`, the server releases the node's capacity in the same step. The node
  agent's `ReleaseNodeCapacity` becomes an observation: it waits until its Node
  shows the released state and records the binding. The signed activation grant
  stays as the node's local proof that the server authorized activation.
- **Uninstall.** A new node-authenticated endpoint,
  `POST /v1/node-service/drains` (node proof over `blazn-node-drain-v1`, bound
  to the active identity and Kubernetes binding), calls `drain`. The node waits
  for its Pods to go, runs `microk8s leave`, removes what it installed, and
  calls the existing retirement endpoint, which deletes the Node.
- **Rollback and repair.** `POST /v1/node-service/rebootstraps`, same pattern.
- **Operators.** The `hold` route and operations from #271.

This makes the control plane's decision authoritative. Today the grant is
checked only by the node's own root helper, so it constrains an honest node and
nothing else.

### 2. Taints are the enforcement, so registration must be guarded

With the plugin on, a node cannot remove `blazn.dev/bootstrap` or
`blazn.dev/placement-hold`, and a sandbox Pod tolerates only
`blazn.dev/sandbox-node`. A node that adds `blazn.dev/sandbox-eligible` to
itself while bootstrapping or held therefore attracts nothing, and the
eligibility label does not need to move under the protected prefix. The
invariant to keep is: **every state in which a node must not receive sandboxes
is expressed as a taint.**

That invariant has one hole. NodeRestriction checks taints on update, not on
creation, and a node identity may create its own Node (`auth can-i create
nodes` is yes; delete is no). So:

- a machine joining for the first time can register without
  `--register-with-taints`, and Frontro's ordinary Pods can then be scheduled
  onto it;
- after retirement deletes the Node, the machine's kubelet certificate is still
  valid, and it can register again with no taints.

Neither gets another workspace's sandboxes, because the workspace label is
protected. Both put an untainted tenant machine in Frontro's cluster, which is
the incident class from M3.

The issuer's `observe` does not cover this. It runs once, when the node reports
its join, and refuses to continue unless the Node has exactly one bootstrap
taint; it does not remove an untainted Node, and it never sees a machine that
skips the call or registers again later.

The guard is a ValidatingAdmissionPolicy on Node **creation** by group
`system:nodes`, with two rules and a parameter object:

- the new Node must carry `blazn.dev/bootstrap=pending:NoSchedule` and
  `blazn.dev/sandbox-node=true:NoSchedule`, unless its name is in the list of
  Frontro's own hosts;
- the name must not be in the list of retired Blazn node names.

The second rule stands in for certificate revocation, which the cluster cannot
do: a worker's kubelet certificate is signed by the cluster CA, is valid for
ten years, and the API server has no revocation list. The Node authorizer
already confines a certificate to its own node name, so refusing that name at
creation is enough to keep a retired machine out. The issuer adds a name to the
list when it retires a Node and removes it when it issues a new join credential
for the same name, so a machine can be reinstalled.

The guard does not depend on NodeRestriction and can be enforced first. It is
separate from the sandbox boundary policy and its transaction. Its cost is that
adding a Frontro host means adding its name to the host list. Before it is
switched to `Deny`, a fresh `blazn node install` must pass under it: the join
registers with both taints through `--register-with-taints`.

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

1. **Shadow policies on Frontro.** Two ValidatingAdmissionPolicies with
   `validationActions: [Warn, Audit]`: one mirrors the Node update rules for
   group `system:nodes` (other-node writes, taint changes, protected-label
   changes); the other is the registration guard from section 2. The API
   server has no audit log, so the evidence is the metric
   `apiserver_validating_admission_policy_check_total` in the existing
   Prometheus. Run it for a week. Expected: violations only from Blazn nodes.
2. **Server release.** Issuer with `drain`, `rebootstrap` and the extended
   `assign`, stacked on #271's `hold`; broker and API endpoints. Backward
   compatible with old CLIs. Switch the registration guard to `Deny`.
3. **CLI release** that uses the server path, and the minimum-version check.
4. **Qualify on the throwaway cluster with the plugin enabled:** install,
   sandbox lifecycle, uninstall, offline uninstall, a forced rollback, repair,
   and the dry-run table above, where every "accepted" must become "denied"
   except the self-added eligibility label. Also, as a node identity: removing
   `blazn.dev/placement-hold` from itself must be denied, a held node that adds
   the eligibility label must receive no sandbox, and registering without the
   taints must be denied.
5. **Enable on Frontro**, one control-plane host at a time (`ben3`, `ben2`,
   then `ben1`): add `NodeRestriction` to `--enable-admission-plugins` in
   `/var/snap/microk8s/current/args/kube-apiserver`, restart `kubelite`, check
   the API and node readiness, then move on. All three must be done: a request
   that reaches an unchanged API server is not checked.
6. **Verify on Frontro:** the dry-run table, then one real install, sandbox and
   uninstall. Remove the update shadow policy; keep the registration guard.

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
  `blazn.dev/node=true` from the POC. The issuer's control-plane guard used to
  look only for `node-role.kubernetes.io/*` labels, which MicroK8s hosts here
  do not carry; #271 makes it also recognise
  `node.kubernetes.io/microk8s-controlplane`, so every operation refuses
  `ben3`. Removing the stale label is a change to a production control-plane
  host and is still to be decided.
- **Kubelet certificates outlive retirement.** They are valid for ten years
  and cannot be revoked individually; the retired-name list in the
  registration guard is the substitute. Rotating the cluster CA or moving
  workers to short-lived certificates would be stronger and is out of scope.
- **Not covered by this plugin:** a node can still read the Secrets and
  ConfigMaps of Pods scheduled to it, and run anything as root on its own
  host. Those are bounded by placement (only its workspace's sandboxes) and by
  the hardened sandbox runtime (M4.6).

## Work breakdown

| Piece | Area |
|---|---|
| Shadow policy for Node updates, the registration guard, and a Prometheus query for both | hosting |
| Issuer: extended `assign`, then `drain`, then `rebootstrap`, on top of #271; maintain the retired-name list | issuer |
| Broker and API: drains and rebootstraps endpoints; activation uses the extended `assign`; minimum CLI version | control API |
| Node agent: observe instead of patching; drain through the API; offline fallback | CLI |
| Qualification: the dry-run table and the three extra checks as an automated script | qualification |
| Enabling the plugin on the control-plane hosts | hosting, with a maintenance window |
