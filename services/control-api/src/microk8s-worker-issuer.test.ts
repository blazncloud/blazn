import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { UnixMicroK8sWorkerCredentialIssuer } from "./microk8s-worker-issuer.js";

const issue = {
  issuanceId: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  clusterId: "cluster-a",
  expectedNodeName: "worker-a",
  bootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule" as const,
  ttlSeconds: 60,
  workerOnly: true as const,
};

async function fake(handler: (body: Record<string, unknown>) => unknown, stall = false): Promise<{ socket: string; server: Server; close(): Promise<void> }> {
  const root = await mkdtemp(join(tmpdir(), "blazn-issuer-test-"));
  const socket = join(root, "issuer.sock");
  const server = createServer((request, response) => {
    const chunks: Buffer[] = [];
    request.on("data", (chunk: Buffer) => chunks.push(chunk));
    request.on("end", () => {
      if (stall) return;
      response.setHeader("content-type", "application/json");
      if (request.method === "GET" && request.url === "/healthz") { response.end(JSON.stringify({ schemaVersion: "blazn.dev/microk8s-worker-issuer/v1", operation: "health", healthy: true })); return; }
      response.end(JSON.stringify(handler(JSON.parse(Buffer.concat(chunks).toString("utf8")) as Record<string, unknown>)));
    });
  });
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(socket, resolve); });
  return { socket, server, close: async () => { await new Promise<void>((resolve) => server.close(() => resolve())); await rm(root, { recursive: true }); } };
}

test("Unix issuer sends the closed binding and accepts canonical issue and revoke responses", async () => {
  const seen: Record<string, unknown>[] = [];
  const fixture = await fake((body) => {
    seen.push(body);
    return body.operation === "issue" ? {
      schemaVersion: "blazn.dev/microk8s-worker-issuer/v1", operation: "issue", providerHandle: issue.issuanceId,
      credential: "A".repeat(43), clusterId: issue.clusterId, clusterHealthy: true, workerOnly: true,
      expiresAt: "2030-01-01T00:01:00.000Z",
    } : body.operation === "observe" ? { schemaVersion: "blazn.dev/microk8s-worker-issuer/v1", operation: "observe", issuanceId: issue.issuanceId, clusterId: issue.clusterId, nodeName: issue.expectedNodeName, nodeUid: "uid-a", resourceVersion: "17", bootstrapTainted: true, workerOnly: true } : { schemaVersion: "blazn.dev/microk8s-worker-issuer/v1", operation: "revoke", providerHandle: issue.issuanceId, revoked: true };
  });
  try {
    const issuer = new UnixMicroK8sWorkerCredentialIssuer(fixture.socket);
    const result = await issuer.issue(issue, new AbortController().signal);
    assert.equal(result.providerHandle, issue.issuanceId);
    const observed=await issuer.observe({issuanceId:issue.issuanceId,clusterId:issue.clusterId,expectedNodeName:issue.expectedNodeName,bootstrapTaint:issue.bootstrapTaint},new AbortController().signal);
    assert.equal(observed.nodeUid,"uid-a");
    await issuer.revoke(issue.issuanceId, new AbortController().signal);
    await issuer.health(new AbortController().signal);
    assert.deepEqual(seen[0], { schemaVersion: "blazn.dev/microk8s-worker-issuer/v1", operation: "issue", ...issue });
  } finally { await fixture.close(); }
});

test("Unix issuer rejects extra response fields and cluster substitution", async () => {
  for (const mutation of [{ extra: true }, { clusterId: "other" }]) {
    const fixture = await fake(() => ({ schemaVersion: "blazn.dev/microk8s-worker-issuer/v1", operation: "issue",
      providerHandle: issue.issuanceId, credential: "A".repeat(43), clusterId: issue.clusterId,
      clusterHealthy: true, workerOnly: true, expiresAt: "2030-01-01T00:01:00.000Z", ...mutation }));
    try { await assert.rejects(new UnixMicroK8sWorkerCredentialIssuer(fixture.socket).issue(issue, new AbortController().signal), /invalid response/); }
    finally { await fixture.close(); }
  }
});

test("Unix issuer propagates cancellation without exposing response material", async () => {
  const fixture = await fake(() => ({}), true);
  try {
    const controller = new AbortController();
    const pending = new UnixMicroK8sWorkerCredentialIssuer(fixture.socket).issue(issue, controller.signal);
    controller.abort();
    await assert.rejects(pending, /abort/i);
  } finally { await fixture.close(); }
});
