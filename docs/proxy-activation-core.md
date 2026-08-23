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
values. Its per-record marker is the rollback authority, and an adapter may
restore a value only when both that marker and the digest of the live value
match the journal (compare-and-set). No sixth environment marker is published;
the environment contract remains exactly the five named variables. Receipts,
status, CLI results, routes, and events contain neither prior
values nor listener/provider credentials. The listener token stays only in the
listener and activated child/session environment; state stores its fingerprint.
Proxy command errors cross the CLI boundary through stable public messages;
adapter, runner, and policy-loader error text is never rendered directly.

## Recovery

`off` and confirmed `reset` call only the local protected store, environment
compare-and-set adapter, and exact process-identity controller. They never load
the policy or contact the Management API or a provider. Panics become
`RECOVERY_REQUIRED`; corrupt or ambiguous records leave user state untouched.
The exact listener proof binds PID, process start, executable, binary digest,
listener-key fingerprint, nonce, owner, generation, mode, and session.
The listener identity boundary supplies that complete authenticated proof, and
the activation service rejects any field mismatch rather than composing proof
metadata from its own caller state.

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
platform mutation. `process_environment` is a no-op publication mechanism used
only by `proxy run`; the five selected values are passed to that exact child and
are not written into the parent or OS session. `on` accepts only the journaled
`session` mode with the matching native publication mechanism. Linux or macOS
`auto` therefore fails `PROXY_SESSION_UNSUPPORTED` when the adapter cannot prove
that durable session boundary; it never writes a synthetic `scoped_only` or
other out-of-contract journal mode.

`EmbeddedListenerFactory` and the default unavailable CLI factory are core/test
boundaries, not production session implementations. Production `proxy on`
remains unavailable until a native adapter supplies a durable listener child,
an authenticated control channel, and restart-safe process proof. The core's
injected fake adapters exercise journal crash
points, stale state, API independence, abrupt listener loss, idempotency, exact
argv, compare-and-set restoration, and application-config non-mutation.

## Destination credential resolver core

`internal/proxy/credential` defines the platform-neutral boundary for the next
native slice. A platform backend implements only
`Lookup(context.Context, canonicalRef) ([]byte, error)`. Separate injected
backends serve `node-route://` and `workspace-vault://`; the core validates the
complete canonical reference and its destination-class/scheme pairing before
dispatch. It resolves every policy route, de-duplicates identical references,
and performs one concurrent lookup per unique reference.

Successful resolution produces an immutable listener-lifetime snapshot that
implements `router.CredentialProvider`. Request dispatch reads only that
snapshot, so it never contacts a platform store and cannot observe a partial or
rotated activation. Backend buffers are copied after validation and then
best-effort zeroed. Empty values, values over 4096 bytes, and values containing
CR, LF, or NUL are rejected. Errors carry a stable typed failure class while
their text and Go formatting omit backend errors, references, and values.

The resolver is opt-in on `EmbeddedListenerFactory`, the existing core/test
construction boundary. It completes before router preflight and before the
loopback socket is bound; consequently a credential failure returns activation
exit code 3 with no listener identity, journal, or environment publication.
There is deliberately no production default and no Keychain, Secret Service,
configuration, CA, proxy, or OS mutation in this slice. Native adapters must be
added and qualified separately.
