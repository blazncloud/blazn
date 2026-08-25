import { randomUUID } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { renderAuthResult, sendHtml, type AuthMode } from "./auth-page.js";
import { serveActivationPage } from "./activation-http.js";
import { loadConfig } from "./config.js";
import { createDatabase, type Database } from "./db.js";
import { HttpError, jsonBody, requireExactKeys, requiredSecret, requiredString, sendJson } from "./http.js";
import { enforceLimit, remoteIdentity, TrustedProxyPolicy } from "./limits.js";
import { randomToken, sessionRevokePayload, tokenHash, userCode, verifyDeviceProof, verifyPassword } from "./security.js";
import { sessionAccessError } from "./session-state.js";
import { verifyBucket } from "./s3.js";
import { readInvitationKey } from "./workspace-crypto.js";
import { FileNodePlanSigner, readNodeEnrollmentKey } from "./node-crypto.js";
import { NodeHttpRouter } from "./node-http.js";
import { TemplateNodePlanFactory } from "./node-plan.js";
import { NodeService } from "./node-service.js";
import { PgNodeStore } from "./node-store.js";
import { LoopbackNodeBrokerProxy } from "./node-broker-proxy.js";
import { nodeErrorBody, NodeHttpError } from "./node-types.js";
import { WorkspaceHttpRouter } from "./workspace-http.js";
import { WorkspaceService } from "./workspace-service.js";
import { PgWorkspaceStore } from "./workspace-store.js";
import { ProjectHttpRouter } from "./project-http.js";
import { ProjectService } from "./project-service.js";
import { PgProjectStore } from "./project-store.js";
import { RunHttpRouter } from "./run-http.js";
import { RunService } from "./run-service.js";
import { PgRunStore } from "./run-store.js";
import { DevelopmentHttpRouter } from "./development-http.js";
import { DevelopmentService } from "./development-service.js";
import { PgDevelopmentStore } from "./development-store.js";
import { SandboxHttpRouter } from "./sandbox-http.js";
import { SandboxService } from "./sandbox-service.js";
import { PgSandboxStore } from "./sandbox-store.js";
import { routeSandboxRequest } from "./sandbox-server-routing.js";
import { isControlHttpError, normalizeControlHttpError } from "./server-errors.js";
import { OidcClient, type OidcIdentity } from "./oidc.js";
import { activationOriginMatches, activationPublicKeyDigest, oidcCookieKey, oidcTransactionCookie, oidcTransactionFromRequest, sealActivationConfirmation, stateMatches, unsealActivationConfirmation } from "./oidc-state.js";

const config = loadConfig();
const database = createDatabase(config.databaseUrl);
const activeStreams = new Map<string, Set<ServerResponse>>();
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const trustedProxies = new TrustedProxyPolicy(config.trustedProxyCidrs, config.trustedProxyHops);
const oidcClient = config.zitadel ? new OidcClient({ issuerUrl: config.zitadel.issuerUrl, clientId: config.zitadel.clientId, clientSecret: config.zitadel.clientSecret, callbackUrl: `${config.publicUrl}/v1/auth/oidc/callback`, assurancePolicy: config.zitadel.assurancePolicy }) : undefined;
const oidcKey = config.zitadel ? oidcCookieKey(config.zitadel.cookieKey) : undefined;
const workspaceRouter = new WorkspaceHttpRouter(new WorkspaceService(new PgWorkspaceStore(database), readInvitationKey));
const projectRouter = new ProjectHttpRouter(new ProjectService(new PgProjectStore(database)));
const runRouter = new RunHttpRouter(new RunService(new PgRunStore(database)));
const developmentRouter = new DevelopmentHttpRouter(new DevelopmentService(new PgDevelopmentStore(database)));
const sandboxRouter = new SandboxHttpRouter(new SandboxService(new PgSandboxStore(database)));
const nodeSecretsRoot = process.env.BLAZN_NODE_BROKER_SECRETS_ROOT ?? "/etc/blazn/node-broker/secrets";
const nodePlanSigner = new FileNodePlanSigner(process.env.NODE_PLAN_SIGNING_KEY_ID ?? "control-plane-node-plan/v1", process.env.NODE_PLAN_SIGNING_PRIVATE_KEY_FILE ?? "/etc/blazn/node-plan/signing-private-v1.b64url");
const brokerMode = process.env.BLAZN_NODE_BROKER_LOOPBACK ?? "disabled";
if (brokerMode !== "enabled" && brokerMode !== "disabled") throw new Error("BLAZN_NODE_BROKER_LOOPBACK must be enabled or disabled");
const brokerProxy = brokerMode === "enabled" ? new LoopbackNodeBrokerProxy() : undefined;
const nodeRouter = new NodeHttpRouter(new NodeService(
  new PgNodeStore(database),
  () => readNodeEnrollmentKey(process.env.NODE_ENROLLMENT_HMAC_FILE ?? `${nodeSecretsRoot}/enrollment-hmac-v1`),
  new TemplateNodePlanFactory(process.env.NODE_INSTALL_PLAN_TEMPLATE_FILE ?? "/etc/blazn/node-plan/node-install-plan-template-v1.json", nodePlanSigner),
), brokerProxy, (request) => enforceLimit(database, "node-broker-public", remoteIdentity(request, trustedProxies, config.trustedProxySecret), 60, 60));

