import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import type { Database } from "./db.js";
import { rotateBootstrapPassword } from "./password-recovery.js";
import { verifyPassword } from "./security.js";

interface Call { sql: string; values?: unknown[] }

function fakeDatabase(userRows: { id: string }[] = [{ id: "bootstrap-user" }], failOn?: string): { database: Pick<Database, "connect">; calls: Call[] } {
  const calls: Call[] = [];
  const client = {
    async query(sql: string, values?: unknown[]) {
      calls.push(values === undefined ? { sql } : { sql, values });
      if (failOn && sql.startsWith(failOn)) throw new Error("injected database failure");
      if (sql.startsWith("SELECT public.rotate_bootstrap_password") && userRows.length !== 1) throw new Error("configured bootstrap identity must exist exactly once");
      return { rows: [], rowCount: 0 };
    },
    release() {},
  };
  return { database: { connect: async () => client } as unknown as Pick<Database, "connect">, calls };
}

test("rotates the configured identity and invalidates authentication without changing devices", async () => {
  const { database, calls } = fakeDatabase();
  await rotateBootstrapPassword(database, "owner@example.test", "replacement-password");

  assert.deepEqual(calls.map(({ sql }) => sql), [
    "BEGIN",
    "SELECT public.rotate_bootstrap_password($1,$2,$3)",
    "COMMIT",
  ]);
  const recovery = calls[1];
  assert.ok(recovery?.values);
  assert.equal(recovery.values[0], "owner@example.test");
  assert.equal(await verifyPassword("replacement-password", String(recovery.values[1]), String(recovery.values[2])), true);
  assert.equal(calls.some(({ sql }) => /(?:UPDATE|LOCK TABLE).*devices/i.test(sql)), false);
});

test("fails closed and rolls back when the configured identity is absent or duplicated", async () => {
  for (const rows of [[], [{ id: "one" }, { id: "two" }]]) {
    const { database, calls } = fakeDatabase(rows);
    await assert.rejects(rotateBootstrapPassword(database, "owner@example.test", "replacement-password"), /exactly once/);
    assert.equal(calls.at(-1)?.sql, "ROLLBACK");
    assert.equal(calls.some(({ sql }) => /UPDATE\s+users/i.test(sql)), false);
  }
});

test("rejects a short password before opening a transaction", async () => {
  const { database, calls } = fakeDatabase();
  await assert.rejects(rotateBootstrapPassword(database, "owner@example.test", "short"), /at least 12/);
  assert.deepEqual(calls, []);
});

test("rolls back password and revocations when the recovery function fails", async () => {
  const { database, calls } = fakeDatabase([{ id: "bootstrap-user" }], "SELECT public.rotate_bootstrap_password");
  await assert.rejects(rotateBootstrapPassword(database, "owner@example.test", "replacement-password"), /injected/);
  assert.equal(calls.at(-1)?.sql, "ROLLBACK");
  assert.equal(calls.some(({ sql }) => sql === "COMMIT"), false);
});

test("reports an unknown outcome without rollback when commit acknowledgement fails", async () => {
  const { database, calls } = fakeDatabase([{ id: "bootstrap-user" }], "COMMIT");
  await assert.rejects(rotateBootstrapPassword(database, "owner@example.test", "replacement-password"), /outcome is unknown/);
  assert.equal(calls.at(-1)?.sql, "COMMIT");
  assert.equal(calls.some(({ sql }) => sql === "ROLLBACK"), false);
});

test("recovery command discloses no configured identity or secret values", async () => {
  const source = await readFile(new URL("rotate-bootstrap-password.js", import.meta.url), "utf8");
  assert.match(source, /BLAZN_RECOVERY_PASSWORD_FILE/);
  assert.doesNotMatch(source, /process\.argv|BLAZN_RECOVERY_PASSWORD(?!_FILE)/);
  assert.doesNotMatch(source, /console\./);
  assert.deepEqual(Array.from(source.matchAll(/process\.(?:stdout|stderr)\.write\("([^"]+)"\)/g), (match) => match[1]), [
    "bootstrap password rotated; existing authentication is revoked\\n",
    "bootstrap password rotation failed\\n",
  ]);
});

test("ordinary bootstrap remains a separate create-or-verify command", async () => {
  const source = await readFile(new URL("bootstrap.js", import.meta.url), "utf8");
  assert.match(source, /existing initial identity does not match/);
  assert.match(source, /INSERT INTO users/);
  assert.doesNotMatch(source, /UPDATE users SET password|rotateBootstrapPassword|BLAZN_RECOVERY_PASSWORD_FILE/);
});
