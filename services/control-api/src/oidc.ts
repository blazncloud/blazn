import { createHash, createPublicKey, randomBytes, verify } from "node:crypto";

export interface OidcProviderConfig {
  issuerUrl: string;
  clientId: string;
  clientSecret: string;
  callbackUrl: string;
	assurancePolicy: OidcAssurancePolicy;
}

export interface OidcAssurancePolicy {
	provider: "zitadel";
	reviewedRelease: string;
	policyDigest: string;
	acrValues: string[];
	acceptedAmrSets: string[][];
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
  acr: string;
	reviewedRelease: string;
	assurancePolicyDigest: string;
}

export interface IdTokenVerification {
  issuer: string;
  clientId: string;
  nonce: string;
	assurancePolicy: OidcAssurancePolicy;
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
	if (!response.ok) { await response.body?.cancel(); throw new Error(`identity provider returned HTTP ${response.status}`); }
	const maximum = 1024 * 1024;
	const declared = response.headers.get("content-length");
	if (declared && (!/^[0-9]+$/.test(declared) || Number(declared) > maximum)) { await response.body?.cancel(); throw new Error("identity provider response is too large"); }
	if (!response.body) throw new Error("identity provider response body is unavailable");
	const reader = response.body.getReader();
	const chunks: Uint8Array[] = [];
	let size = 0;
	try {
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			size += value.byteLength;
			if (size > maximum) { await reader.cancel(); throw new Error("identity provider response is too large"); }
			chunks.push(value);
		}
	} finally { reader.releaseLock(); }
	const body = Buffer.concat(chunks.map((value) => Buffer.from(value.buffer, value.byteOffset, value.byteLength)), size).toString("utf8");
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

function assuranceSatisfied(acr: string, amr: string[], policy: OidcAssurancePolicy): boolean {
	if (policy.provider !== "zitadel" || !policy.acrValues.includes(acr)) return false;
	const methods = new Set(amr.map((method) => method.toLowerCase()));
	return policy.acceptedAmrSets.some((required) => required.length >= 2 && required.every((method) => methods.has(method.toLowerCase())));
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
	const acr = typeof claims.acr === "string" ? claims.acr : "";
	if (!assuranceSatisfied(acr, amr, input.assurancePolicy)) throw new Error("reviewed ZITADEL assurance and multi-factor policy is required");
  const displayName = typeof claims.name === "string" && claims.name.trim() ? claims.name.trim().slice(0, 128) : claims.email.split("@")[0]!.slice(0, 128);
	return { issuer: String(claims.iss), subject: claims.sub, email: claims.email.trim().toLowerCase(), displayName, amr, acr, reviewedRelease: input.assurancePolicy.reviewedRelease, assurancePolicyDigest: input.assurancePolicy.policyDigest };
}

export class OidcClient {
  private discovery?: Discovery;
  private jwks?: { expiresAt: number; keys: Jwk[] };
	private healthFreshUntil = 0;
	private healthFlight?: Promise<void>;

  constructor(private readonly config: OidcProviderConfig) {
    httpsUrl(config.issuerUrl, "OIDC issuer");
    httpsUrl(config.callbackUrl, "OIDC callback");
  }

  createTransaction(userCode: string, mode: "signin" | "signup"): { state: string; nonce: string; codeVerifier: string; userCode: string; mode: "signin" | "signup"; issuedAt: number } {
    return { state: randomValue(), nonce: randomValue(), codeVerifier: randomValue(48), userCode, mode, issuedAt: Date.now() };
  }

  async authorizationUrl(transaction: ReturnType<OidcClient["createTransaction"]>): Promise<URL> {
    const metadata = await this.metadata();
    const url = httpsUrl(metadata.authorization_endpoint, "authorization endpoint");
		url.search = new URLSearchParams({ response_type: "code", client_id: this.config.clientId, redirect_uri: this.config.callbackUrl, scope: "openid profile email", state: transaction.state, nonce: transaction.nonce, code_challenge: pkceChallenge(transaction.codeVerifier), code_challenge_method: "S256", prompt: "login", acr_values: this.config.assurancePolicy.acrValues.join(" "), ...(transaction.mode === "signup" ? { screen_hint: "signup" } : {}) }).toString();
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
		return verifyOidcIdToken(token.id_token, { issuer: metadata.issuer, clientId: this.config.clientId, nonce: expected.nonce, assurancePolicy: this.config.assurancePolicy, keys: await this.keys(metadata) });
  }

	async health(): Promise<void> {
		if (this.healthFreshUntil > Date.now()) return;
		if (this.healthFlight) return this.healthFlight;
		this.healthFlight = (async () => {
			const metadata = await this.metadata(true);
			const keys = await this.keys(metadata, true);
			if (keys.length === 0) throw new Error("identity provider has no signing keys");
			this.healthFreshUntil = Date.now() + 10_000;
		})();
		try { await this.healthFlight; }
		finally { this.healthFlight = undefined; }
	}

  private async metadata(refresh = false): Promise<Discovery> {
    if (this.discovery && !refresh) return this.discovery;
    const issuer = httpsUrl(this.config.issuerUrl, "OIDC issuer");
    const wellKnown = new URL(".well-known/openid-configuration", issuer.href.endsWith("/") ? issuer : `${issuer.href}/`);
    const metadata = await jsonFetch<Discovery>(wellKnown);
    if (metadata.issuer.replace(/\/$/, "") !== issuer.href.replace(/\/$/, "")) throw new Error("identity provider issuer does not match configuration");
		for (const [label, value] of [["authorization endpoint", metadata.authorization_endpoint], ["token endpoint", metadata.token_endpoint], ["JWKS endpoint", metadata.jwks_uri]] as const) {
			const endpoint = httpsUrl(value, label);
			if (endpoint.origin !== issuer.origin) throw new Error(`${label} must use the reviewed issuer origin`);
		}
    this.discovery = metadata;
    return metadata;
  }

  private async keys(metadata: Discovery, refresh = false): Promise<Jwk[]> {
    if (this.jwks && this.jwks.expiresAt > Date.now() && !refresh) return this.jwks.keys;
    const value = await jsonFetch<{ keys?: Jwk[] }>(httpsUrl(metadata.jwks_uri, "JWKS endpoint"));
    if (!Array.isArray(value.keys) || value.keys.length > 20) throw new Error("identity provider JWKS is invalid");
    this.jwks = { expiresAt: Date.now() + 5 * 60 * 1000, keys: value.keys };
    return value.keys;
  }

}
