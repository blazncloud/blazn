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
const organizations = async (token) => request(token, "/v2/organizations/_search", { method: "POST", body: { query: { offset: "0", limit: 2, asc: true } } });

const existingToken = (await readFile(target, "utf8")).trim();
if (existingToken) {
  const existingCheck = await organizations(existingToken);
  if (existingCheck.ok && Number(existingCheck.payload?.details?.totalResult) === 1 && existingCheck.payload?.result?.length === 1) process.exit(0);
}

const visible = await organizations(loginToken);
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

let membership = await request(loginToken, "/members", { method: "POST", body: { userId, roles: ["IAM_ORG_MANAGER"] }, accepted: [200, 409] });
if (membership.response.status === 409) membership = await request(loginToken, `/members/${userId}`, { method: "PUT", body: { userId, roles: ["IAM_ORG_MANAGER"] } });
if (!membership.ok) fail("instance-wide provider gate role could not be assigned");
const pat = await request(loginToken, `/v2/users/${userId}/pats`, { method: "POST", body: { userId, expirationDate: expiration } });
const gateToken = pat.payload?.token;
if (!pat.ok || typeof gateToken !== "string" || gateToken.length < 16 || gateToken.length > 65_536) fail("provider gate token could not be created");
const authoritative = await organizations(gateToken);
if (!authoritative.ok || Number(authoritative.payload?.details?.totalResult) !== 1 || authoritative.payload?.result?.length !== 1) fail("provider gate token is not instance-authoritative");

const handle = await open(target, "r+", 0o600);
try {
  await handle.truncate(0);
  await handle.writeFile(`${gateToken}\n`, "utf8");
  await handle.sync();
} finally {
  await handle.close();
}
