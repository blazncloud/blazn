# Workspace Run and Artifact contract v1alpha1

Runs and Artifacts are canonical Project-linked Workspace resources. Content,
agents, schedules, sandboxes, local nodes, providers, and future CLIs use this
shared lifecycle rather than private job databases.

## Evidence classes

Every Run declares exactly one `proofClass`:

- `synthetic` is deterministic fixture or no-op proof and has no live placement;
- `local` requires a populated authorized Node identity after admission;
- `sandbox` requires populated Node and Sandbox identities after admission;
- `provider` requires a populated Workspace model/API route after submission.

Null placement is never live proof. Terminal Runs require an immutable receipt
whose proof class, outcome, and plan digest bind the corresponding Run. A
synthetic receipt must never be presented as local, Sandbox, provider, deployed,
or production evidence.

## Artifact boundary

Artifacts belong to one `(workspace_id, project_id)` and may reference a source
Run in the same tenant. Public API metadata exposes content digest, size, media
type, lifecycle status, provenance IDs, and whether a download operation is
available. It never exposes object-store keys, signed URLs, credentials, or
provider secrets. Ready Artifacts require digest, size, and an internal object
key; non-ready Artifacts are not downloadable.

## Persistence and lifecycle

Migration `012_runs_artifacts.sql` creates Runs, ordered events, immutable
receipts, Artifacts, and ordered tenant-bound input-Artifact links with composite
foreign keys. Output names are immutable validated values rather than generic
parameter JSON. Queued, running, and
terminal timestamp/error invariants are enforced in PostgreSQL. Runtime roles
can create and advance resources but cannot physically delete historical Runs,
events, receipts, or Artifacts.

The normative public surface is `packages/contracts/runs.openapi.json`.
Synthetic execution adds three narrowly scoped operations: monotonic progress,
terminal completion, and digest-bound binary Artifact upload. They accept only
the requesting principal's synthetic Run in the selected tenant, cannot set
placement or upgrade proof, and bind completion to the Run's immutable plan
digest and ready same-Run Artifacts. Upload metadata is canonical JSON carried
as unpadded Base64URL in one closed header while bytes remain
`application/octet-stream`; partial, oversized, or
digest/size-mismatched content never becomes ready. Live scheduler admission,
non-synthetic execution, scoped download grants, and provider placement remain
separate authorities.
