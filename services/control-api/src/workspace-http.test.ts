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

test("recognized workspace routes reject unsupported methods with 405", async () => {
  const router = new WorkspaceHttpRouter({} as WorkspaceService);
  const server = workspaceServer(router);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const origin = serverOrigin(server);
    for (const path of [
      "/v1/workspaces",
      "/v1/workspace-invitations/accept",
      "/v1/workspaces/not-a-uuid",
      "/v1/workspaces/not-a-uuid/invitations",
      "/v1/workspaces/not-a-uuid/invitations/not-a-uuid",
      "/v1/workspaces/not-a-uuid/members",
      "/v1/workspaces/not-a-uuid/members/not-a-uuid",
      "/v1/workspaces/not-a-uuid/membership",
      "/v1/workspaces/not-a-uuid/events",
    ]) {
      const response = await fetch(origin + path, { method: "OPTIONS" });
      assert.equal(response.status, 405, path);
      assert.equal((await response.json() as { code: string }).code, "method_not_allowed", path);
    }
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test("workspace stream caps reject deterministically and release disconnected slots", async () => {
  const service = { async eventBatch() { return []; } } as unknown as WorkspaceService;
  const router = new WorkspaceHttpRouter(service, { globalLimit: 2, perUserLimit: 1, pollIntervalMs: 10, idleTimeoutMs: 2_000, maxLifetimeMs: 2_000 });
  const server = workspaceServer(router);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const path = "/v1/workspaces/00000000-0000-4000-8000-000000000001/events";
  try {
    const origin = serverOrigin(server);
    const first = await fetch(origin + path, { headers: { "x-test-user": "00000000-0000-4000-8000-000000000002" } });
    assert.equal(first.status, 200);
    const sameUser = await fetch(origin + path, { headers: { "x-test-user": "00000000-0000-4000-8000-000000000002" } });
    assert.equal(sameUser.status, 429);
    assert.equal((await sameUser.json() as { code: string }).code, "rate_limited");

    const second = await fetch(origin + path, { headers: { "x-test-user": "00000000-0000-4000-8000-000000000003" } });
    assert.equal(second.status, 200);
    const globalExcess = await fetch(origin + path, { headers: { "x-test-user": "00000000-0000-4000-8000-000000000004" } });
    assert.equal(globalExcess.status, 429);
    assert.equal((await globalExcess.json() as { code: string }).code, "rate_limited");

    await first.body?.cancel();
    await second.body?.cancel();
    let released: Response | undefined;
    for (let attempt = 0; attempt < 20; attempt++) {
      released = await fetch(origin + path, { headers: { "x-test-user": "00000000-0000-4000-8000-000000000004" } });
      if (released.status === 200) break;
      await released.body?.cancel();
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    assert.equal(released?.status, 200);
    await released?.body?.cancel();
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test("workspace streams enforce idle and absolute lifetime bounds", async () => {
  let sequence = 0;
  const service = {
    async eventBatch() {
      sequence++;
      return sequence % 2 === 0 ? [{ id: String(sequence), type: "test", payload: {} }] : [];
    },
  } as unknown as WorkspaceService;
  const router = new WorkspaceHttpRouter(service, { pollIntervalMs: 5, idleTimeoutMs: 1_000, maxLifetimeMs: 50 });
  const server = workspaceServer(router);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const started = Date.now();
    const response = await fetch(serverOrigin(server) + "/v1/workspaces/00000000-0000-4000-8000-000000000001/events");
    assert.equal(response.status, 200);
    assert.match(await response.text(), /event: ready/);
    assert.ok(Date.now() - started < 500, "absolute stream lifetime was not enforced");

    const idleRouter = new WorkspaceHttpRouter({ async eventBatch() { return []; } } as unknown as WorkspaceService, { pollIntervalMs: 5, idleTimeoutMs: 40, maxLifetimeMs: 1_000 });
    const idleServer = workspaceServer(idleRouter);
    await new Promise<void>((resolve) => idleServer.listen(0, "127.0.0.1", resolve));
    try {
      const idleStarted = Date.now();
      const idleResponse = await fetch(serverOrigin(idleServer) + "/v1/workspaces/00000000-0000-4000-8000-000000000001/events");
      assert.match(await idleResponse.text(), /heartbeat/);
      assert.ok(Date.now() - idleStarted < 500, "idle stream lifetime was not enforced");
    } finally {
      await new Promise<void>((resolve) => idleServer.close(() => resolve()));
    }
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

function workspaceServer(router: WorkspaceHttpRouter) {
  return createServer((request, response) => {
    const url = new URL(request.url ?? "/", "http://127.0.0.1");
    const userId = typeof request.headers["x-test-user"] === "string" ? request.headers["x-test-user"] : "00000000-0000-4000-8000-000000000002";
    const principal = { userId, email: "user@example.test", displayName: "User" };
    router.handle(request, response, url, principal, async () => principal).catch((error: unknown) => {
      const failure = error instanceof WorkspaceHttpError ? error : new WorkspaceHttpError("invalid_request", "failed");
      if (!response.headersSent) {
        response.writeHead(failure.status, { "content-type": "application/json" });
        response.end(JSON.stringify({ code: failure.code }));
      } else response.end();
    });
  });
}

function serverOrigin(server: ReturnType<typeof createServer>): string {
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("server unavailable");
  return `http://127.0.0.1:${address.port}`;
}
