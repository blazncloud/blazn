import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./provision-provider-gate.mjs", import.meta.url));

for (const projection of ["immediate", "delayed", "stuck"]) test(`rotates only after exact PAT inventory converges: ${projection}`, async () => {
  const directory = await mkdtemp(join(tmpdir(), "blazn-provider-provision."));
  const loginPath = join(directory, "login.pat");
  const gatePath = join(directory, "gate.pat");
  const preloadPath = join(directory, "fetch.mjs");
  const auditPath = join(directory, "calls.jsonl");
  await writeFile(loginPath, "login-token-that-must-not-leak\n", { mode: 0o600 });
  await writeFile(gatePath, "placeholder-token\n", { mode: 0o600 });
  await writeFile(preloadPath, `import {appendFileSync} from "node:fs"; let searches = 0; let sentinelState; let tokens = [{ id: "111111111", userId: "blazn-provider-gate", expirationDate: "2088-01-01T00:00:00Z" }]; globalThis.fetch = async (input, init = {}) => { const path = new URL(input).pathname; appendFileSync(process.env.PROVISION_AUDIT, JSON.stringify({path,method:init.method})+"\\n"); const body = init.body ? JSON.parse(init.body) : {}; const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } }); if (path === "/v2/organizations/_search") { if (body.queries?.[0]?.idQuery) return json(sentinelState ? { details: { totalResult: "1" }, result: [{ id: "blazn-provider-gate-sentinel", name: "Blazn Provider Gate Authority Sentinel", state: sentinelState }] } : { details: { totalResult: "0" }, result: [] }); return json({ details: { totalResult: "1" }, result: [{ id: "123456789", state: "ORGANIZATION_STATE_ACTIVE" }] }); } if (path === "/v2/users/blazn-provider-gate" && init.method === "GET") return json({}, 404); if (path === "/v2/users/new") return json({ id: "blazn-provider-gate" }); if (path === "/admin/v1/members") return json({ details: {} }); if (path === "/v2/users/blazn-provider-gate/pats" && init.method === "POST") { tokens.push({ id: "222222222", userId: "blazn-provider-gate", expirationDate: "2099-01-01T00:00:00Z" }); return json({ tokenId: "222222222", token: "authoritative-gate-token" }); } if (path === "/v2/users/pats/search") { searches++; if (searches > 1 && (process.env.PROJECTION_MODE === "stuck" || (process.env.PROJECTION_MODE === "delayed" && searches < 4))) return json({ pagination: { totalResult: "0" }, result: [] }); return json({ pagination: { totalResult: String(tokens.length) }, result: tokens }); } if (path === "/v2/users/blazn-provider-gate/pats/111111111" && init.method === "DELETE") { tokens = tokens.filter((token) => token.id !== "111111111"); return json({}); } if (path === "/v2/organizations" && init.method === "POST") { sentinelState = "ORGANIZATION_STATE_ACTIVE"; return json({ organizationId: "blazn-provider-gate-sentinel" }, 201); } if (path === "/v2/organizations/blazn-provider-gate-sentinel/deactivate") { sentinelState = "ORGANIZATION_STATE_INACTIVE"; return json({}); } if (path === "/auth/v1/users/me") return json({ user: { id: "blazn-provider-gate" } }); return json({ code: "unexpected" }, 500); };\n`);
  try {
    const child = spawn(process.execPath, ["--import", preloadPath, script], {
      env: { ...process.env, PROJECTION_MODE: projection, PROVISION_AUDIT: auditPath, ZITADEL_API_URL: "http://zitadel-api:8080", BLAZN_ZITADEL_DOMAIN: "identity.example.test", ZITADEL_LOGIN_CLIENT_TOKEN_FILE: loginPath, ZITADEL_PROVIDER_GATE_TOKEN_FILE: gatePath, ZITADEL_PROVIDER_GATE_PAT_EXPIRATION: "2099-01-01T00:00:00Z" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "", stderr = "";
    child.stdout.setEncoding("utf8").on("data", (value) => { stdout += value; });
    child.stderr.setEncoding("utf8").on("data", (value) => { stderr += value; });
    const code = await new Promise((resolve, reject) => { child.once("error", reject); child.once("close", resolve); });
    assert.equal(code, projection === "stuck" ? 1 : 0, stderr);
    if (projection === "stuck") assert.match(stderr, /token rotation did not converge/);
    assert.equal(stdout, "");
    assert.equal(await readFile(gatePath, "utf8"), projection === "stuck" ? "placeholder-token\n" : "authoritative-gate-token\n");
    assert.doesNotMatch(stderr, /login-token-that-must-not-leak|authoritative-gate-token/);
    const calls = (await readFile(auditPath, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
    assert.equal(calls.filter((call) => call.method === "POST" && call.path === "/v2/users/blazn-provider-gate/pats").length, 1);
    if (projection === "stuck") {
      const lastWrite = calls.findLastIndex((call) => call.method !== "GET" && !call.path.endsWith("/search") && !call.path.endsWith("/_search"));
      assert.ok(calls.slice(lastWrite + 1).every((call) => call.path === "/v2/users/pats/search"));
      assert.equal(calls.slice(lastWrite + 1).length, 6);
    }
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
