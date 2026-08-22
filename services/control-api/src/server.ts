import { randomUUID } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { loadConfig } from "./config.js";
import { createDatabase, type Database } from "./db.js";
import { HttpError, jsonBody, requiredString, sendJson } from "./http.js";
import { passwordRecord, randomToken, tokenHash, userCode, verifyPassword } from "./security.js";

const config = loadConfig();
const database = createDatabase(config.databaseUrl);

interface AuthenticatedSession {
  sessionId: string;
  userId: string;
  deviceId: string;
  email: string;
  displayName: string;
}

async function authenticate(request: IncomingMessage): Promise<AuthenticatedSession> {
  const header = request.headers.authorization;
  if (!header?.startsWith("Bearer ")) throw new HttpError(401, "unauthorized", "a bearer session is required");
  const result = await database.query<{
    session_id: string; user_id: string; device_id: string; email: string; display_name: string;
  }>(`SELECT s.id AS session_id, s.user_id, s.device_id, u.email, u.display_name
      FROM sessions s JOIN users u ON u.id = s.user_id JOIN devices d ON d.id = s.device_id
      WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now() AND d.revoked_at IS NULL`, [tokenHash(header.slice(7))]);
  const row = result.rows[0];
  if (!row) throw new HttpError(401, "session_invalid", "the session is expired or revoked");
  await database.query("UPDATE devices SET last_seen_at = now() WHERE id = $1", [row.device_id]);
  return { sessionId: row.session_id, userId: row.user_id, deviceId: row.device_id, email: row.email, displayName: row.display_name };
}

async function health(response: ServerResponse): Promise<void> {
  await database.query("SELECT 1");
  let objectStorage = "not_configured";
  if (config.s3Endpoint) {
    const result = await fetch(`${config.s3Endpoint.replace(/\/$/, "")}/minio/health/live`, { signal: AbortSignal.timeout(2_000) });
    if (!result.ok) throw new HttpError(503, "object_storage_unavailable", "object storage is unavailable");
    objectStorage = "ok";
  }
  sendJson(response, 200, { status: "ok", database: "ok", objectStorage });
}

async function startDeviceAuthorization(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const body = await jsonBody(request);
  const deviceName = requiredString(body, "device_name", 128);
  const platform = requiredString(body, "platform", 64);
  const deviceCode = randomToken();
  const code = userCode();
  const expiresAt = new Date(Date.now() + config.deviceCodeTtlSeconds * 1000);
  await database.query(`INSERT INTO device_authorizations(id, device_code_hash, user_code, device_name, platform, expires_at)
      VALUES ($1, $2, $3, $4, $5, $6)`, [randomUUID(), tokenHash(deviceCode), code, deviceName, platform, expiresAt]);
  sendJson(response, 201, {
    device_code: deviceCode,
    user_code: code,
    verification_uri: `${config.publicUrl}/activate`,
    verification_uri_complete: `${config.publicUrl}/activate?user_code=${encodeURIComponent(code)}`,
    expires_in: config.deviceCodeTtlSeconds,
    interval: 2,
  });
}

function activationPage(response: ServerResponse, code: string): void {
  const escaped = code.replace(/[^A-Z0-9-]/g, "");
  const html = `<!doctype html><html><head><meta charset="utf-8"><title>Authorize Blazn</title></head><body><main><h1>Authorize Blazn CLI</h1><form method="post" action="/v1/auth/device/approve"><label>Code <input name="user_code" value="${escaped}" required></label><label>Email <input name="email" type="email" required></label><label>Name <input name="display_name" required></label><label>Password <input name="password" type="password" required></label><label>Bootstrap secret (new users only) <input name="bootstrap_secret" type="password"></label><button>Authorize</button></form></main></body></html>`;
  response.writeHead(200, { "content-type": "text/html; charset=utf-8", "content-length": Buffer.byteLength(html), "cache-control": "no-store", "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'" });
  response.end(html);
}

async function formBody(request: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  for await (const chunk of request) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  const params = new URLSearchParams(Buffer.concat(chunks).toString("utf8"));
  return Object.fromEntries(params.entries());
}

