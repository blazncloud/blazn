import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

const gate = fileURLToPath(new URL("./provider-gate.mjs", import.meta.url));

test("withdraws Login authorization when a provider becomes active after startup", async () => {
  const directory = await mkdtemp(join(tmpdir(), "blazn-provider-gate-test."));
  const tokenPath = join(directory, "login-client.pat");
  const statePath = join(directory, "providers.json");
  const preloadPath = join(directory, "fetch.mjs");
  const port = 31_000 + Math.floor(Math.random() * 1_000);
  await writeFile(tokenPath, "gate-test-token-that-must-not-leak\n", { mode: 0o600 });
  await writeFile(statePath, "[]\n", { mode: 0o600 });
  await writeFile(preloadPath, `import { readFile } from "node:fs/promises"; globalThis.fetch = async (url) => new Response(JSON.stringify(new URL(url).pathname === "/v2/organizations/_search" ? { details: { totalResult: "1" }, result: [{ id: "123456789", state: "ORGANIZATION_STATE_ACTIVE" }] } : { details: {}, identityProviders: JSON.parse(await readFile(process.env.MOCK_IDP_STATE_FILE, "utf8")) }), { status: 200, headers: { "content-type": "application/json" } });\n`);
  const child = spawn(process.execPath, ["--import", preloadPath, gate], {
    env: {
      ...process.env,
      ZITADEL_API_URL: "http://zitadel-api:8080",
      BLAZN_ZITADEL_DOMAIN: "identity.example.test",
      ZITADEL_SERVICE_USER_TOKEN_FILE: tokenPath,
      BLAZN_PROVIDER_GATE_PORT: String(port),
      MOCK_IDP_STATE_FILE: statePath,
    },
    stdio: ["ignore", "ignore", "pipe"],
  });
  const closed = new Promise((resolve) => child.once("close", resolve));
  let stderr = "";
  child.stderr.setEncoding("utf8").on("data", (value) => { stderr += value; });
  try {
    let healthy;
    for (let attempt = 0; attempt < 40; attempt += 1) {
      try {
        healthy = await fetch(`http://127.0.0.1:${port}/healthz`);
        break;
      } catch {
        await new Promise((resolve) => setTimeout(resolve, 25));
      }
    }
    assert.equal(healthy?.status, 200, stderr);
    assert.equal((await fetch(`http://127.0.0.1:${port}/authorize`, { method: "POST" })).status, 204);

    await writeFile(statePath, '[{"id":"provider-1"}]\n', { mode: 0o600 });
    const blocked = await fetch(`http://127.0.0.1:${port}/authorize`, { method: "POST" });
    assert.equal(blocked.status, 503);
    assert.equal(await blocked.text(), "identity login is temporarily unavailable\n");
    assert.doesNotMatch(stderr, /gate-test-token-that-must-not-leak/);
  } finally {
    if (child.exitCode === null) child.kill("SIGTERM");
    await closed;
    await rm(directory, { recursive: true, force: true });
  }
});
