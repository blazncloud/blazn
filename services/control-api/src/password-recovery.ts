import type { Database } from "./db.js";
import { passwordRecord } from "./security.js";

type RecoveryDatabase = Pick<Database, "connect">;
type CloseableRecoveryDatabase = Pick<Database, "connect" | "end">;

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
    await client.query("SELECT public.rotate_bootstrap_password($1,$2,$3)", [login, record.salt, record.hash]);
    commitAttempted = true;
    await client.query("COMMIT");
  } catch (error) {
    if (commitAttempted) throw new PasswordRecoveryCommitUnknownError();
    await client.query("ROLLBACK").catch(() => undefined);
    throw error;
  } finally {
    try {
      client.release();
    } catch {
      if (commitAttempted) throw new PasswordRecoveryCommitUnknownError();
    }
  }
}

export async function rotateBootstrapPasswordAndClose(database: CloseableRecoveryDatabase, login: string, password: string): Promise<void> {
  let rotationError: unknown;
  try {
    await rotateBootstrapPassword(database, login, password);
  } catch (error) {
    rotationError = error;
  }

  try {
    await database.end();
  } catch {
    if (rotationError === undefined) throw new PasswordRecoveryCommitUnknownError();
  }

  if (rotationError !== undefined) throw rotationError;
}
