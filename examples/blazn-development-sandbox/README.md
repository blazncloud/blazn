# Blazn development sandbox

This image is the interactive development toolchain used through `blazn
sandbox`. It pins the same Go and Node versions as the repository, includes
Git, and preloads the exact Go module and Control API npm dependency locks so
the default-deny runtime does not need package-registry access.

The bundled npm client is retained for running committed scripts, but its
network/archive installation path is deliberately removed. Dependency changes
must be locked and reviewed in a new image build rather than installed inside
the default-deny sandbox.

The runtime writes HOME, temporary files, Go build cache, and npm cache only to
the sandbox artifact volume. The exact committed source tree is materialized at
`/workspace/src/blazn`; source edits are writable, while the base image remains
read-only and unprivileged.
