import assert from "node:assert/strict";
import test from "node:test";
import { activationOriginMatches, activationPublicKeyDigest, oidcCookieKey, sealActivationConfirmation, sealOidcTransaction, stateMatches, unsealActivationConfirmation, unsealOidcTransaction } from "./oidc-state.js";

const key = oidcCookieKey(Buffer.alloc(32, 7).toString("base64url"));
const transaction = { state: "state", nonce: "nonce", codeVerifier: "verifier", userCode: "ABCD-EFGH", mode: "signup" as const, issuedAt: 1_800_000_000_000 };

test("OIDC transactions round-trip through authenticated encryption", () => {
  const sealed = sealOidcTransaction(key, transaction);
  assert.deepEqual(unsealOidcTransaction(key, sealed, transaction.issuedAt + 1_000), transaction);
  assert.doesNotMatch(sealed, /state|nonce|verifier|ABCD/);
});

test("activation requires an exact same-origin POST and binds a raw Ed25519 key digest", () => {
	assert.equal(activationOriginMatches("https://api.blazn.example", "https://api.blazn.example/activate"), true);
	for (const origin of [undefined, "https://evil.example", "https://api.blazn.example.evil", "https://api.blazn.example/path", "null"]) assert.equal(activationOriginMatches(origin, "https://api.blazn.example"), false);
	const publicKey = Buffer.alloc(32, 9).toString("base64url");
	assert.match(activationPublicKeyDigest(publicKey), /^sha256:[0-9a-f]{64}$/);
	assert.throws(() => activationPublicKeyDigest("not-a-key"));
});

test("OIDC transaction tampering and expiry fail closed", () => {
  const sealed = sealOidcTransaction(key, transaction);
	const replacement = sealed[0] === "A" ? "B" : "A";
	assert.throws(() => unsealOidcTransaction(key, `${replacement}${sealed.slice(1)}`, transaction.issuedAt + 1_000));
  assert.throws(() => unsealOidcTransaction(key, sealed, transaction.issuedAt + 11 * 60 * 1_000), /expired/);
});

test("OIDC transactions reject non-canonical base64url aliases", () => {
  const sealed = sealOidcTransaction(key, transaction);
  const packed = Buffer.from(sealed, "base64url");
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const alias = [...alphabet]
    .map((character) => `${sealed.slice(0, -1)}${character}`)
    .find((candidate) => candidate !== sealed && Buffer.from(candidate, "base64url").equals(packed));
  assert.ok(alias, "fixture must have a non-canonical base64url alias");
  assert.throws(() => unsealOidcTransaction(key, alias, transaction.issuedAt + 1_000), /invalid/);
});

test("state comparison is exact", () => {
  assert.equal(stateMatches("same", "same"), true);
  assert.equal(stateMatches("same", "different"), false);
});

test("activation confirmation binds authorization, mode, code, and device public-key digest", () => {
	const confirmation = { authorizationId: "authorization-id", userCode: "ABCD-EFGH", mode: "signin" as const, publicKeyDigest: `sha256:${"a".repeat(64)}`, issuedAt: transaction.issuedAt };
	const sealed = sealActivationConfirmation(key, confirmation);
	assert.deepEqual(unsealActivationConfirmation(key, sealed, confirmation.issuedAt + 1_000), confirmation);
	assert.doesNotMatch(sealed, /authorization|ABCD|sha256/);
	assert.throws(() => unsealActivationConfirmation(key, sealOidcTransaction(key, transaction), confirmation.issuedAt + 1_000));
	assert.throws(() => unsealActivationConfirmation(key, sealed, confirmation.issuedAt + 11 * 60 * 1_000), /expired/);
});
