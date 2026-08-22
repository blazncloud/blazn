import type { Database } from "./db.js";

export type CleanupIntent = { schemaVersion: string; userId: string; workspaceIds: string[] };

export async function assertPocIdentityAbsent(database: Database, intent: CleanupIntent): Promise<void> {
  if (intent.schemaVersion !== "blazn.dev/poc-second-identity-cleanup/v1" || !/^[0-9a-f-]{36}$/.test(intent.userId) || !Array.isArray(intent.workspaceIds) || intent.workspaceIds.some((id) => !/^[0-9a-f-]{36}$/.test(id))) throw new Error("POC identity cleanup intent is invalid");
  const result = await database.query<{ count: string }>(`SELECT (
    (SELECT count(*) FROM users WHERE id=$1) +
    (SELECT count(*) FROM devices WHERE user_id=$1) +
    (SELECT count(*) FROM sessions WHERE user_id=$1) +
    (SELECT count(*) FROM device_authorizations WHERE approved_user_id=$1) +
    (SELECT count(*) FROM workspaces WHERE created_by=$1) +
    (SELECT count(*) FROM workspace_memberships WHERE user_id=$1) +
    (SELECT count(*) FROM workspace_invitations WHERE created_by=$1 OR accepted_by=$1) +
    (SELECT count(*) FROM workspace_idempotency_receipts WHERE principal_id=$1) +
    (SELECT count(*) FROM workspace_audit_events WHERE actor_user_id=$1 OR subject_user_id=$1) +
    (SELECT count(*) FROM workspaces WHERE id=ANY($2::uuid[]))
  )::text AS count`, [intent.userId, intent.workspaceIds]);
  if (result.rows[0]?.count !== "0") throw new Error("POC identity cleanup has ambiguous residual database state");
}
