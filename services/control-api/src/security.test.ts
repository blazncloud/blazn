import assert from "node:assert/strict";
import test from "node:test";
import { passwordRecord, randomToken, tokenHash, userCode, verifyPassword } from "./security.js";

test("tokens are random and only hashes are retained", () => {
  const first = randomToken();
  const second = randomToken();
  assert.notEqual(first, second);
  assert.match(first, /^[A-Za-z0-9_-]+$/);
  assert.equal(tokenHash(first).length, 64);
  assert.notEqual(tokenHash(first), tokenHash(second));
});

test("user codes avoid ambiguous characters", () => {
  for (let index = 0; index < 100; index += 1) {
    assert.match(userCode(), /^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$/);
  }
});

test("password records verify without retaining the password", async () => {
  const password = "correct horse battery staple";
  const record = await passwordRecord(password);
  assert.equal(await verifyPassword(password, record.salt, record.hash), true);
  assert.equal(await verifyPassword("incorrect horse battery staple", record.salt, record.hash), false);
  assert.doesNotMatch(record.hash, /correct|horse|battery|staple/);
});

test("short passwords are rejected", async () => {
  await assert.rejects(passwordRecord("too-short"), /at least 12/);
});
