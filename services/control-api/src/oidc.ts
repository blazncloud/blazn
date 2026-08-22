import { createHash, createPublicKey, randomBytes, verify } from "node:crypto";

export interface OidcProviderConfig {
  issuerUrl: string;
  clientId: string;
  clientSecret: string;
  callbackUrl: string;
  requireMfa: boolean;
}

interface Discovery {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  jwks_uri: string;
}

export interface Jwk {
  kid?: string | undefined;
  kty?: string | undefined;
  use?: string | undefined;
  alg?: string | undefined;
  [key: string]: unknown;
}

export interface OidcIdentity {
  issuer: string;
  subject: string;
  email: string;
  displayName: string;
  amr: string[];
}

export interface IdTokenVerification {
  issuer: string;
  clientId: string;
  nonce: string;
  requireMfa: boolean;
  keys: Jwk[];
  now?: number;
}

function httpsUrl(value: string, label: string): URL {
  const url = new URL(value);
  if (url.protocol !== "https:" || url.username || url.password) throw new Error(`${label} must be an HTTPS URL without credentials`);
  return url;
}

async function jsonFetch<T>(url: URL, init?: RequestInit): Promise<T> {
  const response = await fetch(url, { ...init, redirect: "error", signal: AbortSignal.timeout(8_000) });
  if (!response.ok) throw new Error(`identity provider returned HTTP ${response.status}`);
  const body = await response.text();
  if (body.length > 1024 * 1024) throw new Error("identity provider response is too large");
  return JSON.parse(body) as T;
}

function randomValue(bytes = 32): string { return randomBytes(bytes).toString("base64url"); }
function pkceChallenge(verifier: string): string { return createHash("sha256").update(verifier).digest("base64url"); }

function parseSegment<T>(segment: string): T {
  if (!/^[A-Za-z0-9_-]+$/.test(segment)) throw new Error("ID token encoding is invalid");
  return JSON.parse(Buffer.from(segment, "base64url").toString("utf8")) as T;
}

function audienceIncludes(audience: unknown, clientId: string): boolean {
  return audience === clientId || (Array.isArray(audience) && audience.every((item) => typeof item === "string") && audience.includes(clientId));
}

function mfaSatisfied(amr: string[]): boolean {
  return amr.some((method) => ["mfa", "otp", "totp", "webauthn", "hwk", "swk"].includes(method.toLowerCase()));
}

export function verifyOidcIdToken(encoded: string, input: IdTokenVerification): OidcIdentity {
  const segments = encoded.split(".");
  if (segments.length !== 3) throw new Error("ID token is malformed");
  const header = parseSegment<{ alg?: string; kid?: string; typ?: string }>(segments[0]!);
  const claims = parseSegment<Record<string, unknown>>(segments[1]!);
  if (header.alg !== "RS256" || typeof header.kid !== "string") throw new Error("ID token uses an unsupported signing algorithm");
  const jwk = input.keys.find((item) => item.kid === header.kid && item.kty === "RSA" && (!item.use || item.use === "sig") && (!item.alg || item.alg === "RS256"));
  if (!jwk) throw new Error("ID token signing key is unavailable");
  const valid = verify("RSA-SHA256", Buffer.from(`${segments[0]}.${segments[1]}`), createPublicKey({ key: jwk as unknown as import("node:crypto").JsonWebKey, format: "jwk" }), Buffer.from(segments[2]!, "base64url"));
  if (!valid) throw new Error("ID token signature is invalid");
  const now = input.now ?? Math.floor(Date.now() / 1000);
  if (claims.iss !== input.issuer || !audienceIncludes(claims.aud, input.clientId) || typeof claims.exp !== "number" || claims.exp < now - 30 || typeof claims.iat !== "number" || claims.iat > now + 30 || claims.nonce !== input.nonce) throw new Error("ID token claims are invalid");
  if (Array.isArray(claims.aud) && claims.aud.length > 1 && claims.azp !== input.clientId) throw new Error("ID token authorized party is invalid");
  if (claims.email_verified !== true || typeof claims.email !== "string" || typeof claims.sub !== "string") throw new Error("a verified email identity is required");
  const amr = Array.isArray(claims.amr) && claims.amr.every((value) => typeof value === "string") ? claims.amr as string[] : [];
  if (input.requireMfa && !mfaSatisfied(amr)) throw new Error("multi-factor authentication is required");
  const displayName = typeof claims.name === "string" && claims.name.trim() ? claims.name.trim().slice(0, 128) : claims.email.split("@")[0]!.slice(0, 128);
  return { issuer: String(claims.iss), subject: claims.sub, email: claims.email.trim().toLowerCase(), displayName, amr };
}

