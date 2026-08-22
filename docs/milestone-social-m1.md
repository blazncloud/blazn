# Blazn Social Milestone 1 integration

## Outcome

With only the root CLI installed, `blazn person search ...` identifies the
allowlisted Social plugin, offers its signed private installation, resumes the
original command, and returns deterministic provenance-backed entity results.

The root repository owns plugin trust and lifecycle. The separate private
`blazncloud/blazn-social` repository owns search, storage, providers, contact
classification, connection imports, and later publishing adapters.

## Root acceptance

- Catalog conflicts and unknown commands fail closed.
- Strict manifests and semantic compatibility are enforced.
- Tampered receipts, binaries, manifests, signatures, checksums, and archives
  are rejected.
- Installation and activation are atomic and rollbackable.
- Canonical and alias commands preserve stdin, stdout, stderr, JSON mode, and
  plugin exit status.
- Interactive yes/no, JSON/non-TTY behavior, and no-install help are tested.

## Cross-repository acceptance

The full gate additionally requires a signed `blazn-social` release, a clean
temporary home, the prompt/install/replay journey, synthetic 50-person and
50-company conformance, live GLEIF and SEC smoke tests, Hunter's documented
dummy-key flow, local connection import, cost refusal before provider calls,
encrypted contact storage, idempotent deletion, and a private live-provider
qualification corpus when Exa/Brave and Hunter credentials are supplied.
