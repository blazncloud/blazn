import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";
import { serveActivationPage, type ActivationHttpDependencies } from "./activation-http.js";

const pending = { id: "11111111-1111-4111-8111-111111111111", deviceName: "Developer laptop", platform: "darwin/arm64", publicKey: "public-key" };

function fixture(overrides: Partial<ActivationHttpDependencies> = {}) {
  const seen: string[] = [];
  const dependencies: ActivationHttpDependencies = {
    lookup: async (code) => { seen.push(code); return code === "ABCD-EFGH" ? pending : undefined; },
    oidcEnabled: true,
    publicKeyDigest: () => `sha256:${"a".repeat(64)}`,
    activationConfirmation: ({ userCode, mode }) => `sealed-${userCode}-${mode}`,
    ...overrides,
  };
  const server = createServer((request, response) => {
    void serveActivationPage(response, new URL(request.url ?? "/", "http://127.0.0.1"), dependencies).catch((error: unknown) => {
      response.writeHead(500, { "content-type": "text/plain" });
      response.end(error instanceof Error ? error.message : "failed");
    });
  });
  return { server, seen };
}

async function listen(server: ReturnType<typeof createServer>): Promise<string> {
  await new Promise<void>((resolve, reject) => server.listen(0, "127.0.0.1", resolve).once("error", reject));
  const address = server.address();
  assert.ok(address && typeof address === "object");
  return `http://127.0.0.1:${address.port}`;
}

async function close(server: ReturnType<typeof createServer>): Promise<void> {
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}

test("bare activation GET renders a secure code-entry landing page without querying", async () => {
  const { server, seen } = fixture();
  const origin = await listen(server);
  try {
    const response = await fetch(`${origin}/activate`);
    const html = await response.text();
    assert.equal(response.status, 200);
    assert.match(response.headers.get("content-type") ?? "", /^text\/html/);
    assert.equal(response.headers.get("cache-control"), "no-store");
    assert.equal(response.headers.get("x-content-type-options"), "nosniff");
    assert.match(response.headers.get("content-security-policy") ?? "", /default-src 'none'/);
    assert.match(response.headers.get("content-security-policy") ?? "", /form-action 'self'/);
    assert.match(html, /method="get" action="\/activate"/);
    assert.match(html, /name="user_code"/);
    assert.match(html, /blazn auth login/);
    assert.deepEqual(seen, []);
  } finally { await close(server); }
});

test("complete activation URL normalizes the code and preserves signin and signup", async () => {
  const { server, seen } = fixture();
  const origin = await listen(server);
  try {
    const signin = await fetch(`${origin}/activate?user_code=abcd%20efgh`);
    const signinHtml = await signin.text();
    assert.equal(signin.status, 200);
    assert.match(signinHtml, /Welcome back/);
    assert.match(signinHtml, /ABCD-EFGH/);
    assert.match(signinHtml, /sealed-ABCD-EFGH-signin/);
    const signup = await fetch(`${origin}/activate?user_code=ABCD-EFGH&mode=signup`);
    const signupHtml = await signup.text();
    assert.equal(signup.status, 200);
    assert.match(signupHtml, /Create your Blazn account/);
    assert.match(signupHtml, /sealed-ABCD-EFGH-signup/);
    assert.deepEqual(seen, ["ABCD-EFGH", "ABCD-EFGH"]);
  } finally { await close(server); }
});

test("invalid and expired activation codes render HTML without unsafe reflection", async () => {
  const { server, seen } = fixture();
  const origin = await listen(server);
  try {
    const attack = `<img src=x onerror=alert(1)>`;
    const invalid = await fetch(`${origin}/activate?user_code=${encodeURIComponent(attack)}`);
    const invalidHtml = await invalid.text();
    assert.equal(invalid.status, 400);
    assert.match(invalid.headers.get("content-type") ?? "", /^text\/html/);
    assert.match(invalidHtml, /eight-letter code shown by your CLI/);
    assert.doesNotMatch(invalidHtml, /<img|onerror|authorization_not_found/);
    assert.deepEqual(seen, []);

    const expired = await fetch(`${origin}/activate?user_code=WXYZ-2345`);
    const expiredHtml = await expired.text();
    assert.equal(expired.status, 404);
    assert.match(expired.headers.get("content-type") ?? "", /^text\/html/);
    assert.match(expiredHtml, /invalid, expired, or has already been used/);
    assert.doesNotMatch(expiredHtml, /authorization_not_found|WXYZ-2345/);
    assert.deepEqual(seen, ["WXYZ-2345"]);
  } finally { await close(server); }
});
