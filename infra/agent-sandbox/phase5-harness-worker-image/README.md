# Harness worker foundation image

This directory packages `blazn-harness-worker` as a reproducible
`linux/amd64` + `linux/arm64` OCI image foundation using the same reviewed
source-commit, pinned-builder, per-architecture vulnerability scan, and digest
report conventions as the Phase 5 controller images.

This is **not a runnable Hermes image**. The foundation intentionally contains
no `/opt/blazn/hermes`, carries OCI labels stating that Hermes is absent, and
the build report records `hermesIncluded: false` and `runnable: false`. Do not
deploy it as a Run workload.

`build-foundation.sh SOURCE_DIR OUTPUT_DIR`:

- refuses a dirty checkout or a commit other than
  `BLAZN_EXPECTED_SOURCE_COMMIT`;
- builds both supported Linux architectures into one content-addressed OCI
  archive without mutable tags;
- scans each architecture with the pinned Trivy and fails on CRITICAL
  findings; and
- records the index digest, child digests, archive SHA-256, and non-runnable
  Hermes gate in `build-report.json`.

The remaining production gate is an independently approved, licensed Hermes
artifact for each supported architecture. A reviewed derived image must copy
that artifact to `/opt/blazn/hermes` as root-owned mode `0555`, preserve the
protected `/opt/blazn` directory chain, rebuild and scan the complete image,
and bind the exact executable SHA-256 in `harnessExecutableDigest`. Controller
launch/mount wiring and a disposable real Run must then qualify that final
digest before it is deployed.

Example build from a clean reviewed checkout:

```sh
BLAZN_EXPECTED_SOURCE_COMMIT=$(git rev-parse HEAD) \
  ./infra/agent-sandbox/phase5-harness-worker-image/build-foundation.sh \
  . /secure/output/harness-worker-foundation
```
