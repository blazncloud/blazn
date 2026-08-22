# Proxy and LLM Router Contract Freeze

**Contract:** `proxy/v1alpha1`
**Scope:** process/session endpoint activation, authenticated loopback listener, OpenAI/Anthropic subsets, deterministic policy and one bounded fallback

## Narrow POC gate

1. One standalone `blazn` binary starts an authenticated loopback listener.
2. `proxy run -- COMMAND...` injects endpoint variables without a shell or any
   application/shell config edit. Qualified macOS/Linux session activation
   applies only to newly launched applications.
3. The listener accepts OpenAI Responses, OpenAI Chat Completions, and
   Anthropic Messages, routes one alias to local Qwen, and permits at most one
   explicitly authorized cloud fallback before first byte.
4. `proxy off` restores exact prior state with no daemon, provider, or
   Management API dependency.
5. Twenty on/off cycles per required platform, crash/reboot recovery,
   streaming/cancellation/fallback, and byte-for-byte application-config
   snapshots pass without secret or content leakage.

The POC uses `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `ANTHROPIC_BASE_URL`,
`ANTHROPIC_API_KEY`, and `ANTHROPIC_AUTH_TOKEN`. Both Anthropic credential
variables are replaced with the per-activation listener credential so Claude
Code cannot bypass the listener through its gateway-token precedence. All
prior values are snapshotted and compare-and-set restored. API-key/token values are per-activation listener credentials,
never provider credentials. It does not modify `HTTP_PROXY`, `HTTPS_PROXY`,
shell profiles, provider configs, or system trust/CA state. Unsupported clients
are reported `BYPASS/UNSUPPORTED`.

## CLI

```text
blazn proxy on --policy POLICY [--mode auto|session]
blazn proxy off [--remove-ca]
blazn proxy status
blazn proxy doctor --policy POLICY
blazn proxy routes --policy POLICY
blazn proxy run --policy POLICY -- COMMAND...
blazn proxy tail [--cursor CURSOR] [--follow]
blazn proxy reset --yes [--remove-ca]
```

`scoped` is intentionally not a durable `on` mode; scoped activation exists
only for `proxy run`. Same-policy activation is idempotent. A different active digest is exit `6`
and requires `off`. `run` executes exact argv, forwards signals/exit status,
uses scoped state, and restores automatically. `off` is idempotent and
daemon-independent. `reset` removes only receipt-proven Blazn state; ambiguity
returns exit `9` and recovery instructions.

Exit codes:

- `0`: success/idempotent/doctor warning.
- `1`: unexpected failure.
- `2`: invalid flags, schema, or requested capability.
- `3`: listener/destination credential unavailable.
- `4`: policy or data-boundary denial.
- `5`: policy, alias, or route not found.
- `6`: lifecycle/policy conflict.
- `7`: listener/provider/platform unavailable.
- `8`: lifecycle/route deadline.
- `9`: partial restoration or `RECOVERY_REQUIRED`.

Stable errors include `PROXY_ALREADY_ACTIVE_DIFFERENT_POLICY`,
`PROXY_SESSION_UNSUPPORTED`, `POLICY_INVALID`, `NO_COMPLIANT_ROUTE`,
`CREDENTIAL_UNAVAILABLE`, `LISTENER_UNHEALTHY`, `FALLBACK_NOT_PERMITTED`,
`FALLBACK_AFTER_STREAM_FORBIDDEN`, `STATE_OWNERSHIP_AMBIGUOUS`, and
`RECOVERY_REQUIRED`.

## JSON and event boundary

Every command result includes `command`, `contractVersion`, `status`, `state`,
and `timestamp`. Activation/status results expose listener health/process
identity, policy digest, protocol names, and published variable names only.
They never expose values, prior state, provider credentials, or content.

`tail` uses JSONL matching
[`event.schema.json`](../packages/contracts/proxy/event.schema.json). Events
contain operational route/outcome/latency/usage metadata and never headers,
cookies, prompts, messages, tools, response content, or credentials.

## Transactional state

State is anchored to the OS account identity, ignoring invocation-controlled
HOME/XDG:

- macOS: `Library/Application Support/Blazn/proxy`.
- Linux: `.local/share/blazn/proxy`.

Directories are owner `0700`; files/keys/locks are real, single-link `0600`.
One per-user lifecycle flock plus a short reservation serializes changes.
Network/provider checks run outside the lock. Before mutation, the command
reacquires it, verifies the nonce, and performs write/fsync/rename/parent-fsync.

The frozen journal and redundant receipt schemas are
[`activation-journal.schema.json`](../packages/contracts/proxy/activation-journal.schema.json)
and [`activation-receipt.schema.json`](../packages/contracts/proxy/activation-receipt.schema.json).
It records exact prior variable values only in the protected journal. Receipts
contain names/digests, never prior values or provider secrets.

Checksums are `sha256:` plus lowercase SHA-256 over RFC 8785 canonical JSON of
the complete object with `checksum` omitted. The receipt binds the same
activation generation, platform, mode, OS-session identity, journal digest,
policy digest, listener process identity, and published environment digests.

Restoration is compare-and-set: restore only values still carrying the recorded
marker/digest. PID termination additionally requires process-start identity,
executable identity, binary digest, listener-key fingerprint, activation nonce,
owner UID, generation, mode, and OS-session identity. If the journal is valid,
exact prior values may be restored and a missing/corrupt receipt repaired. If
the protected journal is corrupt or missing, the receipt can prove and stop the
listener but cannot reconstruct prior environment values: it leaves the
environment untouched, records `RECOVERY_REQUIRED`, and returns exit `9` with
exact manual remediation. A receipt never contains prior values. If both
records are corrupt or ownership is ambiguous, no environment or process state
is changed.

## Listener and protocols

Authenticated loopback endpoints:

```text
POST /v1/responses
POST /v1/chat/completions
GET  /v1/models
POST /v1/messages
GET  /healthz
```

`/healthz` is the only unauthenticated endpoint and exposes only an aggregate
ready/not-ready result. `/v1/models` and every inference endpoint require the
activation listener credential. The listener accepts the appropriate
`Authorization` or `x-api-key` source header, validates it in constant time,
then removes every source credential header before upstream dispatch.

Independent protocol adapters exchange only the frozen
[`normalized-request`](../packages/contracts/proxy/normalized-request.schema.json),
[`normalized-response`](../packages/contracts/proxy/normalized-response.schema.json),
[`normalized-stream-event`](../packages/contracts/proxy/normalized-stream-event.schema.json),
and [`normalized-error`](../packages/contracts/proxy/normalized-error.schema.json)
shapes. A translation must reject a source feature that cannot be represented
without loss; it may never silently omit it.

Supported POC subset:

- OpenAI Chat: text roles, tools/tool choice, JSON schema response format,
  streaming, usage, cancellation.
- OpenAI Responses: text/instructions, function calls/results, structured
  output, streaming order, usage, cancellation.
- Anthropic Messages: system/text blocks, tool_use/tool_result, tools/choice,
  sampling/stops, streaming order, stop reason, usage.

Reject multimodal input, beta/prompt-caching headers, computer use, extended
thinking, batches, WebSockets, embeddings, and unsupported tool/JSON semantics
before upstream. Never silently drop fields. No fallback occurs after any
response byte/event or after tool side effects. The harness executes tools.

## Router policy

The JSON Schema is
[`policy.schema.json`](../packages/contracts/proxy/policy.schema.json).
The POC freezes one local Qwen primary route plus one authorized cloud route,
maximum two attempts. Fallback is allowed only before first byte for connection
failure, pre-first-byte timeout, 429, 5xx, unavailable model, or compatible
context overflow. Local-to-external fallback requires an explicit policy bit
and compatible data boundary. Each alias declares the request data class and
allowed destination boundaries; every route declares accepted data classes.
Fallback must be present in the exact allowed-boundary transition list, and
`local_only` may never leave `local`. Cycles, missing credentials, incompatible
protocols/capabilities, disallowed hosts, budgets, and boundary contradictions
fail validation before activation.

Route endpoints are decomposed into scheme, exact host, port, and base path.
Local routes resolve only to loopback or an authenticated node tunnel. External
provider/cloud routes require HTTPS, an exact workspace-policy hostname
allowlist match, and public-unicast resolution. The implementation resolves and
validates every address before connect and again for redirects/retries, rejects
link-local/private/loopback/metadata destinations for external routes, pins the
validated address for that connection, preserves the validated Host/SNI name,
and does not follow a redirect outside the route allowlist.

## Credentials

- Listener credential: random, activation-local, journal-protected, injected
  into source API-key variables, stripped before upstream.
- Destination credentials: references to the merged OS credential backend or
  short workspace-vault leases; injected only into the selected adapter.
- Management API/session credentials never enter listener state, child env,
  routes, or logs.
- Prior application API keys are restoration-only and never reused upstream.

## Platform gate

- macOS ARM64: scoped run; qualified launchctl user-session publication for new
  apps; default Keychain destination storage.
- Ubuntu AMD64/ARM64: scoped `proxy run` is the POC requirement. `on --mode
  session` fails `PROXY_SESSION_UNSUPPORTED`; `auto` starts a `scoped_only`
  listener unless doctor proves a user-systemd environment inherited by newly
  launched applications.
- Required exact fixtures: generic OpenAI fixture `proxy-fixture/v1`, Hermes
  Agent `0.19.0`, Codex CLI `0.147.0` Responses including a nested child, and
  Claude Code `2.1.212` Anthropic Messages. A version mismatch is unsupported
  until its endpoint-variable behavior passes the same fixture.
- Windows is deferred and must report unsupported without mutation.

The POC policy is always supplied as an explicit local `--policy` file. The
file is a real owner-only file whose RFC 8785 digest is verified before network
activity. Dynamic Management API retrieval, cache refresh, and hot policy swap
are deferred; `off` and recovery never read the policy file or use the network.

The qualification policy uses logical alias `company-assistant`, local primary
model `qwen3.8` over OpenAI Chat at `http://127.0.0.1:11434/v1` (or the same
loopback endpoint established by an authenticated node tunnel), and one cloud
fallback `gpt-5.4` over OpenAI Responses at
`https://api.openai.com:443/v1`. The only external hostname allowlisted is
`api.openai.com`; its destination credential reference is
`workspace-vault://poc/model-providers/openai`. Local-to-external fallback is
enabled only for `public` and `company` data. `restricted` and `local_only`
remain local. Qualification fails rather than substituting another model,
endpoint, protocol, or credential.

