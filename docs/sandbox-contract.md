# Phase 5 sandbox contract freeze

This change freezes the POC contract before server, controller, broker, CLI wiring, or deployment work begins. It does not enable a route or claim production-grade isolation. The fixed notice is: **POC orchestration isolation only; approved non-sensitive workloads only.**

## Contract identity

- `packages/contracts/sandbox-template.schema.json` defines `blazn.dev/v1alpha1` `SandboxTemplate` manifests. The content digest is `sha256:` plus the lowercase SHA-256 of RFC 8785/JCS canonical UTF-8 bytes for the fully resolved `spec` object only.
- `packages/contracts/sandboxes.openapi.json` defines the authenticated template, immutable version, sandbox, operation, event, access-grant, exec, and single-file transfer surfaces.
- `packages/contracts/sandbox-cli-contract.json` freezes commands, JSON/NDJSON envelopes, exit codes, and the rule that access tokens never enter argv or CLI output.
- `internal/client/sandbox.gen.go` is pinned to the exact bytes of all three documents. Regeneration fails if contract bytes change without an intentional generator update.

Template name and version pairs and template content digests are unique within a template. Variants have unique architectures. Images are an OCI index digest plus the selected architecture's child digest; mutable tags are invalid. A repository has an approved HTTPS identity and a destination confined beneath `/workspace/src`; its exact commit is mandatory on sandbox creation. Artifacts are confined beneath `/workspace/artifacts`.

The schema exposes no raw Pod, volume, environment, secret, service-account, node-selector, toleration, RuntimeClass, hostPath, privilege, added-capability, or unrestricted-egress input. `poc-restricted-v1`, `default-deny-v1`, and the non-sensitive isolation acknowledgement are fixed policy identities, not caller-selected Kubernetes settings.

## Lifecycle and durable identity

Creation atomically binds the template ID, immutable version ID and human version, template digest, variant, OCI index digest, architecture child digest, architecture, allocation backend (`direct` or `claim`), queue, admission, artifact contract, source commits, isolation acknowledgement, and expiry. A response never includes backend credentials.

Sandbox observed states are `requested`, `queued`, `provisioning`, `ready`, `running`, `stopping`, `stopped`, `deleting`, `deleted`, and `failed`; desired states are `ready`, `stopped`, and `deleted`. Durable operations are `pending`, `running`, `succeeded`, `failed`, or `recovery_required`. Events use a monotonic sequence and reconnect through `Last-Event-ID`.

Stop revokes every grant, exports the declared artifacts, destroys the direct Sandbox or claim backend, and retains the sandbox record. Delete performs stop first for a live backend and then tombstones the record. A terminal operation receipt records cleanup, grant revocation, artifact export, and backend destruction rather than treating a client disconnect as cancellation.

## Authorization and access grants

Active members may read published templates. Owners and administrators may mutate drafts and publish immutable versions. Owners, administrators, and operators may create, stop, or delete sandboxes. Sandbox inspection and access are limited to the requester, or an owner/administrator using an audited override. Members and viewers cannot create or control sandboxes. Inaccessible and cross-workspace identities are returned as `404`.

The runner service account is tokenless and orchestration-only; private broker credentials are never returned. Each grant binds workspace, sandbox, user, session, kind, token-key identity, and an expiry of at most 60 seconds. Only a token hash is stored. The token is returned once under `Cache-Control: no-store` and sent only as `Authorization: Blazn-Grant <token>` to the same origin. It is atomically consumed before attach or file access; replay, expiry, and revocation return `410`.

Exec accepts 1–32 bounded argv entries and returns base64 stdout/stderr, the remote exit code, and truncation. Upload/download handles one regular file, follows no symlinks, is confined beneath an approved workspace subtree, verifies SHA-256, and is limited to 8 MiB.

## Persistence boundary

Migration `009_sandboxes.sql` is additive and forward-only. Immutable canonical version bytes and specs live separately from mutable publication status. Every child relation carries workspace scope and uses composite foreign keys so a UUID cannot cross a tenant boundary. Idempotency keys bind principal and operation to a request digest. Recursive secret-key rejection applies to stored JSON. Runtime privileges are explicit; immutable versions have no runtime update/delete grant, while bootstrap and node-broker roles receive no sandbox-table privilege.

The schema's `x-blazn-invariants` records two constraints JSON Schema 2020-12 cannot express portably: unique variant architectures and digest uniqueness within persisted template identity. The contract test and PostgreSQL constraints enforce those boundaries. Runtime schema validation and authorization are implementation work for the next independently reviewed PR.
