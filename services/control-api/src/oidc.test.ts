import assert from "node:assert/strict";
import { generateKeyPairSync, sign } from "node:crypto";
import test from "node:test";
import { OidcClient, verifyOidcIdToken, type Jwk } from "./oidc.js";

const pair = generateKeyPairSync("rsa", { modulusLength: 2048 });
const jwk = pair.publicKey.export({ format: "jwk" });
Object.assign(jwk, { kid: "test-key", use: "sig", alg: "RS256" });

function token(overrides: Record<string, unknown> = {}): string {
  const now = 1_800_000_000;
  const header = Buffer.from(JSON.stringify({ alg: "RS256", kid: "test-key", typ: "JWT" })).toString("base64url");
  const payload = Buffer.from(JSON.stringify({ iss: "https://identity.blazn.example/", aud: "client-id", sub: "zitadel-user-id", email: "USER@example.com", email_verified: true, name: "Blaze User", nonce: "nonce", acr: "urn:zitadel:blazn:aal2", amr: ["pwd", "otp"], iat: now - 5, exp: now + 300, ...overrides })).toString("base64url");
  const signature = sign("RSA-SHA256", Buffer.from(`${header}.${payload}`), pair.privateKey).toString("base64url");
  return `${header}.${payload}.${signature}`;
}

const verification = { issuer: "https://identity.blazn.example/", clientId: "client-id", nonce: "nonce", assurancePolicy: { provider: "zitadel" as const, reviewedRelease: "v4.17.1", policyDigest: `sha256:${"a".repeat(64)}`, acrValues: ["urn:zitadel:blazn:aal2"], acceptedAmrSets: [["pwd", "otp"], ["pwd", "webauthn"]] }, keys: [jwk as Jwk], now: 1_800_000_000 };

test("verified OIDC identity requires signature, issuer, audience, nonce, email, and MFA", () => {
  assert.deepEqual(verifyOidcIdToken(token(), verification), { issuer: verification.issuer, subject: "zitadel-user-id", email: "user@example.com", displayName: "Blaze User", amr: ["pwd", "otp"], acr: "urn:zitadel:blazn:aal2", reviewedRelease: "v4.17.1", assurancePolicyDigest: `sha256:${"a".repeat(64)}` });
  assert.throws(() => verifyOidcIdToken(token({ nonce: "wrong" }), verification), /claims/);
  assert.throws(() => verifyOidcIdToken(token({ email_verified: false }), verification), /verified email/);
  assert.throws(() => verifyOidcIdToken(token({ amr: ["pwd"] }), verification), /multi-factor/);
	assert.throws(() => verifyOidcIdToken(token({ amr: ["mfa"] }), verification), /assurance/);
	assert.throws(() => verifyOidcIdToken(token({ acr: "urn:zitadel:unreviewed", amr: ["pwd", "otp"] }), verification), /assurance/);
	assert.doesNotThrow(() => verifyOidcIdToken(token({ amr: ["pwd", "webauthn"] }), verification));
});

test("OIDC identity rejects an altered signature", () => {
  const segments = token().split(".");
  const signature = Buffer.from(segments[2]!, "base64url");
  signature[0] = signature[0]! ^ 1;
  assert.throws(() => verifyOidcIdToken(`${segments[0]}.${segments[1]}.${signature.toString("base64url")}`, verification), /signature/);
});

test("OIDC health validates discovery and signing keys and requests reviewed ACR values", async () => {
	const originalFetch = globalThis.fetch;
	const calls: string[] = [];
	globalThis.fetch = (async (input: string | URL | Request) => {
		const url = String(input);
		calls.push(url);
		if (url.endsWith("/.well-known/openid-configuration")) return new Response(JSON.stringify({ issuer: verification.issuer, authorization_endpoint: `${verification.issuer}oauth/v2/authorize`, token_endpoint: `${verification.issuer}oauth/v2/token`, jwks_uri: `${verification.issuer}oauth/v2/keys` }), { status: 200 });
		if (url.endsWith("/oauth/v2/keys")) return new Response(JSON.stringify({ keys: [jwk] }), { status: 200 });
		throw new Error(`unexpected fetch ${url}`);
	}) as typeof fetch;
	try {
		const client = new OidcClient({ issuerUrl: verification.issuer, clientId: verification.clientId, clientSecret: "secret", callbackUrl: "https://api.blazn.example/v1/auth/oidc/callback", assurancePolicy: verification.assurancePolicy });
		await Promise.all(Array.from({ length: 20 }, () => client.health()));
		const authorization = await client.authorizationUrl(client.createTransaction("ABCD-EFGH", "signin"));
		assert.equal(authorization.searchParams.get("acr_values"), verification.assurancePolicy.acrValues.join(" "));
		assert.equal(calls.length, 2);
		await client.health();
		assert.equal(calls.length, 2, "health bypassed bounded cache or singleflight");
	} finally { globalThis.fetch = originalFetch; }
});

test("OIDC discovery streaming is cancelled at the hard byte cap", async () => {
	const originalFetch = globalThis.fetch;
	let cancelled = false;
	globalThis.fetch = (async () => new Response(new ReadableStream({
		start(controller) {
			controller.enqueue(new Uint8Array(700_000));
			controller.enqueue(new Uint8Array(700_000));
		},
		cancel() { cancelled = true; }
	}), { status: 200 })) as typeof fetch;
	try {
		const client = new OidcClient({ issuerUrl: verification.issuer, clientId: verification.clientId, clientSecret: "secret", callbackUrl: "https://api.blazn.example/v1/auth/oidc/callback", assurancePolicy: verification.assurancePolicy });
		await assert.rejects(client.health(), /too large/);
		assert.equal(cancelled, true);
	} finally { globalThis.fetch = originalFetch; }
});