export class OidcClient {
  private discovery?: Discovery;
  private jwks?: { expiresAt: number; keys: Jwk[] };

  constructor(private readonly config: OidcProviderConfig) {
    httpsUrl(config.issuerUrl, "OIDC issuer");
    httpsUrl(config.callbackUrl, "OIDC callback");
  }

  createTransaction(userCode: string, mode: "signin" | "signup"): { state: string; nonce: string; codeVerifier: string; userCode: string; mode: "signin" | "signup"; issuedAt: number } {
    return { state: randomValue(), nonce: randomValue(), codeVerifier: randomValue(48), userCode, mode, issuedAt: Date.now() };
  }

  async authorizationUrl(transaction: ReturnType<OidcClient["createTransaction"]>, connection?: string): Promise<URL> {
    const metadata = await this.metadata();
    const url = httpsUrl(metadata.authorization_endpoint, "authorization endpoint");
    url.search = new URLSearchParams({ response_type: "code", client_id: this.config.clientId, redirect_uri: this.config.callbackUrl, scope: "openid profile email", state: transaction.state, nonce: transaction.nonce, code_challenge: pkceChallenge(transaction.codeVerifier), code_challenge_method: "S256", prompt: "login", ...(transaction.mode === "signup" ? { screen_hint: "signup" } : {}), ...(connection ? { connection } : {}) }).toString();
    return url;
  }

  async callback(callbackUrl: URL, expected: { state: string; nonce: string; codeVerifier: string }): Promise<OidcIdentity> {
    const state = callbackUrl.searchParams.get("state") ?? "";
    const code = callbackUrl.searchParams.get("code") ?? "";
    if (state !== expected.state || !code) throw new Error("identity callback state is invalid");
    const metadata = await this.metadata();
    const tokenEndpoint = httpsUrl(metadata.token_endpoint, "token endpoint");
    const token = await jsonFetch<{ id_token?: string }>(tokenEndpoint, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded", accept: "application/json" }, body: new URLSearchParams({ grant_type: "authorization_code", client_id: this.config.clientId, client_secret: this.config.clientSecret, redirect_uri: this.config.callbackUrl, code, code_verifier: expected.codeVerifier }) });
    if (!token.id_token) throw new Error("identity provider did not return an ID token");
    return verifyOidcIdToken(token.id_token, { issuer: metadata.issuer, clientId: this.config.clientId, nonce: expected.nonce, requireMfa: this.config.requireMfa, keys: await this.keys(metadata) });
  }

  private async metadata(): Promise<Discovery> {
    if (this.discovery) return this.discovery;
    const issuer = httpsUrl(this.config.issuerUrl, "OIDC issuer");
    const wellKnown = new URL(".well-known/openid-configuration", issuer.href.endsWith("/") ? issuer : `${issuer.href}/`);
    const metadata = await jsonFetch<Discovery>(wellKnown);
    if (metadata.issuer.replace(/\/$/, "") !== issuer.href.replace(/\/$/, "")) throw new Error("identity provider issuer does not match configuration");
    for (const [label, value] of [["authorization endpoint", metadata.authorization_endpoint], ["token endpoint", metadata.token_endpoint], ["JWKS endpoint", metadata.jwks_uri]] as const) httpsUrl(value, label);
    this.discovery = metadata;
    return metadata;
  }

  private async keys(metadata: Discovery): Promise<Jwk[]> {
    if (this.jwks && this.jwks.expiresAt > Date.now()) return this.jwks.keys;
    const value = await jsonFetch<{ keys?: Jwk[] }>(httpsUrl(metadata.jwks_uri, "JWKS endpoint"));
    if (!Array.isArray(value.keys) || value.keys.length > 20) throw new Error("identity provider JWKS is invalid");
    this.jwks = { expiresAt: Date.now() + 5 * 60 * 1000, keys: value.keys };
    return value.keys;
  }

}
