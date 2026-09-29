import { createHmac, randomInt, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";

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
  return { sender: new ResendEmailSender(apiKey, from), codeKey };
}
