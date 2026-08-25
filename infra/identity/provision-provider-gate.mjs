import { lstat, open, readFile } from "node:fs/promises";

const fail = (message) => { throw new Error(`provider gate provisioning failed: ${message}`); };
const api = new URL(process.env.ZITADEL_API_URL ?? "");
if (api.protocol !== "http:" || api.hostname !== "zitadel-api" || api.port !== "8080") fail("internal API URL is invalid");
const domain = process.env.BLAZN_ZITADEL_DOMAIN ?? "";
if (!/^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/.test(domain) || domain.includes("..")) fail("public domain is invalid");
const loginToken = (await readFile(process.env.ZITADEL_LOGIN_CLIENT_TOKEN_FILE ?? "", "utf8")).trim();
if (!loginToken) fail("login client token is empty");
const target = process.env.ZITADEL_PROVIDER_GATE_TOKEN_FILE ?? "";
const targetStat = await lstat(target);
if (!targetStat.isFile() || targetStat.isSymbolicLink() || targetStat.nlink !== 1 || (targetStat.mode & 0o777) !== 0o600 || targetStat.size > 65_536) fail("provider gate token target is invalid");
const expiration = process.env.ZITADEL_PROVIDER_GATE_PAT_EXPIRATION ?? "";
if (!/^20[2-9][0-9]-[0-9]{2}-[0-9]{2}T[0-9:.]+Z$/.test(expiration) || Date.parse(expiration) < Date.now() + 86_400_000) fail("provider gate token expiration is invalid");

