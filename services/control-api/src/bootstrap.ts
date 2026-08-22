import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { passwordRecord, verifyPassword } from "./security.js";

const databaseUrlFile = process.env.BOOTSTRAP_DATABASE_URL_FILE;
const passwordFile = process.env.BLAZN_INITIAL_PASSWORD_FILE;
const login = process.env.BLAZN_INITIAL_LOGIN?.trim().toLowerCase();
const displayName = process.env.BLAZN_INITIAL_DISPLAY_NAME?.trim();
if (!databaseUrlFile || !passwordFile || !login || !displayName) throw new Error("bootstrap database URL, password file, login, and display name are required");
const database = createDatabase((await readFile(databaseUrlFile, "utf8")).trim());
try {
  const password = (await readFile(passwordFile, "utf8")).trimEnd();
  const record = await passwordRecord(password);
  const client = await database.connect();
  let created = false;
  try {
    await client.query("BEGIN");
    await client.query("SELECT pg_advisory_xact_lock(hashtext('blazn-initial-identity'))");
    const existing = await client.query<{ display_name: string; password_salt: string; password_hash: string }>("SELECT display_name,password_salt,password_hash FROM users WHERE email=$1", [login]);
    if (existing.rows[0]) {
      const matches = existing.rows[0].display_name === displayName && await verifyPassword(password, existing.rows[0].password_salt, existing.rows[0].password_hash);
      if (!matches) throw new Error("existing initial identity does not match the configured bootstrap identity");
      await client.query("COMMIT");
    } else {
      const count = await client.query<{ count: string }>("SELECT count(*)::text AS count FROM users");
      if (count.rows[0]?.count !== "0") throw new Error("configured initial identity is absent while other users exist");
      await client.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,$4,$5)", [randomUUID(), login, displayName, record.salt, record.hash]);
      await client.query("COMMIT");
      created = true;
    }
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
  process.stdout.write(created ? "initial Blazn identity created\n" : "initial Blazn identity already present\n");
} finally {
  await database.end();
}
