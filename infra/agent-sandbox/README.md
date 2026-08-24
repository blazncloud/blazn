# Agent Sandbox Phase 4A qualification

This directory qualifies pinned upstream orchestration in a disposable kind
cluster. It never installs into the shared cluster or ben4's standalone
MicroK8s. `versions.env` is the source of truth for release assets, checksums,
container inventory, the kind node, and the synthetic workload.

Run the static audit with `./test-static.sh`. Run the full install/lifecycle/
uninstall proof on an isolated Linux Docker host with `./test-disposable.sh`.
The latter creates only a collision-resistant `blazn-as-v056-<12-hex>` kind
cluster after proving that name is absent. Cleanup authority is enabled only
for that verified creation attempt, including partial-create failures.

Passing Phase 4A proves orchestration compatibility, not hardened isolation.
Real untrusted or cross-tenant work remains blocked until a gVisor or Kata
RuntimeClass is separately installed and qualified.

The non-mutating Phase 5 controller image, suspended Deployment, narrow RBAC,
and exact egress preparation live in `phase5-controller/`. That bundle does not
replace or broaden Phase 4C and cannot be scaled against Phase 4C's synthetic,
single-name admission policy.

## Upstream inventory and licenses

- Kubernetes SIG Apps Agent Sandbox v0.5.6: Apache-2.0.
- Kubernetes SIG Scheduling Kueue v0.19.2: Apache-2.0.
- Kubernetes SIG Testing kind v0.32.0: Apache-2.0.
- `kindest/node` contains Kubernetes and distribution components under their
  respective upstream licenses.
- BusyBox 1.36.1 is GPL-2.0 and is used only as a disposable synthetic workload,
  not redistributed by Blazn.

The release manifests reference one controller image each. Their resolved
multi-platform digest inventory is recorded in `versions.env`; tests also
verify the original manifest checksums, require exactly one expected source-tag
occurrence, rewrite it to the recorded digest reference before apply, and prove
the running Pod `imageID` resolves to that digest.

## Static trust findings

Agent Sandbox installs four CRDs, two ClusterRoles, two ClusterRoleBindings,
admission webhooks, services, and one controller. Its service account can
delete Pods cluster-wide but cannot list Secrets cluster-wide. Consequently the
upstream all-in-one manifest must not be applied to the shared cluster without
a reviewed namespace/admission/RBAC boundary and the serialized cluster-change
lock.

Kueue admission is Pod-level: Blazn must place
`kueue.x-k8s.io/queue-name` in every managed Sandbox `podTemplate`. A Sandbox
without that label must fail validation rather than fall back to an unmanaged
Pod.

Acceptance requires the generated Pod to be Running and the admitted Kueue
Workload to reserve exactly the fixture's 100m CPU and 64Mi memory. Uninstall
proves zero Kueue CRDs, ClusterRoles/Bindings, visibility APIServices, webhooks,
fixture queues/flavors, controller namespace, and Docker resources.
