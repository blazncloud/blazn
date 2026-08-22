import assert from "node:assert/strict";
import test from "node:test";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { SandboxHttpRouter } from "./sandbox-http.js";
import { routeSandboxRequest } from "./sandbox-server-routing.js";

test("sandbox integration routes matches and preserves authenticated session identity", async () => {
  let principal: unknown;
  const router = {
    matches: (path: string) => path === "/v1/sandboxes/00000000-0000-4000-8000-000000000001",
    handle: async (_request: IncomingMessage, _response: ServerResponse, _url: URL, authenticate: () => Promise<unknown>) => {
      principal = await authenticate();
    },
  } as unknown as SandboxHttpRouter;
  const handled = await routeSandboxRequest(
    router,
    {} as IncomingMessage,
    {} as ServerResponse,
    new URL("http://localhost/v1/sandboxes/00000000-0000-4000-8000-000000000001"),
    async () => ({ sessionId: "session-1", userId: "user-1", email: "user@example.test", displayName: "Test User" }),
  );
  assert.equal(handled, true);
  assert.deepEqual(principal, { sessionId: "session-1", userId: "user-1", email: "user@example.test", displayName: "Test User" });
});

test("sandbox integration leaves unmatched routes to existing routers", async () => {
  let authenticated = false;
  const router = { matches: () => false } as unknown as SandboxHttpRouter;
  const handled = await routeSandboxRequest(router, {} as IncomingMessage, {} as ServerResponse, new URL("http://localhost/v1/projects"), async () => {
    authenticated = true;
    return { sessionId: "s", userId: "u", email: "e", displayName: "n" };
  });
  assert.equal(handled, false);
  assert.equal(authenticated, false);
});
