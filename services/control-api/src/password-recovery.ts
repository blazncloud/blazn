import type { Database } from "./db.js";
import { passwordRecord } from "./security.js";

type RecoveryDatabase = Pick<Database, "connect">;

export class PasswordRecoveryCommitUnknownError extends Error {
  constructor() {
    super("password recovery commit outcome is unknown");
  }
}

export async function rotateBootstrapPassword(database: RecoveryDatabase, login: string, password: string): Promise<void> {
  const record = await passwordRecord(password);
  const client = await database.connect();
  let commitAttempted = false;
  try {
    await client.query("BEGIN");
    await client.query("SELECT pg_advisory_xact_lock(hashtext('blazn-initial-identity'))");
    await client.query("LOCK TABLE sessions, device_authorizations IN SHARE ROW EXCLUSIVE MODE");
    const identity = await client.query<{ id: string }>("SELECT id FROM users WHERE email=$1 FOR UPDATE", [login]);
    if (identity.rowCount !== 1 || !identity.rows[0]) throw new Error("configured bootstrap identity must exist exactly once");

    await client.query("UPDATE users SET password_salt=$1,password_hash=$2 WHERE id=$3", [record.salt, record.hash, identity.rows[0].id]);
    await client.query("UPDATE sessions SET revoked_at=COALESCE(revoked_at, now()) WHERE user_id=$1", [identity.rows[0].id]);
    await client.query("UPDATE device_authorizations SET expires_at=LEAST(expires_at, now()),consumed_at=COALESCE(consumed_at, now()) WHERE consumed_at IS NULL");
    commitAttempted = true;
    await client.query("COMMIT");
  } catch (error) {
    if (commitAttempted) throw new PasswordRecoveryCommitUnknownError();
    await client.query("ROLLBACK").catch(() => undefined);
    throw error;
  } finally {
    client.release();
  }
}
