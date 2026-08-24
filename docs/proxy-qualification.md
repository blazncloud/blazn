# Proxy qualification evidence

`infra/proxy/qualification` freezes the Phase 6A serialized per-user
qualification matrix and its evidence rules. This slice is static: the only
executable adapter is an in-memory/file-backed fake used by unit tests. It does
not call D-Bus, systemd, launchd, the Blazn proxy lifecycle, a credential store,
a CA API, a model provider, or a reboot command.

The Linux profile names `systemd_user_environment` as the candidate publication
mechanism. Its checked-in template has `mutationEnabled: false` and
`pending_native_qualification`; it cannot run a native cycle. The macOS template
is `unsupported_until_launchd` and validation rejects any attempt to enable it.
That result is intentional: macOS must remain fail-closed until a descriptor-safe
listener child and reviewed launchd user-session adapter exist.

## Commands and default behavior

The command wrappers are:

```text
infra/proxy/qualification/preflight.sh
infra/proxy/qualification/plan.sh
infra/proxy/qualification/capture-before.sh
infra/proxy/qualification/cycle.sh
infra/proxy/qualification/recovery.sh
infra/proxy/qualification/route-proof.sh
infra/proxy/qualification/verify.sh
infra/proxy/qualification/cleanup.sh
```

All state-affecting/evidence-recording commands default to a JSON plan and set
`wouldExecute` to false. `preflight` and `plan` only validate and describe the
profile. `verify` is read-only. No command silently changes its mode because a
lock, D-Bus session, binary, or supported-looking host happens to exist.

Every executable receipt is bound to:

- canonical source commit and tree;
- exact candidate binary and policy SHA-256 digests;
- exact host, user, and login-session identifiers;
- one `proxyqual-*` correlation identifier;
- a profile digest;
- the inode identities of both the coordinator lock and local host/user lock;
- the exact action, cycle number, fault case, client, and route decision.

The local lock basename is derived from `SHA256(hostId + "\n" + userId)` and
prevents two coordinators from targeting one host/user session. Both lock files
must be pre-created regular, non-symlink files, mode `0600`, `0640`, or `0644`,
and not writable by group or other. Both are acquired non-blocking for each
receipt. The coordinator lock is a scheduling fence; panic-safe `proxy off`
continues to rely on the product's own per-user activation lock rather than this
evidence lock.

## Frozen evidence matrix

`capture-before` records only digests and safe booleans. It snapshots exactly:

1. `OPENAI_BASE_URL`
2. `OPENAI_API_KEY`
3. `ANTHROPIC_BASE_URL`
4. `ANTHROPIC_API_KEY`
5. `ANTHROPIC_AUTH_TOKEN`

It also records cryptographic tree sentinels for the approved Codex, Claude, and
Hermes configuration roots, an authenticated direct-connectivity proof digest,
and a digest of owner state. Values and file contents never enter evidence.
Credential stores are outside the snapshot; native qualification may observe
safe metadata or controlled fixtures but must not export their contents.

The exact twenty-cycle matrix includes normal stop, abrupt kill, reboot, journal
corruption, manager outage, partial publication, ambiguous recovery, repeated
on/off, receipt/both-record corruption, stale PID reuse, missing CA, and
Management API outage. Each cycle must prove byte-identical client sentinels,
exact-five compare-and-set restoration, direct connectivity, and zero listener
or Blazn-owned state residue. Zero-residue receipts are derived from explicit,
available listener and owned-state observations bound to the exact activation,
login session, and platform account state root; an unavailable, malformed, or
positive observation fails the action and cannot be finalized. Ambiguous ownership must emit
`RECOVERY_REQUIRED`, report `userStateChanged: false`, and leave user state
untouched.

Route evidence requires authenticated `ROUTED`, `DIRECT`, and `BYPASS` receipts
for Hermes Agent `0.19.0`, Codex CLI `0.147.0`, and `proxy-fixture/v1`. A tunnel,
endpoint override, or process launch is not route proof. Claude Code `2.1.212`
is recorded once as `UNSUPPORTED` for `anthropic-native` with reason
`native_protocol_unsupported`; the harness does not turn a checked fixture shape
into a native-protocol claim.

Finalization refuses missing or duplicate cycles, incomplete route decisions,
config drift, CAS conflicts, lost direct connectivity, residue, unauthenticated
proofs, dishonest Claude claims, or artifacts containing prompt/message/tool
payload, token, cookie, listener credential, or private-key fields. Each receipt
has a checksum; the finalized manifest has a digest and a deterministic
`SHA256SUMS` covering every JSON artifact.

## Native approval gates

Native work remains prohibited until all of these gates are satisfied in one
immutable profile:

1. A reviewed Linux systemd native adapter replaces the static harness refusal
   and proves newly launched applications inherit the exact-five environment.
2. A sacrificial Linux host/user/session is reserved exclusively, with exact
   stable host, UID, and login-session identities.
3. The candidate source commit/tree, binary, POC policy, and their digests are
   fixed; the canonical worktree is clean.
4. `nativeApproval` contains a non-empty ticket, approver, approval/expiry
   timestamps, exact source/binary/policy/owner bindings, and scope
   `phase6a-native-proxy-qualification`; the profile is reviewed with
   `supportStatus: approved_candidate` and `mutationEnabled: true`.
5. The exact coordinator and host/user lock files are created safely and their
   inode identities are captured before approval.
6. An operator records the direct-connectivity and exact-five/config/owner
   baseline before any activation.
7. For each action, the operator sets
   `BLAZN_PROXY_QUALIFICATION_MODE=mutate`, an exact `proxyqual-*` correlation,
   and the full action-bound approval string printed by the refusal. Approval is
   not reusable across actions, cycles, faults, clients, decisions, profiles,
   artifacts, sessions, or replaced lock files.
8. Reboot, daemon kill, systemd-manager outage, and corruption cases receive
   separate destructive authorization and an external recovery path. Neither a
   shared workstation session nor the current control-plane Mac is eligible.
9. Cleanup runs under the same identities and proves compare-and-set direct
   restoration plus zero residue before the host/user reservation is released.
10. The checksummed evidence finalizes and independently verifies before Linux
    session activation is advertised as supported.

macOS additionally requires a reviewed launchd publisher/restorer and native
listener process adapter before any of these gates can be attempted. Until
then, it has no native approval path.

## Static verification

Run only on a Linux verification lane:

```text
make test-proxy-qualification-static
```

The test suite uses fake adapters and temporary files. It covers strict schema
and profile validation, plan-by-default behavior, malicious path/correlation
and lock inputs, lock contention, exact-five snapshots, the frozen matrix,
route-proof completeness, config mutation, CAS/residue cleanup failures,
redaction, checksums, and rehashed evidence tampering.