function closeStream(sessionId: string): void {
  const streams = activeStreams.get(sessionId);
  for (const stream of streams ?? []) {
    if (!stream.destroyed) {
      stream.write(`event: revoked\nid: ${Date.now()}\ndata: {}\n\n`);
      stream.end();
    }
  }
  activeStreams.delete(sessionId);
}

function registerStream(sessionId: string, response: ServerResponse): void {
  const streams = activeStreams.get(sessionId) ?? new Set<ServerResponse>();
  streams.add(response);
  activeStreams.set(sessionId, streams);
  response.on("close", () => {
    streams.delete(response);
    if (streams.size === 0) activeStreams.delete(sessionId);
  });
}

interface AuthenticatedSession {
  sessionId: string;
  userId: string;
  deviceId: string;
  email: string;
  displayName: string;
  accessToken: string;
}

async function authenticate(request: IncomingMessage): Promise<AuthenticatedSession> {
  const header = request.headers.authorization;
  if (!header?.startsWith("Bearer ")) throw new HttpError("unauthorized", "a bearer session is required");
  const result = await database.query<{
    session_id: string; user_id: string; device_id: string; email: string; display_name: string;
    session_revoked_at: Date | null; device_revoked_at: Date | null; access_expires_at: Date;
  }>(`SELECT s.id AS session_id, s.user_id, s.device_id, u.email, u.display_name,
      s.revoked_at AS session_revoked_at, d.revoked_at AS device_revoked_at, s.access_expires_at
      FROM sessions s JOIN users u ON u.id = s.user_id JOIN devices d ON d.id = s.device_id
      WHERE s.token_hash = $1 AND d.user_id = s.user_id`, [tokenHash(header.slice(7))]);
  const row = result.rows[0];
  if (!row) throw new HttpError("session_revoked", "the session is unavailable or has been superseded");
  const accessError = sessionAccessError({ sessionRevokedAt: row.session_revoked_at, deviceRevokedAt: row.device_revoked_at, accessExpiresAt: row.access_expires_at });
  if (accessError) throw new HttpError(accessError, accessError === "access_expired" ? "the access credential is expired" : "the session or device is revoked");
  await database.query("UPDATE devices SET last_seen_at = now() WHERE id = $1", [row.device_id]);
  return { sessionId: row.session_id, userId: row.user_id, deviceId: row.device_id, email: row.email, displayName: row.display_name, accessToken: header.slice(7) };
}

async function health(response: ServerResponse): Promise<void> {
  await database.query("SELECT 1");
  const result = await fetch(`${config.s3Endpoint.replace(/\/$/, "")}/minio/health/live`, { signal: AbortSignal.timeout(2_000) });
  if (!result.ok) throw new HttpError("object_storage_unavailable", "object storage is unavailable");
  try {
    await verifyBucket(config.s3Endpoint, config.s3Region, config.s3Bucket, config.s3AccessKey, config.s3SecretKey);
  } catch {
    throw new HttpError("object_storage_unavailable", "object storage credentials or required bucket are unavailable");
  }
  if (brokerProxy) {
    try { await brokerProxy.health(AbortSignal.timeout(2_000)); }
    catch { throw new NodeHttpError("node_broker_unavailable", "Node broker is unavailable"); }
  }
	if (oidcClient) {
		try { await oidcClient.health(); }
		catch { throw new HttpError("identity_provider_unavailable", "configured identity provider is unavailable"); }
	}
  sendJson(response, 200, { status: "ok", database: "ok", objectStorage: "ok", identityProvider: oidcClient ? "ok" : "disabled" });
}

