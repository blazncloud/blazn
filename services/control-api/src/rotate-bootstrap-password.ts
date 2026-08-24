import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { PasswordRecoveryCommitUnknownError, rotateBootstrapPassword } from "./password-recovery.js";

async function main(): Promise<void> {
  const databaseUrlFile = process.env.BOOTSTRAP_DATABASE_URL_FILE;
  const passwordFile = process.env.BLAZN_RECOVERY_PASSWORD_FILE;
  const login = process.env.BLAZN_INITIAL_LOGIN?.trim().toLowerCase();
  if (!databaseUrlFile || !passwordFile || !login) throw new Error("recovery configuration is incomplete");

  const database = createDatabase((await readFile(databaseUrlFile, "utf8")).trim());
  try {
    const password = (await readFile(passwordFile, "utf8")).trimEnd();
    await rotateBootstrapPassword(database, login, password);
  } finally {
    await database.end();
  }
}

try {
  await main();
  process.stdout.write("bootstrap password rotated; existing authentication is revoked\n");
} catch (error) {
  process.stderr.write("bootstrap password rotation failed\n");
  process.exitCode = error instanceof PasswordRecoveryCommitUnknownError ? 11 : 10;
}
