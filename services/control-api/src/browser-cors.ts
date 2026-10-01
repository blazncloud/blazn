import type { IncomingMessage, ServerResponse } from "node:http";

export function browserOrigins(raw: string | undefined): Set<string> {
  return new Set((raw ? raw.split(",") : []).map((value) => {
    const origin = value.trim();
    const url = new URL(origin);
    if (url.protocol !== "https:" || url.origin !== origin) throw new Error("BROWSER_ORIGINS must contain exact HTTPS origins");
    return origin;
  }));
}

// The activation page's own forms post here as plain navigations. The page is
// served with Referrer-Policy: no-referrer, so a browser sends "Origin: null"
// with them. They carry no bearer credential and are guarded by the device
// code and the emailed code, so they do not take part in the origin check.
const activationForms = new Set(["/v1/auth/device/email-code", "/v1/auth/device/email-verify"]);

// Only JSON/bearer API routes participate. OIDC navigation and its sealed
// activation capability keep their existing cookie and CSRF contract.
export function browserCors(request: IncomingMessage, response: ServerResponse, allowed: Set<string>): boolean {
  if (allowed.size === 0) return false;
  const path = request.url?.split("?")[0] ?? "";
  if (!path.startsWith("/v1/") || path.startsWith("/v1/auth/oidc/") || activationForms.has(path)) return false;
  response.setHeader("Vary", "Origin");
  const origin = request.headers.origin;
  if (!origin) return false;
  if (!allowed.has(origin)) {
    response.writeHead(403, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ code: "forbidden", message: "browser origin is not allowed" }));
    return true;
  }
  response.setHeader("Access-Control-Allow-Origin", origin);
  response.setHeader("Access-Control-Expose-Headers", "X-Request-Id, Retry-After");
  if (request.method !== "OPTIONS") return false;
  response.setHeader("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers");
  const methods = ["GET", "POST", "PUT", "PATCH", "DELETE"];
  const headers = ["authorization", "content-type", "idempotency-key"];
  const method = request.headers["access-control-request-method"];
  const requested = request.headers["access-control-request-headers"];
  if (typeof method !== "string" || !methods.includes(method) || (requested !== undefined && (typeof requested !== "string" || requested.split(",").some((header) => !headers.includes(header.trim().toLowerCase()))))) {
    response.writeHead(403);
    response.end();
    return true;
  }
  response.setHeader("Access-Control-Allow-Methods", methods.join(", "));
  response.setHeader("Access-Control-Allow-Headers", headers.join(", "));
  response.setHeader("Access-Control-Max-Age", "300");
  response.writeHead(204);
  response.end();
  return true;
}