async function startDeviceAuthorization(request: IncomingMessage, response: ServerResponse): Promise<void> {
  await enforceLimit(database, "device-start", remoteIdentity(request, trustedProxies, config.trustedProxySecret), 20, 60);
  const body = await jsonBody(request);
  requireExactKeys(body, ["devicePublicKey", "deviceName", "platform"]);
  const deviceName = requiredString(body, "deviceName", 128);
  const platform = requiredString(body, "platform", 64);
  if (!/^(darwin|linux)\/(amd64|arm64)$/.test(platform)) throw new HttpError("invalid_request", "platform is unsupported");
  const publicKey = requiredString(body, "devicePublicKey", 128);
  if (!/^[A-Za-z0-9_-]{43}$/.test(publicKey)) throw new HttpError("invalid_public_key", "devicePublicKey must be a raw Ed25519 public key encoded as base64url");
  const deviceCode = randomToken();
  const code = userCode();
  const challenge = randomToken();
  const expiresAt = new Date(Date.now() + config.deviceCodeTtlSeconds * 1000);
  await database.query("DELETE FROM device_authorizations WHERE expires_at < now() OR consumed_at < now() - interval '1 hour'");
  const pending = await database.query<{ count: string }>("SELECT count(*)::text AS count FROM device_authorizations WHERE expires_at > now() AND consumed_at IS NULL");
  if (Number(pending.rows[0]?.count ?? 0) >= 10_000) throw new HttpError("authorization_capacity", "device authorization capacity is temporarily exhausted");
  await database.query(`INSERT INTO device_authorizations(id, device_code_hash, user_code, device_name, platform, public_key, challenge, expires_at)
      VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, [randomUUID(), tokenHash(deviceCode), code, deviceName, platform, publicKey, challenge, expiresAt]);
  sendJson(response, 201, {
    deviceCode,
    userCode: code,
    verificationUri: `${config.publicUrl}/activate`,
    verificationUriComplete: `${config.publicUrl}/activate?user_code=${encodeURIComponent(code)}`,
    expiresIn: config.deviceCodeTtlSeconds,
    interval: 2,
    challenge,
  });
}

async function startOidc(request: IncomingMessage, response: ServerResponse): Promise<void> {
	if (!oidcClient || !oidcKey || !config.zitadel) throw new HttpError("not_found", "ZITADEL identity is not configured");
  await enforceLimit(database, "oidc-start", remoteIdentity(request, trustedProxies, config.trustedProxySecret), 20, 60);
	const origin = request.headers.origin;
	if (!activationOriginMatches(origin, request.headers.referer, request.headers["sec-fetch-site"], config.publicUrl)) throw new HttpError("activation_confirmation_required", "OIDC activation requires an explicit same-origin confirmation");
	const body = await formBody(request);
	requireExactKeys(body, ["user_code", "mode", "activation_confirmation"]);
	const code = requiredString(body, "user_code", 16).toUpperCase();
	const modeValue = requiredString(body, "mode", 8);
	if (modeValue !== "signin" && modeValue !== "signup") throw new HttpError("invalid_request", "activation mode is invalid");
	const mode = modeValue;
	const sealed = requiredSecret(body, "activation_confirmation", 4096);
	let confirmation;
	try { confirmation = unsealActivationConfirmation(oidcKey, sealed); }
	catch { throw new HttpError("activation_confirmation_required", "activation confirmation is invalid or expired"); }
	if (confirmation.userCode !== code || confirmation.mode !== mode) throw new HttpError("activation_confirmation_required", "activation confirmation does not match this device request");
	const authorization = await database.query<{ id: string; public_key: string }>("SELECT id, public_key FROM device_authorizations WHERE user_code=$1 AND expires_at > now() AND consumed_at IS NULL", [code]);
	const pending = authorization.rows[0];
	if (!pending) throw new HttpError("authorization_not_found", "authorization code is invalid or expired");
	const publicKeyDigest = activationPublicKeyDigest(pending.public_key);
	if (confirmation.authorizationId !== pending.id || confirmation.publicKeyDigest !== publicKeyDigest) throw new HttpError("activation_confirmation_required", "activation confirmation does not match this device key");
  const transaction = oidcClient.createTransaction(code, mode);
  const destination = await oidcClient.authorizationUrl(transaction);
  response.writeHead(303, { location: destination.href, "set-cookie": oidcTransactionCookie(oidcKey, transaction), "cache-control": "no-store", "referrer-policy": "no-referrer" });
  response.end();
}

async function approveOidcIdentity(transaction: { userCode: string; mode: AuthMode }, identity: OidcIdentity): Promise<void> {
  const client = await database.connect();
  try {
    await client.query("BEGIN");
    const authorization = await client.query<{ id: string }>("SELECT id FROM device_authorizations WHERE user_code=$1 AND expires_at > now() AND approved_user_id IS NULL AND consumed_at IS NULL FOR UPDATE", [transaction.userCode]);
    const pending = authorization.rows[0];
    if (!pending) throw new Error("The device authorization expired or was already used.");
    await client.query("SELECT pg_advisory_xact_lock(hashtext($1))", [`oidc:${identity.email}`]);
    const existingIdentity = await client.query<{ user_id: string }>("SELECT user_id FROM user_identities WHERE issuer=$1 AND subject=$2 FOR UPDATE", [identity.issuer, identity.subject]);
    let userId = existingIdentity.rows[0]?.user_id;
    if (userId) {
      await client.query("UPDATE user_identities SET email=$1,last_login_at=now() WHERE issuer=$2 AND subject=$3", [identity.email, identity.issuer, identity.subject]);
    } else {
      if (transaction.mode !== "signup") throw new Error("No Blazn account is linked to this identity. Choose Sign up first.");
      const existingUser = await client.query("SELECT id FROM users WHERE email=$1 FOR UPDATE", [identity.email]);
      if (existingUser.rowCount) throw new Error("An account already exists for this email. Sign in with the existing method; social linking requires separate approval.");
      userId = randomUUID();
      await client.query("INSERT INTO users(id,email,display_name,email_verified_at) VALUES($1,$2,$3,now())", [userId, identity.email, identity.displayName]);
      await client.query("INSERT INTO user_identities(issuer,subject,user_id,email) VALUES($1,$2,$3,$4)", [identity.issuer, identity.subject, userId, identity.email]);
    }
		await client.query("UPDATE device_authorizations SET approved_user_id=$1,approved_identity_provider='zitadel',approved_identity_release=$2,approved_identity_policy_digest=$3,approved_identity_acr=$4,approved_identity_amr=$5 WHERE id=$6", [userId, identity.reviewedRelease, identity.assurancePolicyDigest, identity.acr, identity.amr, pending.id]);
    await client.query("COMMIT");
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally { client.release(); }
}

async function oidcCallback(request: IncomingMessage, response: ServerResponse, url: URL): Promise<void> {
  if (!oidcClient || !oidcKey) throw new HttpError("not_found", "ZITADEL identity is not configured");
  try {
    const transaction = oidcTransactionFromRequest(request, oidcKey);
    if (!stateMatches(transaction.state, url.searchParams.get("state") ?? "")) throw new Error("identity callback state is invalid");
    const identity = await oidcClient.callback(url, transaction);
    await enforceLimit(database, "oidc-callback", `${identity.issuer}:${identity.subject}`, 20, 15 * 60);
    await approveOidcIdentity(transaction, identity);
    sendHtml(response, 200, renderAuthResult("Device authorized", "Your verified Blazn identity is now linked to this CLI.", true), true);
  } catch (error) {
    const message = error instanceof Error && /^(No Blazn account|An account already exists|The device authorization|multi-factor authentication|a verified email)/.test(error.message) ? error.message : "Blazn could not verify this identity transaction. No account or device was changed.";
    sendHtml(response, 400, renderAuthResult("Sign-in could not be completed", message, false), true);
  }
}

async function formBody(request: IncomingMessage, limit = 64 * 1024): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += bytes.length;
    if (size > limit) throw new HttpError("request_too_large", "request body is too large");
    chunks.push(bytes);
  }
  const params = new URLSearchParams(Buffer.concat(chunks).toString("utf8"));
  return Object.fromEntries(params.entries());
}

async function approveDevice(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const contentType = request.headers["content-type"] ?? "";
  const body = contentType.startsWith("application/x-www-form-urlencoded") ? await formBody(request) : await jsonBody(request);
  requireExactKeys(body, ["user_code", "email", "password"]);
  const code = requiredString(body, "user_code", 16).toUpperCase();
  const email = requiredString(body, "email", 254).toLowerCase();
  const password = requiredSecret(body, "password", 1024);
  const callerIdentity = remoteIdentity(request, trustedProxies, config.trustedProxySecret);
  await enforceLimit(database, "device-approve-ip", callerIdentity, 20, 15 * 60);
  const liveAuthorization = await database.query<{ id: string }>("SELECT id FROM device_authorizations WHERE user_code = $1 AND expires_at > now() AND approved_user_id IS NULL AND consumed_at IS NULL", [code]);
  const live = liveAuthorization.rows[0];
  if (!live) throw new HttpError("authorization_not_found", "authorization code is invalid, expired, or already used");
  const accountLimitIdentity = `${live.id}:${callerIdentity}:${email}`;
  await enforceLimit(database, "device-approve-account", accountLimitIdentity, 10, 15 * 60);
  const client = await database.connect();
  try {
    await client.query("BEGIN");
    const authorization = await client.query<{ id: string }>("SELECT id FROM device_authorizations WHERE user_code = $1 AND expires_at > now() AND approved_user_id IS NULL AND consumed_at IS NULL FOR UPDATE", [code]);
    const pending = authorization.rows[0];
    if (!pending) throw new HttpError("authorization_not_found", "authorization code is invalid, expired, or already used");
    const user = await client.query<{ id: string; password_salt: string | null; password_hash: string | null }>("SELECT id, password_salt, password_hash FROM users WHERE email = $1", [email]);
    let userId: string;
    if (user.rows[0]?.password_salt && user.rows[0].password_hash) {
      if (!(await verifyPassword(password, user.rows[0].password_salt, user.rows[0].password_hash))) throw new HttpError("identity_rejected", "identity verification failed");
      userId = user.rows[0].id;
    } else throw new HttpError("identity_rejected", "identity verification failed");
    await client.query("UPDATE device_authorizations SET approved_user_id = $1 WHERE id = $2", [userId, pending.id]);
    await client.query("COMMIT");
  } catch (error) {
    await client.query("ROLLBACK");
    if (contentType.startsWith("application/x-www-form-urlencoded")) {
      sendHtml(response, 403, renderAuthResult("Sign-in was rejected", "The email or password could not be verified. No device was authorized.", false));
      return;
    }
    throw error;
  } finally {
    client.release();
  }
  if (contentType.startsWith("application/x-www-form-urlencoded")) {
    sendHtml(response, 200, renderAuthResult("Device authorized", "Your existing Blazn account approved this CLI.", true));
  } else {
    sendJson(response, 200, { status: "approved" });
  }
}

async function exchangeDeviceCode(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const body = await jsonBody(request);
  requireExactKeys(body, ["deviceCode", "proof"]);
  const code = requiredString(body, "deviceCode", 512);
  const signature = requiredString(body, "proof", 256);
  const client = await database.connect();
  try {
    await client.query("BEGIN");
    const result = await client.query<{ id: string; approved_user_id: string | null; device_name: string; platform: string; public_key: string; challenge: string; last_polled_at: Date | null }>(`SELECT id, approved_user_id, device_name, platform, public_key, challenge, last_polled_at FROM device_authorizations
        WHERE device_code_hash = $1 AND expires_at > now() AND consumed_at IS NULL FOR UPDATE`, [tokenHash(code)]);
    const authorization = result.rows[0];
    if (!authorization) throw new HttpError("expired_token", "device authorization is invalid or expired");
    if (authorization.last_polled_at && Date.now() - authorization.last_polled_at.getTime() < 1_000) throw new HttpError("slow_down", "device authorization is being polled too quickly", 1);
    await client.query("UPDATE device_authorizations SET last_polled_at = now(), poll_count = poll_count + 1 WHERE id = $1", [authorization.id]);
    const canonical = `blazn-device-session-v1\n${code}\n${authorization.challenge}`;
    if (!verifyDeviceProof(authorization.public_key, canonical, signature)) throw new HttpError("device_proof_invalid", "device proof could not be verified");
    if (!authorization.approved_user_id) {
      await client.query("COMMIT");
      sendJson(response, 428, { code: "authorization_pending", message: "waiting for browser authorization", requestId: response.getHeader("x-request-id") });
      return;
    }
    const deviceId = randomUUID();
    const sessionId = randomUUID();
    const token = randomToken();
    const refreshToken = randomToken();
    await client.query("INSERT INTO devices(id, user_id, name, platform, public_key) VALUES ($1, $2, $3, $4, $5)", [deviceId, authorization.approved_user_id, authorization.device_name, authorization.platform, authorization.public_key]);
    await client.query("INSERT INTO sessions(id, user_id, device_id, token_hash, refresh_token_hash, access_expires_at, refresh_expires_at) VALUES ($1, $2, $3, $4, $5, now() + ($6 * interval '1 second'), now() + ($7 * interval '1 second'))", [sessionId, authorization.approved_user_id, deviceId, tokenHash(token), tokenHash(refreshToken), config.sessionTtlSeconds, config.refreshTtlSeconds]);
    await client.query("UPDATE device_authorizations SET consumed_at = now() WHERE id = $1", [authorization.id]);
    await client.query("COMMIT");
    sendJson(response, 200, { accessToken: token, refreshToken, expiresIn: config.sessionTtlSeconds, deviceId });
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

async function refreshSession(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const body = await jsonBody(request);
  requireExactKeys(body, ["refreshToken", "deviceId", "proof"]);
  const deviceId = requiredString(body, "deviceId", 64);
  if (!UUID_PATTERN.test(deviceId)) throw new HttpError("invalid_request", "deviceId must be a UUID");
  const refreshToken = requiredString(body, "refreshToken", 512);
  const signature = requiredString(body, "proof", 256);
  const client = await database.connect();
  try {
    await client.query("BEGIN");
    const result = await client.query<{ session_id: string; public_key: string }>(`SELECT s.id AS session_id, d.public_key FROM sessions s JOIN devices d ON d.id=s.device_id
      WHERE s.device_id=$1 AND s.refresh_token_hash=$2 AND s.revoked_at IS NULL AND s.refresh_expires_at > now() AND d.revoked_at IS NULL AND d.user_id=s.user_id FOR UPDATE`, [deviceId, tokenHash(refreshToken)]);
    const session = result.rows[0];
    if (!session) throw new HttpError("session_revoked", "refresh credential is expired or revoked");
    const canonical = `blazn-refresh-v1\n${deviceId}\n${tokenHash(refreshToken)}`;
    if (!verifyDeviceProof(session.public_key, canonical, signature)) throw new HttpError("device_proof_invalid", "device proof could not be verified");
    const nextAccess = randomToken();
    const nextRefresh = randomToken();
    await client.query("UPDATE sessions SET token_hash=$1, refresh_token_hash=$2, refresh_version=refresh_version+1, access_expires_at=now()+($3 * interval '1 second'), refresh_expires_at=now()+($4 * interval '1 second') WHERE id=$5", [tokenHash(nextAccess), tokenHash(nextRefresh), config.sessionTtlSeconds, config.refreshTtlSeconds, session.session_id]);
    await client.query("COMMIT");
    sendJson(response, 200, { accessToken: nextAccess, refreshToken: nextRefresh, expiresIn: config.sessionTtlSeconds, deviceId });
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

async function revokeSessionWithProof(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const body = await jsonBody(request);
  requireExactKeys(body, ["deviceId", "proof"]);
  const deviceId = requiredString(body, "deviceId", 64);
  if (!UUID_PATTERN.test(deviceId)) throw new HttpError("invalid_request", "deviceId must be a UUID");
  const signature = requiredString(body, "proof", 256);
  const client = await database.connect();
  let revokedSessionIds: string[] = [];
  try {
    await client.query("BEGIN");
    const result = await client.query<{ public_key: string }>("SELECT public_key FROM devices WHERE id=$1 FOR UPDATE", [deviceId]);
    const device = result.rows[0];
    if (!device) throw new HttpError("device_not_found", "the device was not found");
    const canonical = sessionRevokePayload(deviceId);
    if (!verifyDeviceProof(device.public_key, canonical, signature)) throw new HttpError("device_proof_invalid", "device proof could not be verified");
    const revoked = await client.query<{ id: string }>("UPDATE sessions SET revoked_at=COALESCE(revoked_at, now()) WHERE device_id=$1 RETURNING id", [deviceId]);
    revokedSessionIds = revoked.rows.map((session) => session.id);
    await client.query("COMMIT");
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
  for (const sessionId of revokedSessionIds) closeStream(sessionId);
  response.writeHead(204, { "cache-control": "no-store" });
  response.end();
}

async function route(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const url = new URL(request.url ?? "/", config.publicUrl);
  if (request.method === "GET" && url.pathname === "/healthz") return health(response);
  if (request.method === "GET" && url.pathname === "/activate") return serveActivationPage(response, url, {
    lookup: async (code) => {
      const authorization = await database.query<{ id: string; device_name: string; platform: string; public_key: string }>("SELECT id, device_name, platform, public_key FROM device_authorizations WHERE user_code=$1 AND expires_at > now() AND consumed_at IS NULL", [code]);
      const device = authorization.rows[0];
      return device ? { id: device.id, deviceName: device.device_name, platform: device.platform, publicKey: device.public_key } : undefined;
    },
    oidcEnabled: Boolean(oidcClient),
    publicKeyDigest: activationPublicKeyDigest,
    ...(oidcClient && oidcKey ? { activationConfirmation: ({ authorizationId, userCode: code, mode, publicKeyDigest }) => sealActivationConfirmation(oidcKey, { authorizationId, userCode: code, mode, publicKeyDigest, issuedAt: Date.now() }) } : {}),
  });
	if (request.method === "POST" && url.pathname === "/v1/auth/oidc/start") return startOidc(request, response);
  if (request.method === "GET" && url.pathname === "/v1/auth/oidc/callback") return oidcCallback(request, response, url);
  if (request.method === "POST" && url.pathname === "/v1/auth/device/authorizations") return startDeviceAuthorization(request, response);
  if (request.method === "POST" && url.pathname === "/v1/auth/device/approve") return approveDevice(request, response);
  if (request.method === "POST" && url.pathname === "/v1/auth/device/sessions") return exchangeDeviceCode(request, response);
  if (request.method === "POST" && url.pathname === "/v1/auth/sessions/refresh") return refreshSession(request, response);
  if (request.method === "POST" && url.pathname === "/v1/auth/sessions/revoke") return revokeSessionWithProof(request, response);
  if (request.method === "GET" && url.pathname === "/v1/auth/me") {
    const session = await authenticate(request);
    const device = await database.query<{ id: string; name: string; platform: string; created_at: Date; last_seen_at: Date }>("SELECT id, name, platform, created_at, last_seen_at FROM devices WHERE id=$1 AND user_id=$2", [session.deviceId, session.userId]);
    const row = device.rows[0];
    if (!row) throw new HttpError("session_revoked", "the session device is unavailable");
    return sendJson(response, 200, { user: { id: session.userId, email: session.email, displayName: session.displayName, status: "active" }, device: { id: row.id, name: row.name, platform: row.platform, status: "active", createdAt: row.created_at.toISOString(), lastUsedAt: row.last_seen_at.toISOString() } });
  }
  if (request.method === "DELETE" && url.pathname === "/v1/auth/session") {
    const session = await authenticate(request);
    await database.query("UPDATE sessions SET revoked_at = now() WHERE id = $1", [session.sessionId]);
    closeStream(session.sessionId);
    response.writeHead(204, { "cache-control": "no-store" });
    response.end();
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/auth/devices") {
    const session = await authenticate(request);
    const devices = await database.query("SELECT id, name, platform, created_at, last_seen_at, revoked_at FROM devices WHERE user_id = $1 ORDER BY created_at DESC", [session.userId]);
    return sendJson(response, 200, { items: devices.rows.map((device: Record<string, unknown>) => ({ id: device.id, name: device.name, platform: device.platform, status: device.revoked_at ? "revoked" : "active", createdAt: device.created_at, lastUsedAt: device.last_seen_at })) });
  }
  const deviceMatch = url.pathname.match(/^\/v1\/auth\/devices\/([0-9a-f-]+)$/);
  if (request.method === "DELETE" && deviceMatch?.[1]) {
    if (!UUID_PATTERN.test(deviceMatch[1])) throw new HttpError("invalid_request", "deviceId must be a UUID");
    const session = await authenticate(request);
    const result = await database.query("UPDATE devices SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL RETURNING id", [deviceMatch[1], session.userId]);
    if (!result.rowCount) throw new HttpError("device_not_found", "device was not found");
    const revoked = await database.query<{ id: string }>("UPDATE sessions SET revoked_at = now() WHERE device_id = $1 RETURNING id", [deviceMatch[1]]);
    for (const row of revoked.rows) closeStream(row.id);
    response.writeHead(204, { "cache-control": "no-store" });
    response.end();
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/events") {
    const session = await authenticate(request);
    response.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-store", connection: "keep-alive" });
    response.write(`event: ready\nid: ${Date.now()}\ndata: ${JSON.stringify({ sessionId: session.sessionId })}\n\n`);
    registerStream(session.sessionId, response);
    void (async () => {
      while (!response.destroyed && !response.writableEnded) {
        await new Promise((resolve) => setTimeout(resolve, 1_000));
        if (response.destroyed || response.writableEnded) break;
        try {
          const active = await database.query("SELECT 1 FROM sessions s JOIN devices d ON d.id=s.device_id WHERE s.id=$1 AND s.revoked_at IS NULL AND d.revoked_at IS NULL AND d.user_id=s.user_id AND s.access_expires_at > now()", [session.sessionId]);
          if (!active.rowCount) {
            closeStream(session.sessionId);
            break;
          }
        } catch {
          response.end();
          const streams = activeStreams.get(session.sessionId);
          streams?.delete(response);
          if (streams?.size === 0) activeStreams.delete(session.sessionId);
          break;
        }
      }
    })();
    return;
  }
  if (nodeRouter.matches(url.pathname)) {
    return nodeRouter.handle(request, response, url, async () => {
      const session = await authenticate(request);
      return { userId: session.userId, email: session.email, displayName: session.displayName };
    });
  }
  if (await routeSandboxRequest(sandboxRouter, request, response, url, () => authenticate(request))) return;
  if (developmentRouter.matches(url.pathname)) {
    const session = await authenticate(request);
    return developmentRouter.handle(request, response, url, { userId: session.userId, sessionId: session.sessionId, accessToken: session.accessToken, email: session.email, displayName: session.displayName });
  }
  if (runRouter.matches(url.pathname)) {
    const session = await authenticate(request);
    return runRouter.handle(request, response, url, { userId: session.userId, email: session.email, displayName: session.displayName });
  }
  if (projectRouter.matches(url.pathname)) {
    const session = await authenticate(request);
    return projectRouter.handle(request, response, url, { userId: session.userId, email: session.email, displayName: session.displayName });
  }
  if (workspaceRouter.matches(url.pathname)) {
    const session = await authenticate(request);
    const principal = { userId: session.userId, email: session.email, displayName: session.displayName };
    return workspaceRouter.handle(request, response, url, principal, async () => {
      const current = await authenticate(request);
      return { userId: current.userId, email: current.email, displayName: current.displayName };
    });
  }
  const known = new Set(["/healthz", "/activate", "/v1/auth/oidc/start", "/v1/auth/oidc/callback", "/v1/auth/device/authorizations", "/v1/auth/device/approve", "/v1/auth/device/sessions", "/v1/auth/sessions/refresh", "/v1/auth/sessions/revoke", "/v1/auth/me", "/v1/auth/session", "/v1/auth/devices", "/v1/events"]);
  if (known.has(url.pathname) || deviceMatch) throw new HttpError("method_not_allowed", "method is not allowed for this route");
  throw new HttpError("not_found", "route not found");
}

const server = createServer((request, response) => {
  const started = Date.now();
  const requestId = randomUUID();
  response.setHeader("x-request-id", requestId);
  route(request, response).catch((error: unknown) => {
    const httpError = normalizeControlHttpError(error);
    if (!response.headersSent) {
      if ("retryAfter" in httpError && httpError.retryAfter) response.setHeader("retry-after", String(httpError.retryAfter));
      sendJson(response, httpError.status, httpError instanceof NodeHttpError ? nodeErrorBody(httpError, requestId) : { code: httpError.code, message: httpError.message, requestId });
    }
    else response.end();
    if (!isControlHttpError(error) && process.env.NODE_ENV !== "test") console.error("control-api request failed", { method: request.method, path: request.url?.split("?")[0], error: error instanceof Error ? error.name : "unknown" });
  }).finally(() => {
    if (process.env.NODE_ENV !== "test") console.info("control-api request", { method: request.method, path: request.url?.split("?")[0], status: response.statusCode, duration_ms: Date.now() - started });
  });
});

server.headersTimeout = 10_000;
server.requestTimeout = 15_000;
server.keepAliveTimeout = 5_000;
server.maxHeadersCount = 100;
server.listen(config.port, config.bindAddress, () => console.info("control-api listening", { port: config.port, bindAddress: config.bindAddress }));

let shuttingDown = false;
async function shutdown(): Promise<void> {
  if (shuttingDown) return;
  shuttingDown = true;
  for (const sessionId of activeStreams.keys()) closeStream(sessionId);
  const closed = new Promise<void>((resolve) => server.close(() => resolve()));
  const deadline = setTimeout(() => server.closeAllConnections(), 10_000);
  await closed;
  clearTimeout(deadline);
  await database.end();
}
function handleSignal(): void {
  void shutdown().then(() => process.exit(0)).catch((error: unknown) => {
    console.error("control-api shutdown failed", { error: error instanceof Error ? error.name : "unknown" });
    process.exit(1);
  });
}
process.on("SIGTERM", handleSignal);
process.on("SIGINT", handleSignal);
