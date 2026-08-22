import assert from "node:assert/strict";
import test from "node:test";
import { sessionAccessError } from "./session-state.js";

const now = Date.parse("2026-08-22T00:00:00Z");

test("access expiry is recoverable and distinct from revocation", () => {
  assert.equal(sessionAccessError({ sessionRevokedAt: null, deviceRevokedAt: null, accessExpiresAt: new Date(now - 1) }, now), "access_expired");
  assert.equal(sessionAccessError({ sessionRevokedAt: new Date(now), deviceRevokedAt: null, accessExpiresAt: new Date(now + 1) }, now), "session_revoked");
  assert.equal(sessionAccessError({ sessionRevokedAt: null, deviceRevokedAt: new Date(now), accessExpiresAt: new Date(now + 1) }, now), "device_revoked");
  assert.equal(sessionAccessError({ sessionRevokedAt: null, deviceRevokedAt: null, accessExpiresAt: new Date(now + 1) }, now), undefined);
});
