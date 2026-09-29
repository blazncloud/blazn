import assert from "node:assert/strict";
import test from "node:test";
import { displayNameFromEmail, emailCodeHash, emailCodeMatches, generateEmailCode, normalizeEmail, normalizeEmailCode, ResendEmailSender } from "./email-login.js";

test("email addresses are normalized and malformed ones rejected", () => {
  assert.equal(normalizeEmail("  Ben@Example.COM "), "ben@example.com");
  assert.equal(normalizeEmail("no-at-sign"), undefined);
  assert.equal(normalizeEmail("a@b"), undefined);
  assert.equal(normalizeEmail("a<b>@example.com"), undefined);
  assert.equal(displayNameFromEmail("ben.pelo@example.com"), "ben.pelo");
});

test("codes are six digits and tolerate spacing", () => {
  for (let index = 0; index < 50; index += 1) assert.match(generateEmailCode(), /^[0-9]{6}$/);
  assert.equal(normalizeEmailCode("123 456"), "123456");
  assert.equal(normalizeEmailCode("123-456"), "123456");
  assert.equal(normalizeEmailCode("12345"), undefined);
  assert.equal(normalizeEmailCode("abcdef"), undefined);
});

test("code digests are keyed and bound to authorization and address", () => {
  const key = "a".repeat(64);
  const digest = emailCodeHash(key, "auth-1", "a@example.com", "123456");
  assert.match(digest, /^[0-9a-f]{64}$/);
  assert.ok(emailCodeMatches(digest, emailCodeHash(key, "auth-1", "a@example.com", "123456")));
  assert.ok(!emailCodeMatches(digest, emailCodeHash(key, "auth-2", "a@example.com", "123456")));
  assert.ok(!emailCodeMatches(digest, emailCodeHash(key, "auth-1", "b@example.com", "123456")));
  assert.ok(!emailCodeMatches(digest, emailCodeHash("b".repeat(64), "auth-1", "a@example.com", "123456")));
});

test("Resend delivery posts one recipient and surfaces provider rejection", async () => {
  const original = globalThis.fetch;
  const calls: { url: string; init: RequestInit }[] = [];
  globalThis.fetch = (async (url: string, init: RequestInit) => { calls.push({ url, init }); return new Response("{}", { status: calls.length === 1 ? 200 : 403 }); }) as typeof fetch;
  try {
    const sender = new ResendEmailSender("re_test", "Blazn <no-reply@example.com>", "https://resend.test/emails");
    await sender.sendLoginCode({ to: "a@example.com", code: "123456", deviceName: "laptop" });
    const body = JSON.parse(String(calls[0]?.init.body));
    assert.deepEqual(body.to, ["a@example.com"]);
    assert.match(body.text, /123456/);
    assert.equal((calls[0]?.init.headers as Record<string, string>).authorization, "Bearer re_test");
    await assert.rejects(sender.sendLoginCode({ to: "a@example.com", code: "123456", deviceName: "laptop" }), /HTTP 403/);
  } finally { globalThis.fetch = original; }
});
