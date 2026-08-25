import { readFile, stat } from "node:fs/promises";

const fail = (message) => {
  throw new Error(`external identity provider safe-off gate failed: ${message}`);
};

const api = new URL(process.env.ZITADEL_API_URL ?? "");
if (api.protocol !== "http:" || api.hostname !== "zitadel-api" || api.port !== "8080" || !["", "/"].includes(api.pathname) || api.username || api.password || api.search || api.hash) {
  fail("internal API URL is invalid");
}
const domain = process.env.BLAZN_ZITADEL_DOMAIN ?? "";
if (!/^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/.test(domain) || domain.includes("..")) fail("public domain is invalid");

const tokenPath = process.env.ZITADEL_SERVICE_USER_TOKEN_FILE ?? "";
const tokenStat = await stat(tokenPath);
if (!tokenStat.isFile() || tokenStat.size < 1 || tokenStat.size > 65_536) fail("service token file is invalid");
const token = (await readFile(tokenPath, "utf8")).trim();
if (!token) fail("service token is empty");

const response = await fetch(new URL("/v2/settings/login/idps", api), {
  method: "GET",
  redirect: "error",
  signal: AbortSignal.timeout(5_000),
  headers: {
    authorization: `Bearer ${token}`,
    accept: "application/json",
    "x-forwarded-host": domain,
    "x-forwarded-proto": "https",
    "x-zitadel-instance-host": domain,
    "x-zitadel-public-host": domain,
  },
});
if (!response.ok) fail(`inventory returned HTTP ${response.status}`);
const payloadBytes = new Uint8Array(await response.arrayBuffer());
if (payloadBytes.byteLength > 1_048_576) fail("inventory response is too large");
const payload = JSON.parse(new TextDecoder().decode(payloadBytes));
if (!payload || typeof payload !== "object" || Array.isArray(payload) || !payload.details || typeof payload.details !== "object") fail("inventory response is invalid");
const providers = payload.identityProviders ?? [];
if (!Array.isArray(providers)) fail("inventory provider list is invalid");
if (providers.length !== 0) fail(`${providers.length} active provider(s) are not allowed with the pinned Login image`);

process.stdout.write(`${JSON.stringify({
  schemaVersion: "blazn.identity.active-idps/v1",
  activeProviderCount: 0,
  observedAt: new Date().toISOString(),
})}\n`);
