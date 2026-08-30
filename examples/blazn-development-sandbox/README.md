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
BLAZN_WORKSPACE_ID=WORKSPACE_ID \
  examples/blazn-development-sandbox/run-live.sh
```

The entrypoint finds `blazn` on `PATH`, uses the current Git commit, targets the
qualified AMD64 lane, and reads the immutable template reference from the
checked-in template. The default commit requires a clean tree. An explicit
`--source` must name a commit reachable from an `origin` ref after the
entrypoint refreshes the remote refs; push before starting if that preflight
fails. It reuses the already-published
immutable template by default. Run `run-live.sh --help` to select ARM64 or
override another default. Use `--publish-template` only during first-time
workspace setup; it overrides an inherited `BLAZN_SKIP_TEMPLATE_PUBLISH`.

The acceptance validates and optionally publishes the template, then creates
and watches an exact-commit Sandbox,
checks both toolchains, runs the Go and Node suites, verifies upload/download,
creates the patch artifact, stops the Sandbox, then deletes it and waits for
both terminal states. Stop-to-delete requires database migration 036 or later.
Before stopping, it downloads and verifies the patch and a neighboring
`.sha256` file. Each run gets a fresh
`${TMPDIR:-/tmp}/blazn-development-output.XXXXXX/` directory outside the source
tree, so reruns never collide. The final names appear only after same-directory
temporary files pass checksum verification. Use `--patch-output PATH` to choose
another durable destination; explicit existing files are never overwritten.
Set `BLAZN_E2E_KEEP_EVIDENCE=1`
to retain the printed temporary evidence directory, including complete failing
test logs, after cleanup.

The live Sandbox Go matrix excludes `internal/node`, whose installer tests
deliberately require root-owned private host-directory ancestry, and
`internal/workspace`, whose default-home test requires the host libc account
resolver rather than Go's pure-Go container fallback. Those host-only packages
remain covered by the full Linux CI lane. The Sandbox runs every other Go
package plus the complete control-API and coding-agent Node suites.
