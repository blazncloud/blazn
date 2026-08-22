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

test("node enrollment signing trust is immutable and relationally bound to plans", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/006_node_plan_signing_trust.sql"), "utf8");
  assert.match(sql, /requires zero pre-contract node enrollments/);
  assert.match(sql, /plan_signing_public_key char\(43\) NOT NULL/);
  assert.match(sql, /plan_signing_key_fingerprint char\(64\) NOT NULL/);
  assert.match(sql, /FOREIGN KEY \(enrollment_id, signing_key_id\)[\s\S]*REFERENCES node_enrollments\(id, plan_signing_key_id\)/);
  assert.match(sql, /REVOKE UPDATE ON TABLE node_enrollments FROM blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT UPDATE \([^)]*plan_signing_(?:key_id|public_key|key_fingerprint)/);
});

test("Node broker can connect without receiving schema creation rights", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/007_node_broker_connect.sql"), "utf8");
  assert.match(sql, /GRANT CONNECT ON DATABASE %I TO blazn_node_broker/);
  assert.match(sql, /REVOKE CREATE ON SCHEMA public FROM blazn_node_broker/);
  assert.match(sql, /REVOKE INSERT, UPDATE ON TABLE node_join_issuances FROM blazn_node_broker/);
  assert.match(sql, /GRANT INSERT \([\s\S]*credential_ciphertext[\s\S]*expires_at[\s\S]*\) ON TABLE node_join_issuances TO blazn_node_broker/);
  assert.doesNotMatch(sql, /GRANT INSERT \([^)]*(?:consumed_at|revoked_at|joined_node_uid)/);
});

test("Node broker durable intents and row-lock function are narrowly granted", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/008_node_broker_intents.sql"), "utf8");
  assert.match(sql, /provider_handle text NOT NULL CHECK \(provider_handle = id::text\)/);
  assert.match(sql, /status IN \('pending', 'issuing', 'revoke_required', 'completed', 'revoked'\)/);
  assert.match(sql, /CHECK \(\(status = 'issuing'\) = \(lease_expires_at IS NOT NULL\)\)/);
  assert.match(sql, /FOR UPDATE OF e,p,n/);
  assert.match(sql, /REVOKE ALL ON FUNCTION node_broker_lock_join_binding/);
  assert.match(sql, /GRANT EXECUTE ON FUNCTION node_broker_lock_join_binding/);
});