async function approveDevice(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const contentType = request.headers["content-type"] ?? "";
  const body = contentType.startsWith("application/x-www-form-urlencoded") ? await formBody(request) : await jsonBody(request);
  const code = requiredString(body, "user_code", 16).toUpperCase();
  const email = requiredString(body, "email", 254).toLowerCase();
  const displayName = requiredString(body, "display_name", 128);
  const password = requiredString(body, "password", 1024);
  const authorization = await database.query<{ id: string }>("SELECT id FROM device_authorizations WHERE user_code = $1 AND expires_at > now() AND approved_user_id IS NULL AND consumed_at IS NULL FOR UPDATE", [code]);
  const pending = authorization.rows[0];
  if (!pending) throw new HttpError(404, "authorization_not_found", "authorization code is invalid, expired, or already used");
  let user = await database.query<{ id: string; password_salt: string; password_hash: string }>("SELECT id, password_salt, password_hash FROM users WHERE email = $1", [email]);
  let userId: string;
  if (user.rows[0]) {
    if (!(await verifyPassword(password, user.rows[0].password_salt, user.rows[0].password_hash))) throw new HttpError(403, "identity_rejected", "identity verification failed");
    userId = user.rows[0].id;
  } else {
    const bootstrap = typeof body.bootstrap_secret === "string" ? body.bootstrap_secret : request.headers["x-blazn-bootstrap-secret"];
    if (bootstrap !== config.bootstrapSecret) throw new HttpError(403, "bootstrap_required", "new identities require the bootstrap secret");
    const record = await passwordRecord(password);
    userId = randomUUID();
    await database.query("INSERT INTO users(id, email, display_name, password_salt, password_hash) VALUES ($1, $2, $3, $4, $5)", [userId, email, displayName, record.salt, record.hash]);
  }
  await database.query("UPDATE device_authorizations SET approved_user_id = $1 WHERE id = $2", [userId, pending.id]);
  if (contentType.startsWith("application/x-www-form-urlencoded")) {
    const html = "<!doctype html><html><body><h1>Device authorized</h1><p>You may return to the CLI.</p></body></html>";
    response.writeHead(200, { "content-type": "text/html; charset=utf-8", "content-length": Buffer.byteLength(html), "cache-control": "no-store" });
    response.end(html);
  } else {
    sendJson(response, 200, { status: "approved" });
  }
}

