# Milestone 2A — Workspace Contract Freeze

**Status:** Contract candidate  
**Scope:** Workspaces, memberships, one-time invitations, fixed roles, local selection, and workspace event revocation

## POC acceptance gate

Milestone 2A is accepted when two distinct authenticated users complete this
flow against the deployed control plane:

1. User A creates a workspace and becomes its owner.
2. User A creates a one-time member invitation.
3. User B accepts the invitation through stdin and selects the workspace.
4. Both users can list/get the workspace and its members.
5. User B is denied workspace-management actions.
6. User A removes User B.
7. User B immediately loses workspace REST and SSE access.
8. A second workspace does not leak through lists, lookup, events, member or
   invitation counts, cursors, or distinguishable not-found responses.

Nodes, projects, agents, artifacts, organizations, custom roles, workspace
deletion, and ownership transfer are outside this milestone.

The live gate requires a second root-provisioned local identity. Provisioning
is a one-shot administrative operation through `blazn_bootstrap`; it is not a
public API and must be idempotent for an exact login/display name.

## Fixed role policy

| Capability | Owner | Administrator | Operator | Member | Viewer |
| --- | --- | --- | --- | --- | --- |
| Read workspace and members | yes | yes | yes | yes | yes |
| Edit workspace | yes | yes | no | no | no |
| Create/revoke invitations | yes | yes | no | no | no |
| Remove members | yes | yes, except owner | no | no | no |
| Change non-owner roles | yes | yes | no | no | no |
| Future node/run operations | yes | yes | yes | bounded | no |

The last active owner cannot leave or be removed. Ownership transfer is
deferred. Roles are stored as constrained text rather than a PostgreSQL enum.

## API contract

The frozen OpenAPI overlay is
[`packages/contracts/workspaces.openapi.json`](../packages/contracts/workspaces.openapi.json).
The surface is:

```text
POST   /v1/workspaces
GET    /v1/workspaces
GET    /v1/workspaces/{workspaceId}
PATCH  /v1/workspaces/{workspaceId}
POST   /v1/workspaces/{workspaceId}/invitations
GET    /v1/workspaces/{workspaceId}/invitations
DELETE /v1/workspaces/{workspaceId}/invitations/{invitationId}
POST   /v1/workspace-invitations/accept
GET    /v1/workspaces/{workspaceId}/members
PATCH  /v1/workspaces/{workspaceId}/members/{userId}
DELETE /v1/workspaces/{workspaceId}/members/{userId}
DELETE /v1/workspaces/{workspaceId}/membership
GET    /v1/workspaces/{workspaceId}/events
```

Rules:

- Workspace authority is re-evaluated on every request and SSE reconnect/poll.
- Local workspace selection is an untrusted selector, never authorization.
- Creates and mutations require `Idempotency-Key`; versioned updates also
  require `expectedVersion` in the request body.
- Lists return `{items,nextCursor}`, including the single-page POC.
- Inaccessible and nonexistent workspaces use the same `workspace_not_found`
  response.
- Invitation tokens are stored only as SHA-256 hashes and never appear in
  list/get/audit/idempotency records. Create retries deterministically rederive
  the same token using HMAC-SHA-256 and the root-owned 32-byte key stored as
  64 lowercase hex characters at
  `/etc/blazn/control-plane/secrets/workspace-invitation-hmac-v1`.
- The invitation key ID is `workspace-invitation-hmac/v1`. The canonical HMAC
  input is UTF-8
  `blazn-workspace-invite-v1\n<workspace-uuid>\n<invitation-uuid>\n<idempotency-key>`;
  UUIDs use lowercase canonical form. The token is the unpadded base64url HMAC
  output and `token_hash` is its lowercase SHA-256 hex digest. Rotation requires
  retaining the old key until every invitation using that key ID is terminal.
- Invitation acceptance, membership insertion, invitation consumption, and
  audit insertion are one locked transaction.
- Idempotency receipts bind principal, workspace, operation, target identity,
  key, and request digest. Membership authorization is re-evaluated before a
  stored response is replayed; removal makes prior receipts inaccessible.
- Member removal closes active workspace streams and rejects reconnect without
  revoking the user's global device session.

## CLI contract

```text
blazn workspace create NAME [--slug SLUG]
blazn workspace list
blazn workspace get [WORKSPACE]
blazn workspace edit WORKSPACE --name NAME --expected-version VERSION
blazn workspace use WORKSPACE
blazn workspace invite [WORKSPACE] --role ROLE [--expires-in DURATION]
blazn workspace invitations [WORKSPACE]
blazn workspace revoke-invite INVITATION --expected-version VERSION
blazn workspace join --invite-stdin
blazn workspace members [WORKSPACE]
blazn workspace set-role USER --role ROLE --expected-version VERSION
blazn workspace remove-member USER --expected-version VERSION
blazn workspace leave
```

Invitation secrets are accepted only through a hidden prompt or stdin, never a
normal argument. The selected workspace is nonsecret local state, written
atomically to an owner-only file. `--workspace` overrides selection.

Representative JSON envelopes:

```json
{"workspace":{"id":"uuid","slug":"acme","name":"Acme","status":"active","version":1,"currentUserRole":"owner","createdAt":"timestamp","updatedAt":"timestamp"}}
```

```json
{"items":[],"nextCursor":null}
```

```json
{"invitation":{"id":"uuid","workspaceId":"uuid","role":"member","status":"pending","version":1,"createdAt":"timestamp","expiresAt":"timestamp"},"inviteToken":"one-time-secret"}
```

Exit behavior remains `0` success, `1` domain/API denial, `2` usage or missing
local context, and `7` unavailable API/auth store/network.

## Persistence and trust boundary

Migration `003_workspaces.sql` adds workspaces, memberships, invitations,
idempotency receipts, and redacted audit events. UUIDs avoid sequence grants.
`blazn_runtime` receives explicit operations only; `blazn_bootstrap` receives
no workspace-table access. A recursive JSONB guard rejects token, authorization,
password, secret, and credential key namespaces at every nesting depth.

The session proves identity. An active membership authorizes workspace access.
Invitation tokens are bounded bearer capabilities for one workspace, role,
expiry, and use. Role metadata never substitutes for server-side policy.

## Required evidence

- Contract/client generation drift and closed-schema validation.
- Five-role policy matrix and last-owner tests.
- Invitation hash, expiry, one-use race, revoke, replay, and redaction tests.
- Concurrent acceptance creates exactly one active membership.
- Idempotency conflicts on the same key with a changed request digest.
- Cross-workspace list, lookup, count, cursor, and SSE isolation.
- Unsafe local context-file rejection and stdin-only join.
- Migration, restart, final backup, and isolated restore.
- Two-user live invite/join/deny/remove/REST/SSE proof with no token in evidence.

## PR and concurrency split

1. Contract/schema/policy/generated client — serialized owner: root.
2. Workspace API/persistence/SSE — ben3; concurrency/isolation tests on ben4.
3. Workspace CLI/context/stdin — ben2; platform tests on a Mac mini.
4. Qualification/second identity/deployment — serialized ben1 owner.

After this contract merges, API and CLI work proceed concurrently. Schema,
migration, generated-client publication, ben1 deployment, and backup/restore
remain serialized.

## Deferred hardening

External IdP/SSO, organizations/teams, custom policy engines, PostgreSQL RLS,
email invitation delivery, service accounts, ownership transfer/deletion, and
a high-scale event broker are production hardening, not POC acceptance.
