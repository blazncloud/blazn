import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { passwordRecord, verifyPassword } from "./security.js";

type Profile = { login: string; displayName: string };
const action = process.env.POC_IDENTITY_ACTION;
const databaseFile = process.env.POC_IDENTITY_DATABASE_URL_FILE;
const passwordFile = process.env.POC_IDENTITY_PASSWORD_FILE;
const profileFile = process.env.POC_IDENTITY_PROFILE_FILE;
if (!action || !databaseFile || !passwordFile || !profileFile) throw new Error("POC identity action and named input files are required");

const profile = JSON.parse(await readFile(profileFile, "utf8")) as Profile;
const keys = Object.keys(profile).sort();
if (keys.join(",") !== "displayName,login" || typeof profile.login !== "string" || typeof profile.displayName !== "string") throw new Error("POC identity profile is invalid");
const login = profile.login.trim().toLowerCase();
const displayName = profile.displayName.trim();
if (!/^[^@\s]+@[^@\s]+$/.test(login) || displayName.length < 1 || displayName.length > 160) throw new Error("POC identity profile values are invalid");
const password = (await readFile(passwordFile, "utf8")).trimEnd();
const database = createDatabase((await readFile(databaseFile, "utf8")).trim());

try {
  const client = await database.connect();
  try {
    await client.query("BEGIN");
    await client.query("SELECT pg_advisory_xact_lock(hashtext('blazn-poc-second-identity'))");
    if (action === "provision") {
      const existing = await client.query<{ id: string; display_name: string; password_salt: string; password_hash: string }>(
        "SELECT id,display_name,password_salt,password_hash FROM users WHERE email=$1", [login]);
      let userId: string;
      let status: string;
      if (existing.rows[0]) {
        const matches = existing.rows[0].display_name === displayName && await verifyPassword(password, existing.rows[0].password_salt, existing.rows[0].password_hash);
        if (!matches) throw new Error("existing POC identity does not match its root-owned profile and password");
        userId = existing.rows[0].id;
        status = "existing";
      } else {
        const record = await passwordRecord(password);
        userId = randomUUID();
        await client.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,$4,$5)", [userId, login, displayName, record.salt, record.hash]);
        status = "created";
      }
      await client.query("COMMIT");
      process.stdout.write(JSON.stringify({ status, userId }) + "\n");
    } else if (action === "cleanup") {
      const userIdFile = process.env.POC_IDENTITY_USER_ID_FILE;
      const workspacesFile = process.env.POC_IDENTITY_WORKSPACES_FILE;
      if (!userIdFile || !workspacesFile) throw new Error("cleanup identity and workspace files are required");
      const userId = (await readFile(userIdFile, "utf8")).trim();
      if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(userId)) throw new Error("cleanup user ID is invalid");
      const cleanup = JSON.parse(await readFile(workspacesFile, "utf8")) as { workspaceIds?: unknown };
      if (Object.keys(cleanup).join(",") !== "workspaceIds" || !Array.isArray(cleanup.workspaceIds) || cleanup.workspaceIds.some((id) => typeof id !== "string" || !/^[0-9a-f-]{36}$/.test(id))) throw new Error("cleanup workspace inventory is invalid");
      const allowed = new Set(cleanup.workspaceIds as string[]);
      if (allowed.size !== cleanup.workspaceIds.length) throw new Error("cleanup workspace inventory contains duplicates");
      const existing = await client.query<{ id: string; display_name: string; password_salt: string; password_hash: string }>(
        "SELECT id,display_name,password_salt,password_hash FROM users WHERE id=$1 AND email=$2 FOR UPDATE", [userId, login]);
      if (!existing.rows[0]) throw new Error("receipted POC identity is absent");
      if (existing.rows[0].display_name !== displayName || !(await verifyPassword(password, existing.rows[0].password_salt, existing.rows[0].password_hash))) throw new Error("receipted POC identity no longer matches its credentials");
      const owned = await client.query("SELECT id FROM workspaces WHERE created_by=$1", [userId]);
      if (owned.rowCount) throw new Error("POC second identity owns a workspace and cannot be automatically removed");
      const references = await client.query<{ workspace_id: string }>(`SELECT DISTINCT workspace_id FROM (
        SELECT workspace_id FROM workspace_memberships WHERE user_id=$1
        UNION ALL SELECT workspace_id FROM workspace_invitations WHERE created_by=$1 OR accepted_by=$1
        UNION ALL SELECT workspace_id FROM workspace_idempotency_receipts WHERE principal_id=$1
        UNION ALL SELECT workspace_id FROM workspace_audit_events WHERE actor_user_id=$1 OR subject_user_id=$1
      ) refs`, [userId]);
      for (const row of references.rows) if (!allowed.has(row.workspace_id)) throw new Error("POC identity has a workspace reference outside the exact cleanup inventory");
      for (const workspaceId of allowed) {
        const workspace = await client.query<{ slug: string; created_by: string }>(
          "SELECT slug,created_by FROM workspaces WHERE id=$1 FOR UPDATE", [workspaceId]);
        if (!workspace.rows[0] || !workspace.rows[0].slug.startsWith("poc-company-") || workspace.rows[0].created_by === userId) throw new Error("cleanup workspace is absent, not qualification-scoped, or owned by the POC identity");
        await client.query("DELETE FROM workspaces WHERE id=$1", [workspaceId]);
      }
      const authorizations = await client.query("DELETE FROM device_authorizations WHERE approved_user_id=$1", [userId]);
      const devices = await client.query("DELETE FROM devices WHERE user_id=$1", [userId]);
      const removed = await client.query("DELETE FROM users WHERE id=$1 AND email=$2", [userId, login]);
      if (removed.rowCount !== 1) throw new Error("POC identity cleanup did not delete exactly one user");
      await client.query("COMMIT");
      process.stdout.write(JSON.stringify({ status: "cleaned", userId, workspaceCount: allowed.size, deviceCount: devices.rowCount ?? 0, authorizationCount: authorizations.rowCount ?? 0 }) + "\n");
    } else {
      throw new Error("unsupported POC identity action");
    }
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
} finally {
  await database.end();
}
