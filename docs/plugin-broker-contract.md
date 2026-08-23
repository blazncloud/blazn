# Scoped plugin broker v1

The root `blazn` process owns authentication, Workspace and optional Project
selection, capability policy, API clients, and cancellation. A signed plugin
receives one inherited full-duplex local socket and never receives a bearer
token, refresh token, provider credential, object-store key, signed URL, or
credential-store path.

## Transport and framing

On Darwin and Linux the child endpoint is inherited as file descriptor 3 and
identified by `BLAZN_PLUGIN_BROKER_FD=3`. Root replaces any inherited value.
The descriptor is a Unix socket pair owned by the root process and plugin
child; it is not a filesystem socket and has no externally connectable name.

Every frame has a 16-byte big-endian header: magic `BZPB`, protocol version
`1`, frame type, flags, unsigned 32-bit stream ID, and unsigned 32-bit payload
length. Control payloads are UTF-8 JSON bounded to 1 MiB. Data payloads are
bounded to 1 MiB per frame, with at most 4,096 streams per invocation. Unknown versions, types, flags, duplicate stream
IDs, oversized frames, malformed JSON, or output after cancellation close the
connection and fail the plugin invocation.

Frame types are request `1`, response `2`, data `3`, end `4`, and cancel `5`.
Stream ID `0xffffffff` is reserved for root-owned invocation cancellation and
must never be allocated by a plugin request.
Metadata requests receive exactly one response on the same stream. Artifact
upload is the sole two-response exchange: `artifact.upload.begin` receives an
`artifact-upload-ready/v1` response before any bytes, then ordered data frames
and one empty end frame receive exactly one terminal `artifact-envelope/v1` or
failure response. Data sent before readiness, after the end frame, or on any
other method stream terminates the invocation. Root hashes and counts bytes
while streaming, atomically activates only an exact digest/size match, then
returns the typed Artifact envelope. Partial or cancelled uploads are removed
and never create a ready Artifact.

## Authority

Requests validate against
`packages/contracts/plugin-broker-request.schema.json`. They intentionally
contain no user, Workspace, Project, API-origin, placement, or credential
fields. Root binds those values from the authenticated invocation and checks
the plugin's allowlisted capabilities before every request and replay.

Initial Content capabilities are `project.read`, `run.read`, `run.create`,
`run.cancel`, `run.synthetic.execute`, `artifact.read`, and `artifact.write`.
Only `proofClass=synthetic` is accepted from a plugin. Local, Sandbox, and
provider placement and completion remain owned by their respective root
authorities. A plugin cannot upgrade proof by choosing a method or payload.

Responses validate against `plugin-broker-response.schema.json`. Successful
metadata is canonical JSON in a bounded string and declares one closed
`resultSchema`; the generated plugin SDK strictly decodes that payload into the
existing Project and Run/Artifact OpenAPI types with unknown fields rejected.
This prevents a response envelope from acquiring ambient authority fields.
Errors use stable code, message, retryability, and request ID fields; they never
return inaccessible resource details. Cancellation of the parent command
emits a cancel frame, cancels API work and streams, and closes the socket after
a bounded drain.

## Replay and evidence

Mutations require caller-generated idempotency keys. Root scopes them to the
authenticated principal, selected Workspace/Project, plugin, method, and exact
request digest. Progress sequence is monotonic. Synthetic completion must bind
the Run's proof class and plan digest and may reference only Artifacts uploaded
for that Run. Receipts never include broker descriptors, absolute temporary
paths, credentials, or internal object keys.