async function exchangeDeviceCode(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const body = await jsonBody(request);
  const code = requiredString(body, "device_code", 512);
  const client = await database.connect();
  try {
    await client.query("BEGIN");
    const result = await client.query<{ id: string; approved_user_id: string | null; device_name: string; platform: string }>(`SELECT id, approved_user_id, device_name, platform FROM device_authorizations
        WHERE device_code_hash = $1 AND expires_at > now() AND consumed_at IS NULL FOR UPDATE`, [tokenHash(code)]);
    const authorization = result.rows[0];
    if (!authorization) throw new HttpError(400, "expired_token", "device authorization is invalid or expired");
    if (!authorization.approved_user_id) throw new HttpError(428, "authorization_pending", "waiting for browser authorization");
    const deviceId = randomUUID();
    const sessionId = randomUUID();
    const token = randomToken();
    await client.query("INSERT INTO devices(id, user_id, name, platform) VALUES ($1, $2, $3, $4)", [deviceId, authorization.approved_user_id, authorization.device_name, authorization.platform]);
    await client.query("INSERT INTO sessions(id, user_id, device_id, token_hash, expires_at) VALUES ($1, $2, $3, $4, now() + ($5 * interval '1 second'))", [sessionId, authorization.approved_user_id, deviceId, tokenHash(token), config.sessionTtlSeconds]);
    await client.query("UPDATE device_authorizations SET consumed_at = now() WHERE id = $1", [authorization.id]);
    await client.query("COMMIT");
    sendJson(response, 200, { access_token: token, token_type: "Bearer", expires_in: config.sessionTtlSeconds, session_id: sessionId, device_id: deviceId });
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

async function route(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const url = new URL(request.url ?? "/", config.publicUrl);
  if (request.method === "GET" && url.pathname === "/healthz") return health(response);
  if (request.method === "GET" && url.pathname === "/activate") return activationPage(response, url.searchParams.get("user_code") ?? "");
  if (request.method === "POST" && url.pathname === "/v1/auth/device/authorizations") return startDeviceAuthorization(request, response);
  if (request.method === "POST" && url.pathname === "/v1/auth/device/approve") return approveDevice(request, response);
  if (request.method === "POST" && url.pathname === "/v1/auth/device/tokens") return exchangeDeviceCode(request, response);
  if (request.method === "GET" && url.pathname === "/v1/auth/session") {
    const session = await authenticate(request);
    return sendJson(response, 200, { user: { id: session.userId, email: session.email, display_name: session.displayName }, device_id: session.deviceId, session_id: session.sessionId });
  }
  if (request.method === "DELETE" && url.pathname === "/v1/auth/session") {
    const session = await authenticate(request);
    await database.query("UPDATE sessions SET revoked_at = now() WHERE id = $1", [session.sessionId]);
    return sendJson(response, 200, { status: "revoked" });
  }
  if (request.method === "GET" && url.pathname === "/v1/auth/devices") {
    const session = await authenticate(request);
    const devices = await database.query("SELECT id, name, platform, created_at, last_seen_at, revoked_at FROM devices WHERE user_id = $1 ORDER BY created_at DESC", [session.userId]);
    return sendJson(response, 200, { devices: devices.rows });
  }
  const deviceMatch = url.pathname.match(/^\/v1\/auth\/devices\/([0-9a-f-]+)$/);
  if (request.method === "DELETE" && deviceMatch?.[1]) {
    const session = await authenticate(request);
    const result = await database.query("UPDATE devices SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL RETURNING id", [deviceMatch[1], session.userId]);
    if (!result.rowCount) throw new HttpError(404, "device_not_found", "device was not found");
    await database.query("UPDATE sessions SET revoked_at = now() WHERE device_id = $1", [deviceMatch[1]]);
    return sendJson(response, 200, { status: "revoked", device_id: deviceMatch[1] });
  }
  if (request.method === "GET" && url.pathname === "/v1/events") {
    const session = await authenticate(request);
    response.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-store", connection: "keep-alive" });
    response.write(`event: ready\ndata: ${JSON.stringify({ session_id: session.sessionId })}\n\n`);
    const timer = setInterval(async () => {
      try {
        const active = await database.query("SELECT 1 FROM sessions s JOIN devices d ON d.id=s.device_id WHERE s.id=$1 AND s.revoked_at IS NULL AND d.revoked_at IS NULL AND s.expires_at > now()", [session.sessionId]);
        if (!active.rowCount) {
          response.write("event: revoked\ndata: {}\n\n");
          response.end();
          clearInterval(timer);
        } else {
          response.write("event: heartbeat\ndata: {}\n\n");
        }
      } catch {
        response.end();
        clearInterval(timer);
      }
    }, 2_000);
    response.on("close", () => clearInterval(timer));
    return;
  }
  throw new HttpError(404, "not_found", "route not found");
}

const server = createServer((request, response) => {
  const started = Date.now();
  route(request, response).catch((error: unknown) => {
    const httpError = error instanceof HttpError ? error : new HttpError(500, "internal_error", "request failed");
    if (!response.headersSent) sendJson(response, httpError.status, { error: { code: httpError.code, message: httpError.message } });
    else response.end();
    if (!(error instanceof HttpError) && process.env.NODE_ENV !== "test") console.error("control-api request failed", { method: request.method, path: request.url?.split("?")[0], error: error instanceof Error ? error.name : "unknown" });
  }).finally(() => {
    if (process.env.NODE_ENV !== "test") console.info("control-api request", { method: request.method, path: request.url?.split("?")[0], status: response.statusCode, duration_ms: Date.now() - started });
  });
});

server.listen(config.port, "0.0.0.0", () => console.info("control-api listening", { port: config.port }));

async function shutdown(): Promise<void> {
  server.close();
  await database.end();
}
process.on("SIGTERM", () => void shutdown());
process.on("SIGINT", () => void shutdown());
