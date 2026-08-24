import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { PasswordRecoveryCommitUnknownError, rotateBootstrapPasswordAndClose } from "./password-recovery.js";

async function main(): Promise<void> {
  const databaseUrlFile = process.env.BOOTSTRAP_DATABASE_URL_FILE;
  const passwordFile = process.env.BLAZN_RECOVERY_PASSWORD_FILE;
  const login = process.env.BLAZN_INITIAL_LOGIN?.trim().toLowerCase();
  if (!databaseUrlFile || !passwordFile || !login) throw new Error("recovery configuration is incomplete");

  const databaseUrl = (await readFile(databaseUrlFile, "utf8")).trim();
  const password = (await readFile(passwordFile, "utf8")).trimEnd();
  const database = createDatabase(databaseUrl);
  await rotateBootstrapPasswordAndClose(database, login, password);
}

try {
  await main();
  process.stdout.write("bootstrap password rotated; existing authentication is revoked\n");
} catch (error) {
  process.stderr.write("bootstrap password rotation failed\n");
  process.exitCode = error instanceof PasswordRecoveryCommitUnknownError ? 11 : 10;
}
