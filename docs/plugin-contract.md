# Blazn plugin contract v1

Blazn plugins extend the single `blazn` command without allowing arbitrary
package discovery. The core binary contains an allowlisted command catalog and
owns installation, verification, activation, rollback, and dispatch.

The catalog defines independently signed Social and Content plugins:

- `blazncloud/blazn-social` owns `blazn social ...` and the `person`,
  `company`, `contact`, `connections`, `post`, `evidence`, `entity`, `data`,
  and `providers` aliases.
- `blazncloud/blazn-content` owns `blazn content ...` and the `media`, `image`,
  `video`, `audio`, `render`, and `remix` aliases.

Command ownership is exclusive. Content is not a Social alias.

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
blazn-<plugin>_<version>_<os>_<arch>.tar.gz
```

The core uses authenticated `gh`, resolves an immutable release tag, downloads
only the catalog-pinned repository assets, verifies the OpenSSH signature and
signed checksums, validates the strict manifest and core compatibility, checks
the single-member archive, and atomically activates a receipt-owned executable.
GitHub release metadata is not a trust root.

Set `BLAZN_PLUGIN_VERSION` to an exact semantic release tag when a reproducible
installation or qualification run must not follow the latest release. The
installer fails closed if GitHub returns a different tag; signatures,
checksums, manifest compatibility, and the candidate handshake remain required.

Social and Content releases use independently pinned `blazn-social-release`
and `blazn-content-release` signing identities and namespaces. Compromise of
one plugin's release key does not authorize releases for the other.

The v2 Social release key fingerprint is
`SHA256:L7rcTp4WYKPsYNmDx8ElbxwHlVc8VQvX9EH4SGlLcFQ`. Root releases predating
this rotation continue to trust only the retired v1 key and therefore fail
closed when presented with v2-signed Social artifacts.

The rotation rollout publishes the v2-trusting root before the first v2-signed
Social release, then qualifies both exact versions together. The new root
rejects retired v1-signed Social releases as a deliberate downgrade defense;
operators must retain the preceding root release only if they must reinstall a
historical v1-signed Social artifact.

## Runtime context

Before dispatch, root `blazn` creates a versioned runtime envelope and replaces
any inherited `BLAZN_PLUGIN_CONTEXT` value. The JSON envelope contains the core
and plugin protocol versions, a unique invocation ID, output format, API
origin, and the authenticated selected Workspace identity when available.

The context status is explicit:

- `selected` includes the API origin, user ID, and Workspace ID;
- `unselected` means authentication succeeded but no Workspace is selected;
- `unavailable` means root could not resolve authenticated Workspace context.

The envelope never includes access tokens, refresh tokens, integration
credentials, provider keys, local model addresses, or credential-store paths.
Root dispatch also replaces the process environment with a small portability
allowlist covering executable lookup, home and temporary directories, locale,
terminal behavior, operating-system process requirements, and TLS certificate
locations. Ambient provider, GitHub, cloud, SSH-agent, proxy, and arbitrary
application variables are not inherited by plugins.

Three existing Social settings cross that boundary by exact name only:
`BLAZN_SOCIAL_HOME`, `BLAZN_SEC_USER_AGENT`, and `HUNTER_API_KEY`. They are
available only when dispatching the `social` plugin, are not exposed to Content
or future plugins, and are excluded from the pre-activation candidate
handshake. Broader provider credentials remain credential-store work rather
than ambient environment inheritance.

Plugins reject unknown fields, incoherent states, unsafe origins, unsupported
versions, and contexts larger than 16 KiB. The runtime envelope is metadata,
not authorization; future Management API and model operations use a separately
scoped root/Workspace broker rather than credentials copied to plugins.

The normative schema is
`packages/contracts/plugin-runtime-context.schema.json`.

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
