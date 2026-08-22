import { createHash, createPublicKey, randomBytes, scrypt as scryptCallback, timingSafeEqual, verify } from "node:crypto";
import { promisify } from "node:util";

const scrypt = promisify(scryptCallback);

export function randomToken(bytes = 32): string {
  return randomBytes(bytes).toString("base64url");
}

export function tokenHash(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

export function userCode(): string {
  const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
  const bytes = randomBytes(8);
  const part = (offset: number) => Array.from(bytes.subarray(offset, offset + 4), (value) => alphabet[value % alphabet.length]).join("");
  return `${part(0)}-${part(4)}`;
}

export async function passwordRecord(password: string): Promise<{ salt: string; hash: string }> {
  if (password.length < 12) throw new Error("password must contain at least 12 characters");
  const salt = randomBytes(16).toString("base64url");
  const derived = (await scrypt(password, salt, 32)) as Buffer;
  return { salt, hash: derived.toString("base64url") };
}

export async function verifyPassword(password: string, salt: string, expected: string): Promise<boolean> {
  const derived = (await scrypt(password, salt, 32)) as Buffer;
  const expectedBytes = Buffer.from(expected, "base64url");
  return expectedBytes.length === derived.length && timingSafeEqual(expectedBytes, derived);
}

export function verifyDeviceProof(publicKey: string, canonical: string, signature: string): boolean {
  try {
    const key = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: publicKey }, format: "jwk" });
    return verify(null, Buffer.from(canonical, "utf8"), key, Buffer.from(signature, "base64url"));
  } catch {
    return false;
  }
}

export function sessionRevokePayload(deviceId: string): string {
  return `blazn-session-revoke-v1\n${deviceId}`;
}

export function secretMatches(actual: string, expected: string): boolean {
  const actualDigest = Buffer.from(tokenHash(actual), "hex");
  const expectedDigest = Buffer.from(tokenHash(expected), "hex");
  return timingSafeEqual(actualDigest, expectedDigest);
}
