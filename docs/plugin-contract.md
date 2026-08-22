# Blazn plugin contract v1

Blazn plugins extend the single `blazn` command without allowing arbitrary
package discovery. The core binary contains an allowlisted command catalog and
owns installation, verification, activation, rollback, and dispatch.

Milestone 1 defines one plugin, `social`, from the private
`blazncloud/blazn-social` repository. Its canonical namespace is
`blazn social ...`; approved aliases are `person`, `company`, `contact`,
`connections`, `content`, `post`, `evidence`, `entity`, `data`, and
`providers`.

## Missing plugin behavior

- Interactive human mode prompts `[y/N]`.
- Declining performs no mutation.
- Approval installs the signed plugin and replays the command exactly once.
- JSON and non-TTY modes never prompt or install implicitly and return
  `plugin_required` with exit code `7`.
- Unknown commands never trigger registry or package searches.
- Help is available from the catalog and never installs a plugin.

## Release assets

Each private GitHub release contains:

```text
plugin.json
SHA256SUMS
SHA256SUMS.sig
blazn-social_<version>_<os>_<arch>.tar.gz
```

The core uses authenticated `gh`, resolves an immutable release tag, downloads
only the catalog-pinned repository assets, verifies the OpenSSH signature and
signed checksums, validates the strict manifest and core compatibility, checks
the single-member archive, and atomically activates a receipt-owned executable.
GitHub release metadata is not a trust root.

Social releases use the independently pinned `blazn-social-release` signing
identity and namespace so compromise of one product's release key does not
authorize releases for the other.

## Manifest

The strict JSON manifest contains integer `schemaVersion`, plugin name and
semantic version, protocol version, minimum core version, a basename-only
executable, command claims, and descriptive capabilities. Unknown fields,
unsafe paths, duplicate commands, unsupported protocol versions, and manifests
larger than 64 KiB fail closed.

## Local storage

Plugins are installed below:

```text
~/.local/share/blazn/plugins/<name>/versions/<version>/
```

Directories must be owned by the current user and mode `0700`. Executables and
receipts are validated as regular, non-symlink, single-link files. The active
receipt includes the binary digest and prior version for rollback. Plugins run
with the user's authority; capability declarations are not a sandbox.
