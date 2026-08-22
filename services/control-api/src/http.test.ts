import assert from "node:assert/strict";
import { Readable } from "node:stream";
import test from "node:test";
import { HttpError, jsonBody, requireExactKeys, requiredString } from "./http.js";

test("jsonBody accepts an object", async () => {
  const request = Readable.from([Buffer.from('{"name":"device"}')]);
  assert.deepEqual(await jsonBody(request as never), { name: "device" });
});

test("jsonBody rejects arrays and oversized input", async () => {
  await assert.rejects(jsonBody(Readable.from(["[]"]) as never), (error: unknown) => error instanceof HttpError && error.status === 400);
  await assert.rejects(jsonBody(Readable.from(["12345"]) as never, 4), (error: unknown) => error instanceof HttpError && error.status === 413);
});

test("requiredString trims and bounds fields", () => {
  assert.equal(requiredString({ name: "  device  " }, "name"), "device");
  assert.throws(() => requiredString({ name: "" }, "name"), HttpError);
  assert.throws(() => requiredString({ name: "long" }, "name", 3), HttpError);
});

test("requireExactKeys rejects missing and additional request fields", () => {
  assert.doesNotThrow(() => requireExactKeys({ one: 1, two: 2 }, ["one", "two"]));
  assert.throws(() => requireExactKeys({ one: 1 }, ["one", "two"]), HttpError);
  assert.throws(() => requireExactKeys({ one: 1, two: 2, extra: 3 }, ["one", "two"]), HttpError);
});
