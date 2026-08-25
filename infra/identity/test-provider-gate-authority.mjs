import { readFile } from "node:fs/promises";

const origin = new URL(process.env.BLAZN_IDENTITY_LOCAL_ORIGIN ?? "");
if (origin.protocol !== "http:" || origin.hostname !== "127.0.0.1" || !origin.port) throw new Error("local identity origin is invalid");
const domain = process.env.ZITADEL_DOMAIN ?? "";
const token = (await readFile(process.env.ZITADEL_PROVIDER_GATE_TOKEN_FILE ?? "", "utf8")).trim();
if (!token) throw new Error("provider gate token is empty");
const headers = { authorization: `Bearer ${token}`, accept: "application/json", "content-type": "application/json", host: domain, "x-forwarded-proto": "https" };
const name = `Blazn Gate Qualification ${Date.now()}`;
let organizationId;
try {
  const created = await fetch(new URL("/v2/organizations", origin), { method: "POST", headers, body: JSON.stringify({ name }), redirect: "error", signal: AbortSignal.timeout(10_000) });
  const payload = await created.json();
  if (created.status !== 201 || !/^[0-9]+$/.test(payload.organizationId ?? "")) throw new Error(`second organization creation failed with HTTP ${created.status}`);
  organizationId = payload.organizationId;
  let blocked;
  for (let attempt = 0; attempt < 20; attempt += 1) {
    blocked = await fetch(new URL("/ui/v2/login/", origin), { headers: { host: domain }, redirect: "manual", signal: AbortSignal.timeout(10_000) });
    if (blocked.status === 503) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (blocked?.status !== 503) throw new Error(`public Login did not fail closed for a hidden second organization (HTTP ${blocked?.status})`);
} finally {
  if (organizationId) {
    const removed = await fetch(new URL(`/v2/organizations/${organizationId}`, origin), { method: "DELETE", headers, redirect: "error", signal: AbortSignal.timeout(10_000) });
    if (!removed.ok) throw new Error(`qualification organization cleanup failed with HTTP ${removed.status}`);
  }
}