## Required tests

- Policy selection/fallback/data-boundary golden matrices.
- Mock local/cloud Chat, Responses, and Messages streaming/tools/cancellation.
- Translation goldens and unsupported-field rejection.
- No fallback after first byte; one logical request ID across attempts.
- Listener-token stripping and destination credential injection/redaction.
- Fault injection after every state write/fsync/rename/publication step.
- Corrupt journal/receipt/both, SIGKILL staging, PID reuse, executable mismatch,
  concurrent lifecycle, and compare-and-set restoration.
- Exact argv/signal/exit behavior without shell interpolation.
- Application/shell config write monitoring and pre/post digests.
- Twenty native cycles plus crash/reboot and unsupported-client reporting.

## PR split

1. Contract/schema/help/fixtures.
2. State/journal/receipt/recovery on ben2.
3. Router/OpenAI listener on ben3.
4. Anthropic normalization/translation on isolated ben4.
5. macOS and Linux platform adapters in parallel.
6. Integration/scoped review.
7. Serialized per-user qualification/evidence.

Shared state schema, normalized envelope, errors, and policy schema remain
integration-owner serialization gates. Per-host proxy mutation is exclusive.

## Qualification inputs

The exact model routes, harness versions, policy source, and platform behavior
above are frozen. Before native activation, the qualification operator records
one sacrificial macOS user session and one disposable Linux user, snapshots the
five published environment variables plus known Codex/Claude/Hermes config
trees, and reserves the per-user proxy mutation lock. Existing Blaze Proxy code
is not copied into the POC unless a later provenance review approves specific
files; behavior is reimplemented against these schemas and fixtures.

## Deferred hardening

Transparent CONNECT/CA interception, LAN/system-wide listeners, Windows,
WebSockets/multimodal/batch/embeddings, broad translations, dynamic queues,
learned routing, content capture, billing, UI, hot policy swap, and arbitrary
desktop-app transparency are outside the POC.
