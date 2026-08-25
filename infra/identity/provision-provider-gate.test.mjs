import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./provision-provider-gate.mjs", import.meta.url));

test("replaces the placeholder with an independently authorized gate token", async () => {
  const directory = await mkdtemp(join(tmpdir(), "blazn-provider-provision."));
  const loginPath = join(directory, "login.pat");
  const gatePath = join(directory, "gate.pat");
  const preloadPath = join(directory, "fetch.mjs");
  await writeFile(loginPath, "login-token-that-must-not-leak\n", { mode: 0o600 });
  await writeFile(gatePath, "placeholder-token\n", { mode: 0o600 });
  await writeFile(preloadPath, `globalThis.fetch = async (input, init = {}) => { const path = new URL(input).pathname; const auth = init.headers?.authorization; const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } }); if (path === "/v2/organizations/_search") { if (auth === "Bearer placeholder-token") return json({ code: "unauthorized" }, 401); return json({ details: { totalResult: "1" }, result: [{ id: "123456789", state: "ORGANIZATION_STATE_ACTIVE" }] }); } if (path === "/v2/users/blazn-provider-gate" && init.method === "GET") return json({}, 404); if (path === "/v2/users/new") return json({ id: "blazn-provider-gate" }); if (path === "/members") return json({ details: {} }); if (path === "/v2/users/blazn-provider-gate/pats") return json({ token: "authoritative-gate-token" }); return json({ code: "unexpected" }, 500); };\n`);
  try {
    const child = spawn(process.execPath, ["--import", preloadPath, script], {
      env: { ...process.env, ZITADEL_API_URL: "http://zitadel-api:8080", BLAZN_ZITADEL_DOMAIN: "identity.example.test", ZITADEL_LOGIN_CLIENT_TOKEN_FILE: loginPath, ZITADEL_PROVIDER_GATE_TOKEN_FILE: gatePath, ZITADEL_PROVIDER_GATE_PAT_EXPIRATION: "2099-01-01T00:00:00Z" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "", stderr = "";
    child.stdout.setEncoding("utf8").on("data", (value) => { stdout += value; });
    child.stderr.setEncoding("utf8").on("data", (value) => { stderr += value; });
    const code = await new Promise((resolve, reject) => { child.once("error", reject); child.once("close", resolve); });
    assert.equal(code, 0, stderr);
    assert.equal(stdout, "");
    assert.equal(await readFile(gatePath, "utf8"), "authoritative-gate-token\n");
    assert.doesNotMatch(stderr, /login-token-that-must-not-leak/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
