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
`--source` must name a commit reachable from a fetched `origin` ref or match an
exact remote branch/tag tip; push before starting if that preflight fails. It
reuses the already-published
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

## Persistent developer session

For routine work, keep one bounded Sandbox across multiple commands. The local
receipt contains only the Sandbox ID and lifecycle metadata, is mode `0600`,
and defaults outside the repository under the user state directory.

```text
dev=examples/blazn-development-sandbox/dev-session.sh
$dev start --workspace WORKSPACE_ID --expires 2h
$dev status
$dev exec -- sh -lc 'cd /workspace/src/blazn && go test ./...'
$dev upload ./local-file /workspace/src/blazn/local-file
$dev download /workspace/src/blazn/result ./result
$dev patch ./checkpoint.patch
$dev finish --patch ./final-change.patch
```

`start` accepts only a clean, pushed commit and records its materialized source
as a fixed local Git baseline. Expiry is capped at two hours. `patch` stages all
changes (including untracked files and deletions) and produces a binary-capable Git patch,
downloads it atomically, verifies its server checksum, and refuses to overwrite
an existing destination. `finish` stops the exact recorded Sandbox, deletes
that same ID, and records `deleted` only after polling proves both `state` and
`desiredState` are deleted. The server-side expiry remains the backstop if the
workstation disconnects. Use `--receipt PATH` to run multiple named sessions.
Individual uploads and downloads are limited to 8 MiB, and downloads refuse to
overwrite local paths. `finish` requires either a final verified patch download
or an explicit `--discard`, so teardown cannot silently destroy unexported work.
`exec` decodes remote stdout and stderr while preserving
the CLI exit distinction: `1` for a remote/API failure, `7` when unavailable,
and `9` for truncated or partial evidence.
