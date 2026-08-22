import assert from "node:assert/strict";
import test from "node:test";
import { generateKeyPairSync, sign } from "node:crypto";
import { passwordRecord, randomToken, secretMatches, tokenHash, userCode, verifyDeviceProof, verifyPassword } from "./security.js";

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

test("device proofs require the matching key and canonical message", () => {
  const keys = generateKeyPairSync("ed25519");
  const publicKey = keys.publicKey.export({ format: "jwk" }).x;
  assert.ok(publicKey);
  const canonical = "blazn-device-session-v1\ndevice-code\nchallenge";
  const signature = sign(null, Buffer.from(canonical), keys.privateKey).toString("base64url");
  assert.equal(verifyDeviceProof(publicKey, canonical, signature), true);
  assert.equal(verifyDeviceProof(publicKey, `${canonical}-changed`, signature), false);
  assert.equal(verifyDeviceProof("invalid", canonical, signature), false);
});

test("bootstrap secrets compare by digest", () => {
  assert.equal(secretMatches("one secret", "one secret"), true);
  assert.equal(secretMatches("one secret", "another secret"), false);
});
