# Coding-agent development example

This is the deterministic, non-sensitive Phase 6 example project. It freezes
the `blazn.yaml` DevelopmentProject, `agent.yaml` Agent draft,
`sandbox-template.yaml` bootstrap template, build context, dependency lock,
direct-argv tests, and a minimal source-editing task. It does not contain credentials and its
offline checks do not contact a registry, builder, cluster, or Management API.

From the repository root, prepare the already-pinned contract validator once,
then run the example checks without network access:

```text
make test-development-contract
examples/coding-agent/scripts/test-offline.sh
```

The first command builds the merged TypeScript contract verifier and may run
`npm ci` only when its existing validator dependencies have not yet been
installed. After that prerequisite exists, `test-offline.sh` is strictly
offline: it refuses a missing, stale, or substituted verifier before rebuilding
it from the bound source with already-installed dependencies.
It also refuses any toolchain other than exact Node `22.19.0` and npm `10.9.3`.
It validates `blazn.yaml` against the merged schema and semantic argv
rules, validates the template schema and all Project/Agent/template identity
cross-references, executes every negative fixture, runs the committed direct-argv
tests, and recomputes the dependency-lock and closed build-context identities.

The IDs in `fixtures/identities.json` are deterministic offline placeholders;
they are not seeded resource IDs. A later authorized import must create the real
Project/template/version resources and regenerate every dependent identity
before API submission. The bootstrap declaration runs only `node --version`;
the later Gate 6 build replaces that image identity with the separately
qualified coding-agent output before publication.

For interactive CLI development, `sandbox-template-dev.yaml` retains the same
pinned images and policy but runs a long-lived direct Node argv. Publish that
version and create a Sandbox from it before using `sandbox exec`, `upload`, or
`download`; the bootstrap template is intentionally a short-lived image proof.

`fixtures/base-image.json` is an unqualified offline declaration, not trusted
registry evidence: this slice does not include raw OCI index bytes or a signed
inspection receipt and therefore does not prove its child-platform mapping.
Gate 6 must re-resolve and verify the index plus exact AMD64/ARM64 descriptors
through the approved registry boundary before any build or template promotion.
