import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./assert-no-active-idps.mjs", import.meta.url));

async function runProbe({ organizationStatus = 200, organizationBody = { details: { totalResult: "1" }, result: [{ id: "123456789", state: "ORGANIZATION_STATE_ACTIVE" }] }, providerStatus = 200, providerBody = { details: {} } }) {
  const directory = await mkdtemp(join(tmpdir(), "blazn-active-idps-test."));
  const tokenPath = join(directory, "login-client.pat");
  const preloadPath = join(directory, "fetch.mjs");
  await writeFile(tokenPath, "test-token-that-must-not-leak\n", { mode: 0o600 });
  await writeFile(preloadPath, `globalThis.fetch = async (url) => { const organizations = new URL(url).pathname === "/v2/organizations/_search"; return new Response(organizations ? ${JSON.stringify(JSON.stringify(organizationBody))} : ${JSON.stringify(JSON.stringify(providerBody))}, { status: organizations ? ${organizationStatus} : ${providerStatus}, headers: { "content-type": "application/json" } }); };\n`);
  try {
    return await new Promise((resolve, reject) => {
      const child = spawn(process.execPath, ["--import", preloadPath, script], {
        env: {
          ...process.env,
          ZITADEL_API_URL: "http://zitadel-api:8080",
          BLAZN_ZITADEL_DOMAIN: "identity.example.test",
          ZITADEL_SERVICE_USER_TOKEN_FILE: tokenPath,
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      let stdout = "", stderr = "";
      child.stdout.setEncoding("utf8").on("data", (value) => { stdout += value; });
      child.stderr.setEncoding("utf8").on("data", (value) => { stderr += value; });
      child.once("error", reject);
      child.once("close", (code) => resolve({ code, stdout, stderr }));
    });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

test("accepts and receipts an empty active-provider inventory", async () => {
  const result = await runProbe({});
  assert.equal(result.code, 0, result.stderr);
  const evidence = JSON.parse(result.stdout);
  assert.equal(evidence.schemaVersion, "blazn.identity.active-idps/v1");
  assert.equal(evidence.organizationCount, 1);
  assert.equal(evidence.activeProviderCount, 0);
  assert.ok(Number.isFinite(Date.parse(evidence.observedAt)));
});

test("rejects any active external provider without leaking the token", async () => {
  const result = await runProbe({ providerBody: { details: {}, identityProviders: [{ id: "provider-1" }] } });
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /1 active provider\(s\) are not allowed/);
  assert.doesNotMatch(result.stderr, /test-token-that-must-not-leak/);
});

test("rejects a second organization before its providers can bypass the gate", async () => {
  const result = await runProbe({ organizationBody: { details: { totalResult: "2" }, result: [{ id: "123456789", state: "ORGANIZATION_STATE_ACTIVE" }, { id: "987654321", state: "ORGANIZATION_STATE_ACTIVE" }] } });
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /exactly one organization is required/);
});

test("fails closed when the organization inventory endpoint is unavailable", async () => {
  const result = await runProbe({ organizationStatus: 503, organizationBody: { code: "unavailable" } });
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /organization inventory returned HTTP 503/);
});
