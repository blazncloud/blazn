import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

test("auth migration grants only the reviewed bootstrap and runtime operations", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const directory = path.resolve(here, "../migrations");
  const migration = (await Promise.all((await readdir(directory)).filter((name) => name.endsWith(".sql")).sort().map((name) => readFile(path.join(directory, name), "utf8")))).join("\n");
  const grants = [
    "REVOKE ALL ON ALL TABLES IN SCHEMA public FROM blazn_runtime, blazn_bootstrap;",
    "REVOKE ALL ON SEQUENCES FROM blazn_runtime, blazn_bootstrap;",
    "GRANT SELECT, INSERT ON TABLE users TO blazn_bootstrap;",
    "GRANT EXECUTE ON FUNCTION public.rotate_bootstrap_password(text, text, text) TO blazn_bootstrap;",
    "REVOKE INSERT, UPDATE, DELETE ON TABLE users FROM blazn_runtime;",
    "GRANT SELECT ON TABLE users TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE ON TABLE devices TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE device_authorizations TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE ON TABLE sessions TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE auth_rate_limits TO blazn_runtime;",
  ];
  for (const grant of grants) assert.ok(migration.includes(grant), `missing least-privilege SQL: ${grant}`);
  assert.doesNotMatch(migration, /GRANT\s+(?:ALL|SELECT, INSERT, UPDATE, DELETE)\s+ON\s+(?:ALL TABLES|TABLE users)\s+TO blazn_runtime/i);
});

test("password recovery is isolated behind a hardened migration-owned function", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const migration = await readFile(path.resolve(here, "../migrations/003_password_recovery_grants.sql"), "utf8");
  assert.match(migration, /SECURITY DEFINER\s+SET search_path = pg_catalog/);
  assert.match(migration, /ALTER FUNCTION public\.rotate_bootstrap_password\(text, text, text\) OWNER TO blazn_migration;/);
  assert.match(migration, /REVOKE ALL ON FUNCTION public\.rotate_bootstrap_password\(text, text, text\) FROM PUBLIC;/);
  assert.match(migration, /GRANT EXECUTE ON FUNCTION public\.rotate_bootstrap_password\(text, text, text\) TO blazn_bootstrap;/);
  assert.doesNotMatch(migration, /GRANT\s+(?:SELECT|INSERT|UPDATE|DELETE|TRUNCATE|REFERENCES).*ON TABLE.*TO blazn_bootstrap/i);
  for (const table of ["users", "sessions", "device_authorizations"]) assert.match(migration, new RegExp(`(?:FROM|UPDATE|TABLE) public\\.${table}\\b`));
  assert.doesNotMatch(migration, /(?:UPDATE|LOCK TABLE) public\.devices\b/);
  assert.ok(migration.indexOf("LOCK TABLE public.sessions") < migration.indexOf("FROM public.users"));
});