const baseHeaders = {
  accept: "application/json",
  "x-forwarded-host": domain,
  "x-forwarded-proto": "https",
  "x-zitadel-instance-host": domain,
  "x-zitadel-public-host": domain,
};
const request = async (token, path, { method = "GET", body, accepted = [200] } = {}) => {
  const response = await fetch(new URL(path, api), {
    method,
    redirect: "error",
    signal: AbortSignal.timeout(10_000),
    headers: { ...baseHeaders, authorization: `Bearer ${token}`, ...(body === undefined ? {} : { "content-type": "application/json" }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength > 1_048_576) fail(`${path} response is too large`);
  let payload = {};
  if (bytes.byteLength) {
    try { payload = JSON.parse(new TextDecoder().decode(bytes)); } catch { fail(`${path} returned invalid JSON`); }
  }
  return { response, payload, ok: accepted.includes(response.status) };
};
const searchOrganizations = async (token, queries) => request(token, "/v2/organizations/_search", { method: "POST", body: { query: { offset: "0", limit: 2, asc: true }, queries } });
const activeOrganizations = (token) => searchOrganizations(token, [{ stateQuery: { state: "ORGANIZATION_STATE_ACTIVE" } }]);
const sentinelId = "blazn-provider-gate-sentinel";
const sentinelName = "Blazn Provider Gate Authority Sentinel";
const sentinelOrganization = (token) => searchOrganizations(token, [{ idQuery: { id: sentinelId } }]);

const visible = await activeOrganizations(loginToken);
if (!visible.ok || Number(visible.payload?.details?.totalResult) !== 1 || visible.payload?.result?.length !== 1) fail("login client organization context is invalid");
const organizationId = visible.payload.result[0]?.id;
if (!/^[0-9]+$/.test(organizationId ?? "")) fail("login client organization ID is invalid");

const userId = "blazn-provider-gate";
const current = await request(loginToken, `/v2/users/${userId}`, { accepted: [200, 404] });
if (current.response.status === 404) {
  const created = await request(loginToken, "/v2/users/new", {
    method: "POST",
    body: { organizationId, userId, username: userId, machine: { name: "Blazn Provider Gate", description: "Request-path external identity provider safe-off gate", accessTokenType: "ACCESS_TOKEN_TYPE_BEARER" } },
  });
  if (!created.ok || created.payload?.id !== userId) fail("machine user could not be created");
} else if (!current.ok || current.payload?.user?.userId !== userId || current.payload.user?.details?.resourceOwner !== organizationId || !current.payload.user?.machine) {
  fail("existing provider gate principal does not match the immutable identity");
}

let membership = await request(loginToken, "/admin/v1/members", { method: "POST", body: { userId, roles: ["IAM_ORG_MANAGER"] }, accepted: [200, 409] });
if (membership.response.status === 409) membership = await request(loginToken, `/admin/v1/members/${userId}`, { method: "PUT", body: { userId, roles: ["IAM_ORG_MANAGER"] } });
if (!membership.ok) fail("instance-wide provider gate role could not be assigned");
const pat = await request(loginToken, `/v2/users/${userId}/pats`, { method: "POST", body: { userId, expirationDate: expiration } });
const gateToken = pat.payload?.token;
const gateTokenId = pat.payload?.tokenId;
if (!pat.ok || typeof gateToken !== "string" || gateToken.length < 16 || gateToken.length > 65_536 || !/^[0-9]+$/.test(gateTokenId ?? "")) fail("provider gate token could not be created");

const listTokens = () => request(loginToken, "/v2/users/pats/search", { method: "POST", body: { pagination: { offset: 0, limit: 100, asc: true }, filters: [{ userIdFilter: { id: userId } }] } });
let tokenInventory = await listTokens();
if (!tokenInventory.ok || Number(tokenInventory.payload?.pagination?.totalResult) !== tokenInventory.payload?.result?.length || !Array.isArray(tokenInventory.payload?.result) || tokenInventory.payload.result.length > 100) fail("provider gate token inventory is invalid");
for (const candidate of tokenInventory.payload.result) {
  if (candidate?.userId !== userId || !/^[0-9]+$/.test(candidate?.id ?? "")) fail("provider gate token inventory contains an invalid entry");
  if (candidate.id !== gateTokenId) {
    const removed = await request(loginToken, `/v2/users/${userId}/pats/${candidate.id}`, { method: "DELETE" });
    if (!removed.ok) fail("superseded provider gate token could not be revoked");
  }
}
tokenInventory = await listTokens();
const retainedToken = tokenInventory.payload?.result?.[0];
if (!tokenInventory.ok || Number(tokenInventory.payload?.pagination?.totalResult) !== 1 || tokenInventory.payload?.result?.length !== 1 || retainedToken?.id !== gateTokenId || retainedToken?.userId !== userId || Date.parse(retainedToken?.expirationDate) !== Date.parse(expiration)) fail("provider gate token rotation did not converge");

let sentinel = await sentinelOrganization(gateToken);
if (!sentinel.ok) fail("authority sentinel inventory is unavailable");
if (Number(sentinel.payload?.details?.totalResult) === 0 && sentinel.payload?.result?.length === 0) {
  const created = await request(gateToken, "/v2/organizations", { method: "POST", body: { name: sentinelName, organizationId: sentinelId }, accepted: [201] });
  if (!created.ok || created.payload?.organizationId !== sentinelId) fail("authority sentinel could not be created");
  sentinel = { ok: true, payload: { details: { totalResult: "1" }, result: [{ id: sentinelId, name: sentinelName, state: "ORGANIZATION_STATE_ACTIVE" }] } };
}
if (Number(sentinel.payload?.details?.totalResult) !== 1 || sentinel.payload?.result?.length !== 1 || sentinel.payload.result[0]?.id !== sentinelId || sentinel.payload.result[0]?.name !== sentinelName) fail("authority sentinel identity is invalid");
if (sentinel.payload.result[0]?.state === "ORGANIZATION_STATE_ACTIVE") {
  const deactivated = await request(gateToken, `/v2/organizations/${sentinelId}/deactivate`, { method: "POST", body: { organizationId: sentinelId } });
  if (!deactivated.ok) fail("authority sentinel could not be deactivated");
} else if (sentinel.payload.result[0]?.state !== "ORGANIZATION_STATE_INACTIVE") fail("authority sentinel state is invalid");

const principal = await request(gateToken, "/auth/v1/users/me");
const authoritativeSentinel = await sentinelOrganization(gateToken);
const authoritative = await activeOrganizations(gateToken);
if (!principal.ok || principal.payload?.user?.id !== userId || !authoritativeSentinel.ok || Number(authoritativeSentinel.payload?.details?.totalResult) !== 1 || authoritativeSentinel.payload?.result?.[0]?.state !== "ORGANIZATION_STATE_INACTIVE" || !authoritative.ok || Number(authoritative.payload?.details?.totalResult) !== 1 || authoritative.payload?.result?.length !== 1) fail("provider gate token is not bound to the authoritative principal and sentinel");

const handle = await open(target, "r+", 0o600);
try {
  await handle.truncate(0);
  await handle.writeFile(`${gateToken}\n`, "utf8");
  await handle.sync();
} finally {
  await handle.close();
}
