# Phase 5 contract-freeze acceptance

Run only the contract lane for this PR:

```bash
make check-sandbox-generated
make test-sandbox-contract
make test-sandbox-postgres
```

`test-sandbox-contract` parses every JSON document and fixture with `jq`, performs actual Draft 2020-12 instance validation and dereferenceable OpenAPI 3.1 validation, and verifies the frozen operations, shared error envelope, isolated forbidden inputs, dot-segment rejection, architecture/name/path uniqueness, exact source coverage, pinned image digests, typed CLI envelopes and exit precedence, regenerated-client field parity, RFC 8785 digest vectors, grant-token transport, chunked raw transfer verification, and the required non-sensitive acknowledgement.

`test-sandbox-postgres` creates a disposable PostgreSQL 17 database, installs migrations through the normal migrator, reapplies the migrator to prove checksum/idempotent behavior, and checks:

- all nine migrations are recorded;
- an exact same-workspace template/version/sandbox/grant graph succeeds;
- the runtime role cannot update/delete immutable template versions;
- a cross-workspace version binding fails its composite foreign key;
- recursive secret-bearing JSON is rejected;
- normalized variant, repository, artifact, source, and artifact-contract identities are complete and unique;
- trusted publish/create entrypoints reject canonical byte/spec/digest and artifact-contract mismatches while direct runtime inserts fail;
- access grants can only move once from active to consumed, expired, or revoked through atomic functions;
- terminal receipts cannot succeed until cleanup, artifact export, grant revocation, and backend destruction are all true;
- event sequence is unique across the whole sandbox rather than per operation;
- the same principal/operation/idempotency key cannot bind a different request;
- grant storage contains a hash, not a bearer value; and
- bootstrap and node-broker roles have no sandbox-table privileges.

The generated-client and PostgreSQL tests are intentionally targeted. This PR does not authorize full infrastructure, release, Compose, Kubernetes, or live-host mutation.

Subsequent implementation acceptance must additionally prove five complete AMD64 and five complete ARM64 lifecycles, visible Kueue admission, single-use expiring grants, artifact export, direct and claim cleanup, bounded Blazn-only drain behavior, and an explicit statement that the POC RuntimeClass boundary is not hardened.
