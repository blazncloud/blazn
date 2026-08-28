# Blazn development sandbox

This image is the interactive development toolchain used through `blazn
sandbox`. It pins the same Go and Node versions as the repository, includes
Git, and preloads the exact Go module and Control API npm dependency locks so
the default-deny runtime does not need package-registry access.

The runtime writes HOME, temporary files, Go build cache, and npm cache only to
the sandbox artifact volume. The exact committed source tree is materialized at
`/workspace/src/blazn`; source edits are writable, while the base image remains
read-only and unprivileged.
