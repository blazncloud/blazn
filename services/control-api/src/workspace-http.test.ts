import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";
import type { WorkspaceService } from "./workspace-service.js";
import { WorkspaceHttpRouter } from "./workspace-http.js";
import { WorkspaceHttpError } from "./workspace-types.js";

test("workspace invitation acceptance keeps bearer secret in a bounded JSON body", async () => {
  let accepted = ""; let requestUrl = ""; let headerLeak = "";
  const service = {
    async acceptInvitation(_principal: unknown, _key: string, token: string) {
      accepted = token;
      return { workspace: { id: "00000000-0000-4000-8000-000000000001" } };
    },
  } as unknown as WorkspaceService;
  const router = new WorkspaceHttpRouter(service);
  const server = createServer((request, response) => {
    requestUrl = request.url ?? "";
    headerLeak = JSON.stringify(request.headers);
    const url = new URL(request.url ?? "/", "http://127.0.0.1");
    router.handle(request, response, url, { userId: "00000000-0000-4000-8000-000000000002", email: "user@example.test", displayName: "User" })
      .catch((error: unknown) => {
        const failure = error instanceof WorkspaceHttpError ? error : new WorkspaceHttpError("invalid_request", "failed");
        response.writeHead(failure.status, { "content-type": "application/json" }); response.end(JSON.stringify({ code: failure.code }));
      });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const address = server.address(); if (!address || typeof address === "string") throw new Error("server unavailable");
    const token = "invite-token-value-that-is-longer-than-32-characters";
    const response = await fetch(`http://127.0.0.1:${address.port}/v1/workspace-invitations/accept`, {
      method: "POST", headers: { "content-type": "application/json", "idempotency-key": "accept-key-0001" }, body: JSON.stringify({ inviteToken: token }),
    });
    assert.equal(response.status, 200);
    assert.equal(accepted, token);
    assert.doesNotMatch(requestUrl, /invite-token/);
    assert.doesNotMatch(headerLeak, /invite-token/);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test("workspace event stream reauthorizes and closes after membership removal", async () => {
  let polls = 0; let reauthorizations = 0;
  const service = {
    async eventBatch() {
      polls++;
      if (polls > 1) throw new WorkspaceHttpError("workspace_not_found", "removed");
      return [];
    },
  } as unknown as WorkspaceService;
  const router = new WorkspaceHttpRouter(service);
  const principal = { userId: "00000000-0000-4000-8000-000000000002", email: "user@example.test", displayName: "User" };
  const server = createServer((request, response) => {
    const url = new URL(request.url ?? "/", "http://127.0.0.1");
    router.handle(request, response, url, principal, async () => { reauthorizations++; return principal; }).catch(() => response.destroy());
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const address = server.address(); if (!address || typeof address === "string") throw new Error("server unavailable");
    const response = await fetch(`http://127.0.0.1:${address.port}/v1/workspaces/00000000-0000-4000-8000-000000000001/events`);
    assert.equal(response.status, 200);
    const stream = await response.text();
    assert.match(stream, /event: membership_revoked/);
    assert.ok(reauthorizations >= 1);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
