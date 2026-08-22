import { readFileSync } from "node:fs";

function valueOrFile(name: string, fileRequired = process.env.NODE_ENV === "production"): string {
  const file = process.env[`${name}_FILE`];
  if (file) return readFileSync(file, "utf8").trim();
  const direct = process.env[name];
  if (direct && !fileRequired) return direct;
  throw new Error(fileRequired ? `${name}_FILE is required in production` : `${name} or ${name}_FILE is required`);
}

function boundedInteger(name: string, fallback: number, minimum: number, maximum: number): number {
  const raw = process.env[name];
  if (!raw) return fallback;
  if (!/^[0-9]+$/.test(raw)) throw new Error(`${name} must contain only decimal digits`);
  const parsed = Number(raw);
  if (!Number.isSafeInteger(parsed) || parsed < minimum || parsed > maximum) throw new Error(`${name} must be between ${minimum} and ${maximum}`);
  return parsed;
}

export interface Config {
  port: number;
  bindAddress: string;
  databaseUrl: string;
  publicUrl: string;
  sessionTtlSeconds: number;
  refreshTtlSeconds: number;
  deviceCodeTtlSeconds: number;
  s3Endpoint: string;
  s3Region: string;
  s3Bucket: string;
  s3AccessKey: string;
  s3SecretKey: string;
  trustedProxyCidrs: string[];
  trustedProxyHops: number;
  trustedProxySecret?: string;
  zitadel?: {
    issuerUrl: string;
    clientId: string;
    clientSecret: string;
    cookieKey: string;
    requireMfa: boolean;
  };
}

function booleanValue(name: string, fallback: boolean): boolean {
  const value = process.env[name];
  if (value === undefined) return fallback;
  if (value === "true") return true;
  if (value === "false") return false;
  throw new Error(`${name} must be true or false`);
}

function zitadelConfig(): Config["zitadel"] {
  const issuerUrl = process.env.ZITADEL_ISSUER_URL?.trim();
  const clientId = process.env.ZITADEL_CLIENT_ID?.trim();
  const configured = Boolean(issuerUrl || clientId || process.env.ZITADEL_CLIENT_SECRET_FILE || process.env.OIDC_COOKIE_KEY_FILE);
  if (!configured) return undefined;
  if (!issuerUrl || !clientId) throw new Error("ZITADEL_ISSUER_URL and ZITADEL_CLIENT_ID are required when ZITADEL is configured");
  const issuer = new URL(issuerUrl);
  if (issuer.protocol !== "https:" || issuer.search || issuer.hash || issuer.username || issuer.password) throw new Error("ZITADEL_ISSUER_URL must be an HTTPS origin");
  return { issuerUrl: issuer.href.replace(/\/$/, ""), clientId, clientSecret: valueOrFile("ZITADEL_CLIENT_SECRET"), cookieKey: valueOrFile("OIDC_COOKIE_KEY"), requireMfa: booleanValue("ZITADEL_REQUIRE_MFA", true) };
}

function cidrList(name: string): string[] {
  const raw = process.env[name];
  if (!raw) {
    if (process.env.NODE_ENV === "production") throw new Error(`${name} is required in production`);
    return ["127.0.0.0/8", "::1/128"];
  }
  const values = raw.split(",").map((value) => value.trim());
  if (values.length === 0 || values.some((value) => value === "")) throw new Error(`${name} must be a comma-separated list of CIDRs`);
  return values;
}

export function loadConfig(): Config {
  const publicUrl = (process.env.PUBLIC_URL ?? "http://127.0.0.1:8080").replace(/\/$/, "");
  if (process.env.NODE_ENV === "production" && !publicUrl.startsWith("https://")) throw new Error("PUBLIC_URL must use https in production");
  const s3Endpoint = process.env.S3_ENDPOINT;
  if (!s3Endpoint) throw new Error("S3_ENDPOINT is required");
  const trustedProxySecretFile = process.env.TRUSTED_PROXY_SECRET_FILE;
  if (process.env.NODE_ENV === "production" && !trustedProxySecretFile) throw new Error("TRUSTED_PROXY_SECRET_FILE is required in production");
  const trustedProxySecret = trustedProxySecretFile ? readFileSync(trustedProxySecretFile, "utf8").trim() : undefined;
  if (trustedProxySecret !== undefined && (trustedProxySecret.length < 32 || trustedProxySecret.length > 512)) throw new Error("trusted proxy secret must contain between 32 and 512 characters");
  const zitadel = zitadelConfig();
  return {
    port: boundedInteger("PORT", 8080, 1, 65535),
    bindAddress: process.env.BIND_ADDRESS ?? "127.0.0.1",
    databaseUrl: valueOrFile("DATABASE_URL"),
    publicUrl,
    sessionTtlSeconds: boundedInteger("SESSION_TTL_SECONDS", 900, 60, 3600),
    refreshTtlSeconds: boundedInteger("REFRESH_TTL_SECONDS", 60 * 60 * 24 * 30, 3600, 60 * 60 * 24 * 90),
    deviceCodeTtlSeconds: boundedInteger("DEVICE_CODE_TTL_SECONDS", 600, 60, 900),
    s3Endpoint,
    s3Region: process.env.S3_REGION ?? "us-east-1",
    s3Bucket: process.env.S3_BUCKET ?? "blazn-poc",
    s3AccessKey: valueOrFile("S3_ACCESS_KEY"),
    s3SecretKey: valueOrFile("S3_SECRET_KEY"),
    trustedProxyCidrs: cidrList("TRUSTED_PROXY_CIDRS"),
    trustedProxyHops: boundedInteger("TRUSTED_PROXY_HOPS", 1, 1, 8),
    ...(trustedProxySecret === undefined ? {} : { trustedProxySecret }),
    ...(zitadel === undefined ? {} : { zitadel }),
  };
}
