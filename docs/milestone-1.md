# Milestone 1 — Standalone CLI Distribution

## Outcome

Milestone 1 produces one statically linked `blazn` binary that requires no Node.js, Python, npm, Go, kubectl, or application runtime on the target machine.

The supported release targets are:

- macOS ARM64
- Linux AMD64
- Linux ARM64

The initial CLI includes:

- Root and command help.
- Human and deterministic JSON output.
- Build and contract version reporting.
- Offline platform, install, configuration-permission, installer-tool, and credential-store diagnostics.
- Receipt-owned uninstall that preserves configuration and refuses a modified or package-manager-owned binary.
- Stable initial exit categories: success `0`, general failure `1`, usage `2`, and required compatibility or connectivity failure `7`.

## Intended public installation

After an approved public distribution origin is available:

```bash
curl -fsSL https://<public-origin>/install.sh |
  BLAZN_VERSION=v0.1.0-poc.1 sh
```

`BLAZN_VERSION` is intentionally required for Milestone 1 so every install resolves an immutable release.

The installer defaults to `$HOME/.local/bin` and does not edit shell or application configuration. Override the destination with `BLAZN_INSTALL_DIR`.

## Release trust

The installer pins the Blazn POC Ed25519 release key:

```text
SHA256:/B552TYf50sxCpMS4R6hLAXoHI7vouJ39yM9BQjr5Dk
```

It verifies the OpenSSH signature on `SHA256SUMS`, requires exactly one checksum entry for the selected archive, validates the archive before extraction, verifies the archive checksum, runs the downloaded binary's version command, and installs the binary and receipt transactionally.

The installer fails closed for:

- Wrong signer or fingerprint.
- Invalid, missing, or modified signature.
- Duplicate or missing checksum entry.
- Tampered or truncated archive.
- Unexpected archive members or paths.
- Unsupported operating system or architecture.
- Missing verification tools.
- Downloaded binary version mismatch.

## Commands

```bash
blazn help
blazn version
blazn version --output=json
blazn doctor
blazn doctor --output=json
blazn uninstall --yes
```

`doctor` is offline-safe. Warnings such as an unavailable Linux desktop credential store do not make the standalone CLI unusable.

## Build and test

```bash
make ci
```

The CI target runs:

- Go formatting verification.
- Go unit tests.
- Cross-platform release packaging and signature tests.
- Installer security and rollback tests.

The release workflow first creates one final-version, commit-addressed candidate containing:

```text
blazn_<version>_darwin_arm64.tar.gz
blazn_<version>_linux_amd64.tar.gz
blazn_<version>_linux_arm64.tar.gz
SHA256SUMS
SHA256SUMS.sig
version.txt
```

Native qualification records the candidate workflow run ID, source SHA, version, and artifact digests. A separate `publish` dispatch on `main` references that exact successful candidate run. Its protected signing job performs no checkout and executes no candidate repository scripts: it recomputes the manifest over the downloaded candidate, signs it with the environment-scoped release key, publishes those exact bytes without rebuilding, and performs an authenticated post-promotion installer smoke.

## End-to-end qualification

The signed candidate was installed through a curl pipe into an isolated prefix, executed, reinstalled idempotently, diagnosed, uninstalled through its receipt, verified to preserve configuration, and reinstalled on:

- `ben1`
- `ben2`
- `ben3`
- `ben4`
- `mac-mini-1`
- `mac-mini-2`
- `mac-mini-3`
- `mac-mini-4`
- `mac-mini-5`
- `mac-mini-6`
- A separately named disposable Ubuntu Linux ARM64 Lima guest on `mac-mini-4`

The disposable guest was removed after qualification. The shared `frontro-agent-worker` VM was not modified.

## Remaining publication gate

`KingJammin/blazn` is currently private and `blazn.benpelo.com` is not yet published. The implementation and controlled signed-origin tests are complete, but an anonymous internet curl command requires one approved public channel:

- Public Blazn releases.
- A separate public distribution repository.
- Signed assets hosted at `blazn.benpelo.com` or another approved public origin.

Repository visibility and public DNS are not changed automatically by this milestone.
