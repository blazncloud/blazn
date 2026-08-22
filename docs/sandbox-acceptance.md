# Phase 5 contract-freeze acceptance

Run only the contract lane for this PR:

```bash
make check-sandbox-generated
make test-sandbox-contract
make test-sandbox-postgres
```

`test-sandbox-contract` parses every JSON document and fixture with `jq`, verifies the frozen operations, shared error envelope, policy constants, architecture uniqueness, pinned image digests, confined paths, CLI envelopes and exit codes, regenerated-client equality, RFC 8785 digest vector, grant-token transport, and the required non-sensitive acknowledgement.

`test-sandbox-postgres` creates a disposable PostgreSQL 17 database, installs migrations through the normal migrator, reapplies the migrator to prove checksum/idempotent behavior, and checks:

- all nine migrations are recorded;
- an exact same-workspace template/version/sandbox/grant graph succeeds;
- the runtime role cannot update/delete immutable template versions;
- a cross-workspace version binding fails its composite foreign key;
- recursive secret-bearing JSON is rejected;
- the same principal/operation/idempotency key cannot bind a different request;
- grant storage contains a hash, not a bearer value; and
- bootstrap and node-broker roles have no sandbox-table privileges.

The generated-client and PostgreSQL tests are intentionally targeted. This PR does not authorize full infrastructure, release, Compose, Kubernetes, or live-host mutation.

Subsequent implementation acceptance must additionally prove five complete AMD64 and five complete ARM64 lifecycles, visible Kueue admission, single-use expiring grants, artifact export, direct and claim cleanup, bounded Blazn-only drain behavior, and an explicit statement that the POC RuntimeClass boundary is not hardened.
