# Coding-agent development example

This is the deterministic, non-sensitive Phase 6 example project. It freezes
the DevelopmentProject declaration, build context, dependency lock, direct-argv
test, and a minimal source-editing task. It does not contain credentials and its
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
offline: it refuses missing validator artifacts rather than installing them.
It validates `blazn.project.json` against the merged schema and semantic argv
rules, executes every negative fixture, runs the committed direct-argv test,
and recomputes the dependency-lock and build-context identities.

The Dockerfile base is the OCI index digest for the official Node 22.19.0
Alpine image. The pinned index contains both `linux/amd64` and `linux/arm64`.
This slice intentionally does not build or publish the image. Gate 6 build,
registry, Sandbox lifecycle, evidence, and publication steps remain separately
approval-gated.
