import { createCipheriv, createDecipheriv, randomBytes, timingSafeEqual } from "node:crypto";
import type { IncomingMessage } from "node:http";

export interface OidcTransaction {
  state: string;
  nonce: string;
  codeVerifier: string;
  userCode: string;
  mode: "signin" | "signup";
  issuedAt: number;
}

const COOKIE_NAME = "blazn_oidc";
const MAX_AGE_SECONDS = 10 * 60;

export function oidcCookieKey(encoded: string): Buffer {
  if (!/^[A-Za-z0-9_-]{43}$/.test(encoded)) throw new Error("OIDC cookie key must be a 32-byte base64url value");
  const key = Buffer.from(encoded, "base64url");
  if (key.length !== 32) throw new Error("OIDC cookie key must decode to exactly 32 bytes");
  return key;
}

export function sealOidcTransaction(key: Buffer, transaction: OidcTransaction): string {
  if (key.length !== 32) throw new Error("OIDC cookie key must contain 32 bytes");
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", key, iv);
  cipher.setAAD(Buffer.from("blazn-oidc-transaction-v1"));
  const ciphertext = Buffer.concat([cipher.update(JSON.stringify(transaction), "utf8"), cipher.final()]);
  return Buffer.concat([iv, cipher.getAuthTag(), ciphertext]).toString("base64url");
}

export function unsealOidcTransaction(key: Buffer, value: string, now = Date.now()): OidcTransaction {
  if (!/^[A-Za-z0-9_-]+$/.test(value) || value.length > 4096) throw new Error("OIDC transaction cookie is invalid");
  const packed = Buffer.from(value, "base64url");
  if (packed.length < 29) throw new Error("OIDC transaction cookie is invalid");
  const decipher = createDecipheriv("aes-256-gcm", key, packed.subarray(0, 12));
  decipher.setAAD(Buffer.from("blazn-oidc-transaction-v1"));
  decipher.setAuthTag(packed.subarray(12, 28));
  let parsed: unknown;
  try { parsed = JSON.parse(Buffer.concat([decipher.update(packed.subarray(28)), decipher.final()]).toString("utf8")); }
  catch { throw new Error("OIDC transaction cookie could not be authenticated"); }
  if (!parsed || typeof parsed !== "object") throw new Error("OIDC transaction cookie is invalid");
  const transaction = parsed as Partial<OidcTransaction>;
  if (typeof transaction.state !== "string" || typeof transaction.nonce !== "string" || typeof transaction.codeVerifier !== "string" || typeof transaction.userCode !== "string" || (transaction.mode !== "signin" && transaction.mode !== "signup") || typeof transaction.issuedAt !== "number") throw new Error("OIDC transaction cookie is incomplete");
  if (transaction.issuedAt > now + 30_000 || now - transaction.issuedAt > MAX_AGE_SECONDS * 1000) throw new Error("OIDC transaction cookie is expired");
  return transaction as OidcTransaction;
}

function cookies(request: IncomingMessage): Map<string, string> {
  const output = new Map<string, string>();
  for (const part of (request.headers.cookie ?? "").split(";")) {
    const separator = part.indexOf("=");
    if (separator < 1) continue;
    output.set(part.slice(0, separator).trim(), part.slice(separator + 1).trim());
  }
  return output;
}

export function oidcTransactionFromRequest(request: IncomingMessage, key: Buffer): OidcTransaction {
  const value = cookies(request).get(COOKIE_NAME);
  if (!value) throw new Error("OIDC transaction cookie is missing");
  return unsealOidcTransaction(key, value);
}

export function oidcTransactionCookie(key: Buffer, transaction: OidcTransaction): string {
  return `${COOKIE_NAME}=${sealOidcTransaction(key, transaction)}; Path=/v1/auth/oidc/callback; Max-Age=${MAX_AGE_SECONDS}; HttpOnly; Secure; SameSite=Lax`;
}

export function stateMatches(expected: string, actual: string): boolean {
  const left = Buffer.from(expected);
  const right = Buffer.from(actual);
  return left.length === right.length && timingSafeEqual(left, right);
}
