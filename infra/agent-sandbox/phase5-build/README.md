# Phase 5 image pipeline

The reviewed path for producing and publishing the `blazn-sandbox-controller`
and `blazn-sandbox-io` images.

`build-images.sh SOURCE_DIR OUTPUT_DIR` runs on a build host with Docker and
the pinned Buildx plugin (`BUILDX_URL`/`BUILDX_SHA256` in `../versions.env`).
It refuses a dirty checkout or one that is not the reviewed
`BLAZN_EXPECTED_SOURCE_COMMIT`, builds both Dockerfiles for `linux/amd64` and
`linux/arm64` into content-addressed OCI archives with provenance and SBOM
attestations disabled, scans each archive with the pinned Trivy and fails on
CRITICAL findings, and writes `build-report.json` recording the index digest,
both per-architecture child digests, and the archive SHA-256 for each image.
It never pushes.

`publish-images.sh BUILD_OUTPUT_DIR` runs on the authoritative live host. It
re-verifies the report against the separately reviewed
`BLAZN_EXPECTED_CONTROLLER_INDEX` / `BLAZN_EXPECTED_SANDBOX_IO_INDEX` digests
and against the archive bytes, reads the existing in-cluster registry
credential through kubectl into a root-only 0600 file that is deleted on
exit and never printed, pushes each OCI index to the existing
`registry.blaze.internal:5000` repositories with the pinned crane, tags with
the reviewed source commit, and fails unless the remote digest equals the
reviewed index digest and the remote index carries both architectures.

`provision-registry-pull.sh` copies the existing in-cluster registry
credential server-side into `blazn-poc-system` and `blazn-poc-sandboxes` as
`blazn-registry-pull` (annotated with the boundary transaction) and attaches
it to the Sandbox ServiceAccounts so kubelet can pull the published
digest-pinned images. The credential never leaves the cluster.

`../test-phase5-build-e2e.sh` proves the pipeline end to end: it builds both
images from the current checkout, verifies the digest report, refuses a
wrong-commit build, publishes through the real publish script into a
disposable digest-pinned local registry with a fake kubectl credential
source, verifies the remote index carries both architectures, and proves a
tampered archive is refused before anything is pushed.
