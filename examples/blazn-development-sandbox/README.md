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

After publishing the development template, run the live CLI-only acceptance
from an authenticated workstation:

```text
examples/blazn-development-sandbox/test-live.sh \
  /path/to/blazn WORKSPACE_ID \
  examples/coding-agent/sandbox-template-dev.yaml \
  coding-agent@go-1.26.2-node-22.19.0-poc-dev-2 \
  EXACT_SOURCE_COMMIT amd64
```

The test publishes the template, creates and watches an exact-commit Sandbox,
checks both toolchains, runs the Go and Node suites, verifies upload/download,
creates the required patch artifact, deletes the Sandbox, and waits for its
terminal deleted state. Set `BLAZN_SKIP_TEMPLATE_PUBLISH=1` only when the exact
immutable version has already been published.
