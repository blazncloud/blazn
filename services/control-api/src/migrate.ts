import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createDatabase } from "./db.js";
import { readMigrationInventory, validateAppliedMigrations } from "./migration-inventory.js";

const migrationUrlFile = process.env.MIGRATION_DATABASE_URL_FILE;
if (!migrationUrlFile) throw new Error("MIGRATION_DATABASE_URL_FILE is required");
const database = createDatabase((await readFile(migrationUrlFile, "utf8")).trim());
const migrationDirectory = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../migrations");
const migrationInventory = await readMigrationInventory(migrationDirectory);
const client = await database.connect();

try {
  await client.query("SELECT pg_advisory_lock(hashtext('blazn-schema-migrations'))");
  await client.query("CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())");
  const applied = await client.query<{ version: string }>("SELECT version FROM schema_migrations ORDER BY version");
  validateAppliedMigrations(migrationInventory, applied.rows.map((row) => row.version));
  for (const name of migrationInventory) {
    const sql = await readFile(path.join(migrationDirectory, name), "utf8");
    const checksum = createHash("sha256").update(sql).digest("hex");
    const exists = await client.query<{ checksum: string }>("SELECT checksum FROM schema_migrations WHERE version = $1", [name]);
    if (exists.rows[0]) {
      if (exists.rows[0].checksum !== checksum) throw new Error(`applied migration ${name} has changed`);
      continue;
    }
    try {
      await client.query("BEGIN");
      await client.query(sql);
      await client.query("INSERT INTO schema_migrations(version, checksum) VALUES ($1, $2)", [name, checksum]);
      await client.query("COMMIT");
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    }
  }
  const completed = await client.query<{ version: string }>("SELECT version FROM schema_migrations ORDER BY version");
  validateAppliedMigrations(migrationInventory, completed.rows.map((row) => row.version), true);
} finally {
  await client.query("SELECT pg_advisory_unlock(hashtext('blazn-schema-migrations'))").catch(() => undefined);
  client.release();
  await database.end();
}
