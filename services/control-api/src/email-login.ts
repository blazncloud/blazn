import { createHmac, randomInt, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import { connect, type Socket } from "node:net";

export const EMAIL_CODE_TTL_SECONDS = 10 * 60;
export const EMAIL_CODE_MAX_ATTEMPTS = 5;
const EMAIL_PATTERN = /^[^\s@<>()[\]\\,;:"]{1,64}@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/;

export interface EmailSender {
  sendLoginCode(input: { to: string; code: string; deviceName: string }): Promise<void>;
}

export interface EmailLoginConfig {
  sender: EmailSender;
  codeKey: string;
}

export function normalizeEmail(value: string): string | undefined {
  const email = value.trim().toLowerCase();
  if (email.length < 3 || email.length > 254 || !EMAIL_PATTERN.test(email)) return undefined;
  return email;
}

export function normalizeEmailCode(value: string): string | undefined {
  const code = value.replace(/[\s-]/g, "");
  return /^[0-9]{6}$/.test(code) ? code : undefined;
}

export function generateEmailCode(): string {
  return String(randomInt(0, 1_000_000)).padStart(6, "0");
}

// The digest is keyed and bound to the authorization and address so a
// database reader cannot brute-force a six-digit code offline.
export function emailCodeHash(key: string, authorizationId: string, email: string, code: string): string {
  return createHmac("sha256", key).update(`${authorizationId}\n${email}\n${code}`).digest("hex");
}

export function emailCodeMatches(expectedHash: string, actualHash: string): boolean {
  const expected = Buffer.from(expectedHash, "hex");
  const actual = Buffer.from(actualHash, "hex");
  return expected.length === actual.length && timingSafeEqual(expected, actual);
}

export function displayNameFromEmail(email: string): string {
  const local = email.split("@")[0] ?? email;
  return local.slice(0, 128) || email.slice(0, 128);
}

export class ResendEmailSender implements EmailSender {
  constructor(private readonly apiKey: string, private readonly from: string, private readonly endpoint = "https://api.resend.com/emails") {}

  async sendLoginCode(input: { to: string; code: string; deviceName: string }): Promise<void> {
    const device = input.deviceName.replace(/[\r\n]/g, " ").slice(0, 128);
    const text = `Your Blazn sign-in code is ${input.code}\n\nEnter it on the activation page to approve "${device}". The code expires in 10 minutes.\n\nIf you did not request this, ignore this email.`;
    const html = `<div style="font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:0 auto;padding:24px;color:#111"><h2 style="margin:0 0 12px">Your Blazn sign-in code</h2><p style="font-size:32px;font-weight:700;letter-spacing:6px;margin:16px 0;color:#f97316">${input.code}</p><p>Enter this code on the activation page to approve <strong>${escape(device)}</strong>. It expires in 10 minutes.</p><p style="color:#666;font-size:13px">If you did not request this, you can ignore this email.</p></div>`;
    const response = await fetch(this.endpoint, {
      method: "POST",
      headers: { authorization: `Bearer ${this.apiKey}`, "content-type": "application/json" },
      body: JSON.stringify({ from: this.from, to: [input.to], subject: `Your Blazn sign-in code: ${input.code}`, text, html }),
      signal: AbortSignal.timeout(10_000),
    });
    if (!response.ok) throw new Error(`email provider rejected the message with HTTP ${response.status}`);
  }
}

function escape(value: string): string {
  return value.replace(/[&<>"']/g, (character) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[character] ?? character);
}

export function emailLoginFromEnvironment(env: NodeJS.ProcessEnv = process.env): EmailLoginConfig | undefined {
  const keyFile = env.RESEND_API_KEY_FILE;
  if (!keyFile) return undefined;
  const from = env.EMAIL_FROM;
  if (!from || !/^[^<>\r\n]{0,64}<?[^\s@<>]+@[^\s@<>]+>?$/.test(from)) throw new Error("EMAIL_FROM is required when RESEND_API_KEY_FILE is set");
  const apiKey = readFileSync(keyFile, "utf8").trim();
  if (!apiKey) throw new Error("RESEND_API_KEY_FILE is empty");
  const codeKeyFile = env.EMAIL_CODE_HMAC_KEY_FILE;
  if (!codeKeyFile) throw new Error("EMAIL_CODE_HMAC_KEY_FILE is required when RESEND_API_KEY_FILE is set");
  const codeKey = readFileSync(codeKeyFile, "utf8").trim();
  if (!/^[0-9a-f]{64}$/.test(codeKey)) throw new Error("EMAIL_CODE_HMAC_KEY_FILE must contain 64 lowercase hex characters");
  const resend = new ResendEmailSender(apiKey, from);
  const capture = captureFromEnvironment(env, from);
  return { sender: capture ? new CaptureRoutingEmailSender(resend, capture.sender, capture.domain) : resend, codeKey };
}

// Development-only capture: sign-in codes for one reserved, undeliverable
// domain (.invalid, .test or .example per RFC 2606/6761) are delivered over
// plain SMTP to a private in-cluster capture inbox so automated qualification
// can read them. Every other recipient still goes through Resend, and a real
// domain can never be configured as the capture domain.
const CAPTURE_DOMAIN_PATTERN = /^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*(?:invalid|test|example)$/;

function captureFromEnvironment(env: NodeJS.ProcessEnv, from: string): { sender: EmailSender; domain: string } | undefined {
  const target = env.EMAIL_CAPTURE_SMTP, domain = env.EMAIL_CAPTURE_DOMAIN;
  if (!target && !domain) return undefined;
  if (!target || !domain || !CAPTURE_DOMAIN_PATTERN.test(domain)) throw new Error("EMAIL_CAPTURE_SMTP and a reserved EMAIL_CAPTURE_DOMAIN are both required");
  const match = /^((?:10|127)\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}):(\d{1,5})$/.exec(target);
  const port = Number(match?.[2]);
  if (!match || !Number.isInteger(port) || port < 1 || port > 65535 || match[1]!.split(".").some((octet) => Number(octet) > 255)) throw new Error("EMAIL_CAPTURE_SMTP must be a private IPv4 host:port");
  return { sender: new SmtpCaptureEmailSender(match[1]!, port, from), domain };
}

export class CaptureRoutingEmailSender implements EmailSender {
  constructor(private readonly primary: EmailSender, private readonly capture: EmailSender, private readonly domain: string) {}
  sendLoginCode(input: { to: string; code: string; deviceName: string }): Promise<void> {
    return input.to.endsWith(`@${this.domain}`) ? this.capture.sendLoginCode(input) : this.primary.sendLoginCode(input);
  }
}

export class SmtpCaptureEmailSender implements EmailSender {
  private readonly fromAddress: string;
  constructor(private readonly host: string, private readonly port: number, from: string, private readonly timeoutMs = 10_000) {
    const address = /<([^<>\s]+)>\s*$/.exec(from)?.[1] ?? from.trim();
    if (!/^[^\s@<>]+@[^\s@<>]+$/.test(address)) throw new Error("capture sender address is invalid");
    this.fromAddress = address;
  }

  async sendLoginCode(input: { to: string; code: string; deviceName: string }): Promise<void> {
    if (/[\r\n<>]/.test(input.to) || !/^[0-9]{6}$/.test(input.code)) throw new Error("capture message is invalid");
    const device = input.deviceName.replace(/[^\x20-\x7e]/g, " ").slice(0, 128);
    const body = [`From: Blazn <${this.fromAddress}>`, `To: <${input.to}>`, `Subject: Your Blazn sign-in code: ${input.code}`, "MIME-Version: 1.0", "Content-Type: text/plain; charset=utf-8", "", `Your Blazn sign-in code is ${input.code}`, "", `Enter it on the activation page to approve "${device}". The code expires in 10 minutes.`].join("\r\n");
    const socket = connect({ host: this.host, port: this.port });
    socket.setTimeout(this.timeoutMs, () => socket.destroy(new Error("capture SMTP timed out")));
    try {
      const reply = replies(socket);
      await expect(reply, 220);
      for (const [command, code] of [["EHLO blazn-control-api", 250], [`MAIL FROM:<${this.fromAddress}>`, 250], [`RCPT TO:<${input.to}>`, 250], ["DATA", 354]] as const) {
        socket.write(`${command}\r\n`);
        await expect(reply, code);
      }
      socket.write(`${body.replace(/^\./gm, "..")}\r\n.\r\n`);
      await expect(reply, 250);
      socket.write("QUIT\r\n");
    } finally { socket.end(); }
  }
}

function replies(socket: Socket): () => Promise<string> {
  let buffer = "";
  const waiting: { resolve: (line: string) => void; reject: (error: Error) => void }[] = [];
  const complete: string[] = [];
  let failure: Error | undefined;
  socket.setEncoding("utf8");
  socket.on("data", (chunk: string) => {
    buffer += chunk;
    if (buffer.length > 16 * 1024) { socket.destroy(new Error("capture SMTP reply too large")); return; }
    let index;
    while ((index = buffer.indexOf("\r\n")) >= 0) {
      const line = buffer.slice(0, index); buffer = buffer.slice(index + 2);
      if (/^\d{3} /.test(line) || !/^\d{3}-/.test(line)) { const next = waiting.shift(); if (next) next.resolve(line); else complete.push(line); }
    }
  });
  const fail = (error: Error) => { failure = error; for (const next of waiting.splice(0)) next.reject(error); };
  socket.on("error", fail);
  socket.on("close", () => fail(new Error("capture SMTP connection closed")));
  return () => {
    const ready = complete.shift();
    if (ready !== undefined) return Promise.resolve(ready);
    if (failure) return Promise.reject(failure);
    return new Promise((resolve, reject) => waiting.push({ resolve, reject }));
  };
}

async function expect(next: () => Promise<string>, code: number): Promise<void> {
  const line = await next();
  if (!line.startsWith(`${code}`)) throw new Error(`capture SMTP rejected the message with ${line.slice(0, 3)}`);
}
