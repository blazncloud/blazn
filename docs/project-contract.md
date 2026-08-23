# Workspace Project contract v1alpha1

Projects are canonical Workspace resources shared by people, agents, and every
first-party Blazn CLI. Content must attach its media manifest and artifacts to a
Workspace Project rather than create a parallel project database.

## Resource boundary

A Project has an immutable ID, owning Workspace, creator, and creation time. Its
slug is unique only inside that Workspace. Name, description, lifecycle status,
and optimistic-concurrency version are mutable. `kind` is a stable discovery
classification such as `general` or `content`; it is metadata, not permission.

Workspace membership authorizes Project access. Read operations require an
active Workspace membership. Create and update operations require an explicit
Workspace policy capability, an idempotency key, and the expected Project
version. Archived Projects remain addressable and auditable; the runtime role
cannot physically delete them.

## API

The normative `packages/contracts/projects.openapi.json` contract defines:

```text
POST  /v1/workspaces/{workspaceId}/projects
GET   /v1/workspaces/{workspaceId}/projects
GET   /v1/workspaces/{workspaceId}/projects/{projectId}
PATCH /v1/workspaces/{workspaceId}/projects/{projectId}
GET   /v1/workspaces/{workspaceId}/projects/{projectId}/profiles/{profileKind}
PUT   /v1/workspaces/{workspaceId}/projects/{projectId}/profiles/{profileKind}
```

Lists default to active Projects and paginate by an opaque cursor. Requests and
responses reject unknown fields. Project IDs are always paired with the owning
Workspace ID so callers cannot treat a globally valid UUID as cross-Workspace
authorization.

## Persistence

Migration `010_projects.sql` creates the tenant-bound Project table, scoped slug
uniqueness, active/archive lifecycle, version checks, creator provenance, and
runtime grants. The runtime role receives no physical-delete permission.

Project tasks, milestones, decisions, Content profiles, Runs, and Artifacts are
separate versioned resources that reference `(project_id, workspace_id)`.

Project profiles are generic root-owned attachment records keyed by
`(workspace_id, project_id, kind)`. A profile binds one plugin schema version
and local draft UUID to a ready same-tenant Artifact and its exact digest.
Creation uses `expectedVersion=0`; updates require the current positive version.
The kind comes only from the route, profiles never contain provider credentials
or arbitrary plugin JSON, and archived Projects cannot accept active profile
mutations. Content uses kind `content`; other plugins share the same contract
without adding product-specific columns to the Project table.
