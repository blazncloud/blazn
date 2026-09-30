import assert from "node:assert/strict";
import test from "node:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { CaptureRoutingEmailSender, displayNameFromEmail, emailCodeHash, emailCodeMatches, emailLoginFromEnvironment, generateEmailCode, normalizeEmail, normalizeEmailCode, ResendEmailSender, SmtpCaptureEmailSender, type EmailSender } from "./email-login.js";

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

test("capture sender delivers one plain SMTP message with the code", async () => {
  const transcript: string[] = [];
  const server = createServer((socket) => {
    socket.setEncoding("utf8"); let data = false, buffer = "";
    socket.write("220 capture ready\r\n");
    socket.on("data", (chunk: string) => {
      buffer += chunk;
      let index;
      while ((index = buffer.indexOf("\r\n")) >= 0) {
        const line = buffer.slice(0, index); buffer = buffer.slice(index + 2);
        transcript.push(line);
        if (data) { if (line === ".") { data = false; socket.write("250 queued\r\n"); } continue; }
        if (line.startsWith("EHLO")) socket.write("250-capture\r\n250 SIZE 1048576\r\n");
        else if (line === "DATA") { data = true; socket.write("354 go\r\n"); }
        else if (line === "QUIT") { socket.end("221 bye\r\n"); }
        else socket.write("250 ok\r\n");
      }
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const port = (server.address() as { port: number }).port;
    await new SmtpCaptureEmailSender("127.0.0.1", port, "Blazn <no-reply@example.com>").sendLoginCode({ to: "qualification-a@blazn.invalid", code: "123456", deviceName: "laptop" });
    assert.ok(transcript.includes("MAIL FROM:<no-reply@example.com>"));
    assert.ok(transcript.includes("RCPT TO:<qualification-a@blazn.invalid>"));
    assert.ok(transcript.some((line) => line === "Subject: Your Blazn sign-in code: 123456"));
  } finally { await new Promise<void>((resolve) => server.close(() => resolve())); }
});

test("capture SMTP rejection surfaces as a delivery failure", async () => {
  const server = createServer((socket) => { socket.write("220 ready\r\n"); socket.on("data", () => socket.write("550 recipient rejected\r\n")); });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const port = (server.address() as { port: number }).port;
    await assert.rejects(new SmtpCaptureEmailSender("127.0.0.1", port, "no-reply@example.com").sendLoginCode({ to: "qualification-a@blazn.invalid", code: "123456", deviceName: "x" }), /rejected/);
  } finally { await new Promise<void>((resolve) => server.close(() => resolve())); }
});

test("only the reserved capture domain is routed to capture", async () => {
  const seen: string[] = [];
  const sender = (name: string): EmailSender => ({ async sendLoginCode(input) { seen.push(`${name}:${input.to}`); } });
  const routing = new CaptureRoutingEmailSender(sender("resend"), sender("capture"), "blazn.invalid");
  for (const to of ["qualification-a@blazn.invalid", "ben@example.com", "a@notblazn.invalid", "a@blazn.invalid.example.com"]) await routing.sendLoginCode({ to, code: "123456", deviceName: "x" });
  assert.deepEqual(seen, ["capture:qualification-a@blazn.invalid", "resend:ben@example.com", "resend:a@notblazn.invalid", "resend:a@blazn.invalid.example.com"]);
});

test("capture configuration refuses real domains and public SMTP hosts", () => {
  const directory = mkdtempSync(path.join(tmpdir(), "blazn-email-"));
  writeFileSync(path.join(directory, "resend"), "re_test"); writeFileSync(path.join(directory, "code"), "a".repeat(64));
  const base = { RESEND_API_KEY_FILE: path.join(directory, "resend"), EMAIL_CODE_HMAC_KEY_FILE: path.join(directory, "code"), EMAIL_FROM: "Blazn <no-reply@example.com>" };
  assert.ok(emailLoginFromEnvironment(base)?.sender instanceof ResendEmailSender);
  assert.ok(emailLoginFromEnvironment({ ...base, EMAIL_CAPTURE_SMTP: "10.152.183.239:1025", EMAIL_CAPTURE_DOMAIN: "blazn.invalid" })?.sender instanceof CaptureRoutingEmailSender);
  for (const [smtp, domain] of [["10.152.183.239:1025", "frontro.com"], ["10.152.183.239:1025", "gmail.com"], ["8.8.8.8:25", "blazn.invalid"], ["mail.example.com:25", "blazn.invalid"], ["10.1.2.3:99999", "blazn.invalid"], ["10.1.2.3:1025", undefined], [undefined, "blazn.invalid"]]) {
    assert.throws(() => emailLoginFromEnvironment({ ...base, ...(smtp ? { EMAIL_CAPTURE_SMTP: smtp } : {}), ...(domain ? { EMAIL_CAPTURE_DOMAIN: domain } : {}) }), /EMAIL_CAPTURE/, `${smtp} ${domain}`);
  }
});
