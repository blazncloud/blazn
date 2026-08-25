import { readFile, stat } from "node:fs/promises";
import { pathToFileURL } from "node:url";

const fail = (message) => {
  throw new Error(`external identity provider safe-off gate failed: ${message}`);
};

export async function assertNoActiveIdentityProviders() {
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

  const headers = {
    authorization: `Bearer ${token}`,
    accept: "application/json",
    "x-forwarded-host": domain,
    "x-forwarded-proto": "https",
    "x-zitadel-instance-host": domain,
    "x-zitadel-public-host": domain,
  };
  const requestJson = async (url, label, options = {}) => {
    const response = await fetch(url, { redirect: "error", signal: AbortSignal.timeout(5_000), headers, ...options });
    if (!response.ok) fail(`${label} returned HTTP ${response.status}`);
    const payloadBytes = new Uint8Array(await response.arrayBuffer());
    if (payloadBytes.byteLength > 1_048_576) fail(`${label} response is too large`);
    return JSON.parse(new TextDecoder().decode(payloadBytes));
  };

  const principal = await requestJson(new URL("/auth/v1/users/me", api), "provider gate principal");
  if (principal?.user?.id !== "blazn-provider-gate") fail("provider gate token subject is invalid");

  const tokenInventory = await requestJson(new URL("/v2/users/pats/search", api), "provider gate token inventory", {
    method: "POST",
    headers: { ...headers, "content-type": "application/json" },
    body: JSON.stringify({ pagination: { offset: 0, limit: 2, asc: true }, filters: [{ userIdFilter: { id: "blazn-provider-gate" } }] }),
  });
  const retainedToken = tokenInventory?.result?.[0];
  if (Number(tokenInventory?.pagination?.totalResult) !== 1 || tokenInventory?.result?.length !== 1 || !/^[0-9]+$/.test(retainedToken?.id ?? "") || retainedToken?.userId !== "blazn-provider-gate" || !Number.isFinite(Date.parse(retainedToken?.expirationDate)) || Date.parse(retainedToken.expirationDate) <= Date.now()) fail("provider gate token inventory is not reconciled");

  const sentinel = await requestJson(new URL("/v2/organizations/_search", api), "authority sentinel", {
    method: "POST",
    headers: { ...headers, "content-type": "application/json" },
    body: JSON.stringify({ query: { offset: "0", limit: 2, asc: true }, queries: [{ idQuery: { id: "blazn-provider-gate-sentinel" } }] }),
  });
  if (Number(sentinel?.details?.totalResult) !== 1 || sentinel?.result?.length !== 1 || sentinel.result[0]?.id !== "blazn-provider-gate-sentinel" || sentinel.result[0]?.name !== "Blazn Provider Gate Authority Sentinel" || sentinel.result[0]?.state !== "ORGANIZATION_STATE_INACTIVE") {
    fail("instance-wide authority sentinel is unavailable");
  }

  const organizations = await requestJson(new URL("/v2/organizations/_search", api), "organization inventory", {
    method: "POST",
    headers: { ...headers, "content-type": "application/json" },
    body: JSON.stringify({ query: { offset: "0", limit: 2, asc: true }, queries: [{ stateQuery: { state: "ORGANIZATION_STATE_ACTIVE" } }] }),
  });
  const totalOrganizations = Number(organizations?.details?.totalResult);
  if (!Number.isSafeInteger(totalOrganizations) || totalOrganizations !== 1 || !Array.isArray(organizations.result) || organizations.result.length !== 1) {
    fail("exactly one organization is required while external providers are disabled");
  }
  const organization = organizations.result[0];
  if (!organization || !/^[0-9]+$/.test(organization.id ?? "") || organization.state !== "ORGANIZATION_STATE_ACTIVE") fail("the sole organization is invalid or inactive");

  const inventoryUrl = new URL("/v2/settings/login/idps", api);
  inventoryUrl.searchParams.set("ctx.orgId", organization.id);
  const payload = await requestJson(inventoryUrl, "provider inventory");
  if (!payload || typeof payload !== "object" || Array.isArray(payload) || !payload.details || typeof payload.details !== "object") fail("inventory response is invalid");
  const providers = payload.identityProviders ?? [];
  if (!Array.isArray(providers)) fail("inventory provider list is invalid");
  if (providers.length !== 0) fail(`${providers.length} active provider(s) are not allowed with the pinned Login image`);

  return { schemaVersion: "blazn.identity.active-idps/v1", authorityPrincipal: "blazn-provider-gate", authoritySentinel: "blazn-provider-gate-sentinel", authorityPatId: retainedToken.id, authorityPatExpiration: retainedToken.expirationDate, organizationCount: 1, activeProviderCount: 0, observedAt: new Date().toISOString() };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.stdout.write(`${JSON.stringify(await assertNoActiveIdentityProviders())}\n`);
}
