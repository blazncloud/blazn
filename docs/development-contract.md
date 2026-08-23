# Development build and publication contract v1alpha1

This contract freezes the narrow Phase 6 POC boundary. It does not install a
builder, contact a registry, publish a template, or claim live evidence.

## Existing resource boundary

A DevelopmentProject declaration extends one canonical Workspace `Project`; it
does not create another project identity or authorization system. A Build is a
typed development resource and may link its execution and evidence to canonical
Runs and Artifacts. Generic Run receipts are not a substitute for the Build's
source, dependency, builder, platform, image, test, and publication evidence.

The normative input and result schemas are:

- `packages/contracts/development-project.schema.json`
- `packages/contracts/development-build.schema.json`
- `packages/contracts/development-cli-contract.json`

## Immutable inputs and outputs

The project declaration binds an HTTPS repository, an immutable
SandboxTemplateVersion ID and digest, exactly `linux/amd64` and `linux/arm64`,
repository-confined build and lockfile paths, lockfile digests, committed argv
test definitions, and approved builder, network, resource, and publication
policy profiles. The registry value names a repository only; it cannot contain a
mutable tag or an output digest.

Test definitions are keyed by stable name. They use direct argv and cannot
invoke a shell or `env` launcher or carry credential assignments, secret-bearing
flags, bearer values, or URL userinfo. The controller binds the canonical test
definition digest and exact source commit to every POC test result.

A Build binds the canonical Workspace and Project, exact 40- or 64-hex source
commit, project manifest, template, locks, build plan, controller-selected
builder and immutable builder image. A successful Build records one immutable
OCI index and the exact AMD64 and ARM64 child references, typed Artifact IDs,
the build-context digest, per-architecture refresh content/cache/input/image
bindings, committed project-test evidence, secret and template-security results,
lifecycle evidence for both platforms, and an auditable reference-Build versus
candidate-Build material comparison. Explained nondeterminism requires a
bounded explanation and review Artifact and does not silently masquerade as
reproducibility.

Changing any material input creates a new Build. Branches, tags, mutable image
tags, incomplete architecture sets, client-asserted source resolution, and
unbound evidence are not accepted.

The semantic verifier also requires the Build repository and builder profile to
equal the committed project declaration. Every output index, child image, and
refresh image remains in the declared registry repository. Each architecture's
refresh input digest is derived from its platform plus the exact template,
source, locks, build context, and plan; its cache key is domain-separated from
that input digest.

## Authority and tenant boundary

A Workspace user may request validation, build, test, evidence export, or
publication only when policy grants that capability. The user-facing API and
CLI never accept authoritative builder identity, output digest, test or scan
result, provenance, or publication eligibility. Those observations may be
finalized only by a separately authenticated internal builder/controller
authority and remain bound to the same Workspace and Project.

`services/control-api/src/development-contract.ts` is the normative semantic
verifier for invariants JSON Schema cannot express. Before terminal commit, the
controller authenticates with its fixed mTLS workload identity, resolves the
canonical Run and every evidence Artifact, and verifies exact Workspace and
Project identity, complete typed evidence, committed project/test digests,
per-platform refresh/image binding, and reproducibility comparison. User-facing
sessions cannot call the controller finalizer boundary.

Reproducibility resolves a distinct reference Build in the same tenant, binds
its receipt, recomputes both input identities, requires unchanged inputs, and
then compares the recorded material digests. A client-provided reference ID or
digest is not evidence.

Raw BuildKit addresses, BuildKit client certificates, registry credentials,
object-store keys, signed URLs, and secret values are neither returned nor
written into evidence. Builds execute repository and dependency code as
untrusted work in a bounded isolated builder. Credential leases are short-lived
and non-exportable and must not enter image layers, logs, caches, refresh
artifacts, or receipts.

Publication is a separate authorized, idempotent mutation bound to the exact
Build ID, expected Build version, Build receipt digest, target template version,
and target template digest. It fails closed for a non-successful or stale Build,
mutable or mismatched output, missing architecture, secret finding, failed
project, security, lifecycle, or cleanup test, or unexplained nondeterminism.
Ineligible Builds carry at least one machine-readable refusal reason.
Publication identity is one atomic object or `null`; if present, its Build
receipt and image index must exactly match the qualified Build outputs. Partial
or substituted publication identities are invalid. The stable target template
is committed in the DevelopmentProject; the controller resolves its Workspace,
draft version, candidate version ID, and candidate digest before finalization,
and publication must match that authorized target exactly.

## CLI acceptance surface

```text
blazn dev validate [-f blazn.yaml]
blazn dev build --ref COMMIT --request-id ID
blazn dev test BUILD --suite poc --request-id ID
blazn dev status BUILD
blazn dev evidence BUILD --output-dir DIRECTORY
blazn dev publish BUILD --expected-version N --request-id ID
```

`dev validate` is offline and non-mutating. The other commands use the selected
Workspace and Project and authenticated Management API. Evidence export refuses
an existing target and symlink traversal and never writes credentials, object
keys, or signed URLs. `dev init` is intentionally outside this POC acceptance
contract.

## Gate 6 acceptance

Gate 6 requires all of the following live evidence before it is called complete:

1. A checked-in `examples/coding-agent` with the declarations, Dockerfile,
   exact dependency locks, deterministic tests, and executable instructions.
2. Offline validation rejects unknown/secret fields, unsafe paths, mutable
   identities, missing locks or architectures, and unapproved policies.
3. An exact fresh commit builds native AMD64 and cluster-scheduled Linux ARM64
   images through the approved BuildKit trust boundary.
4. One immutable OCI index resolves to the recorded child digest for each
   architecture.
5. A content-addressed refresh layer binds template, architecture, source,
   locks, and build inputs and contains no source checkout or credentials.
6. Both images pass the required security and full Sandbox lifecycle suite on
   their target platforms, including artifact export and cleanup.
7. Repeating unchanged inputs yields the same material digest or records
   reviewed, explained nondeterminism.
8. Redacted evidence binds source, locks, builder, outputs, inspection, scans,
   tests, placement, cleanup, timestamps, and signatures or attestations.
9. Every documented failure condition is observed to refuse publication.
10. Successful publication renders and promotes the exact qualified image index
    in the SandboxTemplateVersion, and a later Sandbox resolves the same index
    and architecture child digest.
11. Temporary jobs, credentials, egress windows, and objects are removed without
    affecting unrelated Frontro workloads.

Contract validation and example authoring can proceed before Phase 5 live
qualification. Live publication remains gated on Phase 5 lifecycle evidence,
approved and reverified BuildKit/registry access, approved AMD64 and ARM64
capacity, signing authority, and one serialized publication owner.

## Deferred hardening

`dev init`, generalized DevelopmentEnvironment/Draft/ChangeSet/Bundle/Release
resources, multiple source-control and registry providers, automatic refresh
coalescing and warm pools, richer cache economics, full license/transparency-log
policy, and the desktop development UI are tracked beyond the narrow POC gate.
