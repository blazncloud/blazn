import assert from "node:assert/strict";
import test from "node:test";
import { oidcCookieKey, sealOidcTransaction, stateMatches, unsealOidcTransaction } from "./oidc-state.js";

const key = oidcCookieKey(Buffer.alloc(32, 7).toString("base64url"));
const transaction = { state: "state", nonce: "nonce", codeVerifier: "verifier", userCode: "ABCD-EFGH", mode: "signup" as const, issuedAt: 1_800_000_000_000 };

test("OIDC transactions round-trip through authenticated encryption", () => {
  const sealed = sealOidcTransaction(key, transaction);
  assert.deepEqual(unsealOidcTransaction(key, sealed, transaction.issuedAt + 1_000), transaction);
  assert.doesNotMatch(sealed, /state|nonce|verifier|ABCD/);
});

test("OIDC transaction tampering and expiry fail closed", () => {
  const sealed = sealOidcTransaction(key, transaction);
  const replacement = sealed.at(-1) === "A" ? "B" : "A";
  assert.throws(() => unsealOidcTransaction(key, `${sealed.slice(0, -1)}${replacement}`, transaction.issuedAt + 1_000));
  assert.throws(() => unsealOidcTransaction(key, sealed, transaction.issuedAt + 11 * 60 * 1_000), /expired/);
});

test("state comparison is exact", () => {
  assert.equal(stateMatches("same", "same"), true);
  assert.equal(stateMatches("same", "different"), false);
});
