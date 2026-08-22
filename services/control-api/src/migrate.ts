import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadConfig } from "./config.js";
import { createDatabase } from "./db.js";

const config = loadConfig();
const database = createDatabase(config.databaseUrl);
const migrationDirectory = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../migrations");

try {
  await database.query("CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())");
  for (const name of (await readdir(migrationDirectory)).filter((entry) => entry.endsWith(".sql")).sort()) {
    const exists = await database.query<{ version: string }>("SELECT version FROM schema_migrations WHERE version = $1", [name]);
    if (exists.rowCount) continue;
    const sql = await readFile(path.join(migrationDirectory, name), "utf8");
    const client = await database.connect();
    try {
      await client.query("BEGIN");
      await client.query(sql);
      await client.query("INSERT INTO schema_migrations(version) VALUES ($1)", [name]);
      await client.query("COMMIT");
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }
} finally {
  await database.end();
}
