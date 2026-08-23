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
It validates `blazn.yaml` against the merged schema and semantic argv
rules, validates the template schema and all Project/Agent/template identity
cross-references, executes every negative fixture, runs the committed direct-argv
tests, and recomputes the dependency-lock and closed build-context identities.

The bootstrap template is immediately resolvable from the deterministic IDs in
`fixtures/identities.json` and the pinned Node index/children. It runs only
`node --version`; the later Gate 6 build replaces that bootstrap image identity
with the separately qualified coding-agent output before publication. The Agent
draft binds this exact bootstrap template and contains no credential material.

The Dockerfile base is the OCI index digest for the official Node 22.19.0
Alpine image. The pinned index contains both `linux/amd64` and `linux/arm64`.
This slice intentionally does not build or publish the image. Gate 6 build,
registry, Sandbox lifecycle, evidence, and publication steps remain separately
approval-gated.
