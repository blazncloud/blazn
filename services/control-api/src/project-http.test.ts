import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import test from "node:test";
import { ProjectHttpRouter } from "./project-http.js";
import type { ProjectService } from "./project-service.js";
import { ProjectHttpError } from "./project-types.js";

const workspaceId = "00000000-0000-4000-8000-000000000001";
const projectId = "00000000-0000-4000-8000-000000000002";
const principal = { userId: "00000000-0000-4000-8000-000000000003", email: "user@example.test", displayName: "User" };

function fixture() {
  return { id: projectId, workspaceId, slug: "launch-video", kind: "content", name: "Launch Video", description: "", status: "active" as const, version: 1, createdBy: principal.userId, createdAt: "2026-08-22T00:00:00Z", updatedAt: "2026-08-22T00:00:00Z" };
}

test("Project HTTP routes normalize requests and preserve explicit idempotency", async () => {
  const calls: Array<{ method: string; values: unknown[] }> = [];
  const service = {
    async createProject(...values: unknown[]) { calls.push({ method: "create", values }); return { project: fixture() }; },
    async listProjects(...values: unknown[]) { calls.push({ method: "list", values }); return { items: [fixture()], nextCursor: null }; },
    async getProject(...values: unknown[]) { calls.push({ method: "get", values }); return { project: fixture() }; },
    async updateProject(...values: unknown[]) { calls.push({ method: "update", values }); return { project: { ...fixture(), version: 2, status: "archived" as const } }; },
    async getProjectProfile(...values:unknown[]){calls.push({method:"getProfile",values});return{profile:{kind:"content"}};},
    async putProjectProfile(...values:unknown[]){calls.push({method:"putProfile",values});return{profile:{kind:"content"}};},
  } as unknown as ProjectService;
  const server = projectServer(new ProjectHttpRouter(service));
  await listen(server);
  try {
    const origin = serverOrigin(server);
    let response = await fetch(`${origin}/v1/workspaces/${workspaceId}/projects`, { method: "POST", headers: { "content-type": "application/json", "idempotency-key": "project-create-1" }, body: JSON.stringify({ name: " Launch Video ", kind: "content", description: "" }) });
    assert.equal(response.status, 201);
    response = await fetch(`${origin}/v1/workspaces/${workspaceId}/projects?status=all&cursor=${projectId}`);
    assert.equal(response.status, 200);
    response = await fetch(`${origin}/v1/workspaces/${workspaceId}/projects/${projectId}`);
    assert.equal(response.status, 200);
    response = await fetch(`${origin}/v1/workspaces/${workspaceId}/projects/${projectId}`, { method: "PATCH", headers: { "content-type": "application/json", "idempotency-key": "project-update-1" }, body: JSON.stringify({ expectedVersion: 1, status: "archived" }) });
    assert.equal(response.status, 200);
    response=await fetch(`${origin}/v1/workspaces/${workspaceId}/projects/${projectId}/profiles/content`);assert.equal(response.status,200);
    response=await fetch(`${origin}/v1/workspaces/${workspaceId}/projects/${projectId}/profiles/content`,{method:"PUT",headers:{"content-type":"application/json","idempotency-key":"profile-put-1"},body:JSON.stringify({schemaVersion:"blazn.content/project/v1alpha1",draftId:"00000000-0000-4000-8000-000000000004",artifactId:"00000000-0000-4000-8000-000000000005",digest:`sha256:${"a".repeat(64)}`,status:"active",expectedVersion:0})});assert.equal(response.status,200);
    assert.deepEqual(calls.map((call) => call.method), ["create", "list", "get", "update","getProfile","putProfile"]);
    assert.equal(calls[0]!.values[2], "project-create-1");
    assert.deepEqual(calls[1]!.values.slice(2), ["all", projectId]);
  } finally {
    await close(server);
  }
});

test("Project HTTP rejects unknown fields, no-op updates, unsafe IDs, and methods", async () => {
  const service = {
    async createProject() { throw new Error("must not call"); }, async listProjects() { throw new Error("must not call"); },
    async getProject() { throw new Error("must not call"); }, async updateProject() { throw new Error("must not call"); },
  } as unknown as ProjectService;
  const server = projectServer(new ProjectHttpRouter(service));
  await listen(server);
  try {
    const origin = serverOrigin(server);
    const cases: Array<[string, RequestInit]> = [
      [`${origin}/v1/workspaces/${workspaceId}/projects`, { method: "POST", headers: { "content-type": "application/json", "idempotency-key": "project-create-1" }, body: JSON.stringify({ name: "Project", apiKey: "must-not-pass" }) }],
      [`${origin}/v1/workspaces/${workspaceId}/projects`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ name: "Project" }) }],
      [`${origin}/v1/workspaces/${workspaceId}/projects/${projectId}`, { method: "PATCH", headers: { "content-type": "application/json", "idempotency-key": "project-update-1" }, body: JSON.stringify({ expectedVersion: 1 }) }],
      [`${origin}/v1/workspaces/not-a-uuid/projects`, { method: "GET" }],
      [`${origin}/v1/workspaces/${workspaceId}/projects`, { method: "OPTIONS" }],
      [`${origin}/v1/workspaces/${workspaceId}/projects?status=active&status=all`, { method: "GET" }],
    ];
    for (const [url, init] of cases) {
      const response = await fetch(url, init);
      assert.ok(response.status >= 400, `${init.method} ${url}`);
    }
  } finally {
    await close(server);
  }
});

function projectServer(router: ProjectHttpRouter) {
  return createServer((request, response) => {
    const url = new URL(request.url ?? "/", "http://127.0.0.1");
    router.handle(request, response, url, principal).catch((error: unknown) => {
      const failure = error instanceof ProjectHttpError ? error : new ProjectHttpError("invalid_request", "failed");
      response.writeHead(failure.status, { "content-type": "application/json" });
      response.end(JSON.stringify({ code: failure.code }));
    });
  });
}

async function listen(server: ReturnType<typeof createServer>): Promise<void> {
  await new Promise<void>((resolve, reject) => server.listen(0, "127.0.0.1", resolve).once("error", reject));
}

async function close(server: ReturnType<typeof createServer>): Promise<void> {
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}

function serverOrigin(server: ReturnType<typeof createServer>): string {
  const address = server.address() as AddressInfo;
  return `http://127.0.0.1:${address.port}`;
}
