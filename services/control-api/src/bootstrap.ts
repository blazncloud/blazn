import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { passwordRecord } from "./security.js";

const databaseUrlFile = process.env.BOOTSTRAP_DATABASE_URL_FILE;
const passwordFile = process.env.BLAZN_INITIAL_PASSWORD_FILE;
const login = process.env.BLAZN_INITIAL_LOGIN?.trim().toLowerCase();
const displayName = process.env.BLAZN_INITIAL_DISPLAY_NAME?.trim();
if (!databaseUrlFile || !passwordFile || !login || !displayName) throw new Error("bootstrap database URL, password file, login, and display name are required");
const database = createDatabase((await readFile(databaseUrlFile, "utf8")).trim());
try {
  const count = await database.query<{ count: string }>("SELECT count(*)::text AS count FROM users");
  if (count.rows[0]?.count !== "0") throw new Error("initial identity bootstrap is allowed only when no users exist");
  const password = (await readFile(passwordFile, "utf8")).trimEnd();
  const record = await passwordRecord(password);
  await database.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,$4,$5)", [randomUUID(), login, displayName, record.salt, record.hash]);
  process.stdout.write("initial Blazn identity created\n");
} finally {
  await database.end();
}
