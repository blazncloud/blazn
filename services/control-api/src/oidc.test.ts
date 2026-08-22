import assert from "node:assert/strict";
import { generateKeyPairSync, sign } from "node:crypto";
import test from "node:test";
import { verifyOidcIdToken } from "./oidc.js";

const pair = generateKeyPairSync("rsa", { modulusLength: 2048 });
const jwk = pair.publicKey.export({ format: "jwk" });
Object.assign(jwk, { kid: "test-key", use: "sig", alg: "RS256" });

function token(overrides: Record<string, unknown> = {}): string {
  const now = 1_800_000_000;
  const header = Buffer.from(JSON.stringify({ alg: "RS256", kid: "test-key", typ: "JWT" })).toString("base64url");
  const payload = Buffer.from(JSON.stringify({ iss: "https://identity.blazn.example/", aud: "client-id", sub: "auth0|user", email: "USER@example.com", email_verified: true, name: "Blaze User", nonce: "nonce", amr: ["pwd", "mfa"], iat: now - 5, exp: now + 300, ...overrides })).toString("base64url");
  const signature = sign("RSA-SHA256", Buffer.from(`${header}.${payload}`), pair.privateKey).toString("base64url");
  return `${header}.${payload}.${signature}`;
}

const verification = { issuer: "https://identity.blazn.example/", clientId: "client-id", nonce: "nonce", requireMfa: true, keys: [jwk], now: 1_800_000_000 };

test("verified OIDC identity requires signature, issuer, audience, nonce, email, and MFA", () => {
  assert.deepEqual(verifyOidcIdToken(token(), verification), { issuer: verification.issuer, subject: "auth0|user", email: "user@example.com", displayName: "Blaze User", amr: ["pwd", "mfa"] });
  assert.throws(() => verifyOidcIdToken(token({ nonce: "wrong" }), verification), /claims/);
  assert.throws(() => verifyOidcIdToken(token({ email_verified: false }), verification), /verified email/);
  assert.throws(() => verifyOidcIdToken(token({ amr: ["pwd"] }), verification), /multi-factor/);
});

test("OIDC identity rejects an altered signature", () => {
  const encoded = token();
  assert.throws(() => verifyOidcIdToken(`${encoded.slice(0, -1)}${encoded.endsWith("A") ? "B" : "A"}`, verification), /signature/);
});
