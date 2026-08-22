import assert from "node:assert/strict";
import test from "node:test";
import { HttpError } from "./http.js";
import { SandboxHttpError } from "./sandbox-types.js";
import { isControlHttpError, normalizeControlHttpError } from "./server-errors.js";

test("sandbox HTTP errors retain their exact status and are safe expected failures", () => {
  const sandboxError = new SandboxHttpError("sandbox_access_denied", "sandbox is unavailable");
  assert.equal(isControlHttpError(sandboxError), true);
  assert.equal(normalizeControlHttpError(sandboxError), sandboxError);
  assert.equal(normalizeControlHttpError(sandboxError).status, 404);
});

test("unexpected failures normalize to a redacted internal error", () => {
  const normalized = normalizeControlHttpError(new Error("database password must not escape"));
  assert.ok(normalized instanceof HttpError);
  assert.equal(normalized.code, "internal_error");
  assert.equal(normalized.message, "request failed");
  assert.equal(isControlHttpError(new Error("boom")), false);
});
