# Proxy activation and recovery core

This slice wires the authenticated loopback listener into the standalone
`blazn proxy` command surface and the protected proxy state store. It defines
the platform-neutral boundaries used by the next Linux and macOS adapter PRs;
it does not install a CA, edit provider/application configuration, or implement
launchctl or user-systemd publication.

The public commands are:

```text
blazn proxy on --policy POLICY [--mode auto|session]
blazn proxy off [--remove-ca]
blazn proxy status
blazn proxy doctor --policy POLICY
blazn proxy routes --policy POLICY
blazn proxy tail [--cursor CURSOR] [--follow]
blazn proxy run --policy POLICY -- COMMAND...
blazn proxy reset --yes [--remove-ca]
```

`run` passes exact argv without a shell. JSON Lines are accepted only by
`tail`. Route output excludes credential references. Tail accepts only bounded
fixed operational event fields; arbitrary content cannot enter its output.
`--remove-ca` is reserved but returns `PROXY_CA_REMOVAL_UNSUPPORTED` in this
core lane; it never reports success until a later platform adapter implements
receipted trust removal.

## Activation transaction

The core reserves the per-user lifecycle fence, performs policy/listener
preflight outside the held lock, snapshots exactly the five documented
variables, and then reacquires the fence. It writes and fsyncs the protected
`prepared` journal before publication, advances through `publishing`, and only
then writes the bound `active` journal and redundant receipt. A publication
error or recovered panic leaves `recovery_required` evidence. A crash after the
write-ahead journal remains recoverable after the short reservation expires.

The journal contains exact prior values but only digests and markers for new
values. Receipts, status, CLI results, routes, and events contain neither prior
values nor listener/provider credentials. The listener token stays only in the
listener and activated child/session environment; state stores its fingerprint.

## Recovery

`off` and confirmed `reset` call only the local protected store, environment
compare-and-set adapter, and exact process-identity controller. They never load
the policy or contact the Management API or a provider. Panics become
`RECOVERY_REQUIRED`; corrupt or ambiguous records leave user state untouched.
The exact listener proof binds PID, process start, executable, binary digest,
listener-key fingerprint, nonce, owner, generation, mode, and session.

In-process scoped listeners are stopped through their runtime handle rather
than signaling the CLI PID. Unknown or post-crash processes are delegated to
the platform identity controller. Environment restoration changes only values
whose digest and activation marker still match; direct connectivity and user
changes win conflicts.

## Deferred platform qualification

Real launchctl and user-systemd publication, OS process inspection after a
restart, platform credential stores, signal-forwarding runners, and the native
twenty-cycle/crash/reboot/config-snapshot matrix remain in the next PRs. Until
one of those adapters is selected, the root CLI fails unavailable before any
platform mutation. The core's injected fake adapters exercise journal crash
points, stale state, API independence, abrupt listener loss, idempotency, exact
argv, compare-and-set restoration, and application-config non-mutation.
