import assert from "node:assert/strict";
import { createServer } from "node:http";
import { test } from "node:test";
import { browserCors, browserOrigins } from "./browser-cors.js";

test("browser origins reject wildcard, insecure, path, credentials and null", () => {
  for (const origin of ["*", "null", "http://blazn.frontro.com", "https://blazn.frontro.com/", "https://user@blazn.frontro.com", "https://blazn.frontro.com/path"]) {
    assert.throws(() => browserOrigins(origin));
  }
});

test("exact browser CORS preserves bearer authorization errors and OIDC navigation", async () => {
  const allowed = browserOrigins("https://blazn.frontro.com,https://api.blazn.frontro.com");
  const server = createServer((request, response) => {
    if (browserCors(request, response, allowed)) return;
    response.writeHead(401);
    response.end("authentication required");
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const address = server.address();
    assert.ok(address && typeof address !== "string");
    const base = `http://127.0.0.1:${address.port}`;
    const origin = "https://blazn.frontro.com";
    const preflight = await fetch(base + "/v1/workspaces", { method: "OPTIONS", headers: { Origin: origin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "Authorization, Content-Type, Idempotency-Key" } });
    assert.equal(preflight.status, 204);
    assert.equal(preflight.headers.get("access-control-allow-origin"), origin);
    assert.equal(preflight.headers.get("access-control-allow-credentials"), null);
    for (const headers of [{ Origin: origin }, {}, { Origin: "https://api.blazn.frontro.com" }]) {
      const result = await fetch(base + "/v1/auth/me", { headers });
      assert.equal(result.status, 401);
    }
    for (const bad of ["null", "https://evil.example", "https://blazn.frontro.com.evil.example"]) {
      const result = await fetch(base + "/v1/workspaces", { headers: { Origin: bad } });
      assert.equal(result.status, 403);
      assert.equal(result.headers.get("access-control-allow-origin"), null);
    }
    for (const extra of [{ "Access-Control-Request-Method": "TRACE" }, { "Access-Control-Request-Headers": "X-Blazn-Proxy-Authorization" }]) {
      const result = await fetch(base + "/v1/workspaces", { method: "OPTIONS", headers: { Origin: origin, "Access-Control-Request-Method": "POST", ...extra } });
      assert.equal(result.status, 403);
    }
    const callback = await fetch(base + "/v1/auth/oidc/callback", { headers: { Origin: "https://issuer.example" } });
    assert.equal(callback.status, 401);
    assert.equal(callback.headers.get("access-control-allow-origin"), null);
    // The activation page is served with no-referrer, so its forms arrive with "Origin: null".
    for (const form of ["/v1/auth/device/email-code", "/v1/auth/device/email-verify"]) {
      const posted = await fetch(base + form, { method: "POST", headers: { Origin: "null", "Content-Type": "application/x-www-form-urlencoded" }, body: "user_code=AAAA-BBBB" });
      assert.equal(posted.status, 401, form);
      assert.equal(posted.headers.get("access-control-allow-origin"), null);
    }
    const nullOrigin = await fetch(base + "/v1/workspaces", { method: "POST", headers: { Origin: "null" } });
    assert.equal(nullOrigin.status, 403);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  }
});
