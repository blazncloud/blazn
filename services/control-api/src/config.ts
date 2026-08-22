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
}

export function loadConfig(): Config {
  const publicUrl = (process.env.PUBLIC_URL ?? "http://127.0.0.1:8080").replace(/\/$/, "");
  if (process.env.NODE_ENV === "production" && !publicUrl.startsWith("https://")) throw new Error("PUBLIC_URL must use https in production");
  const s3Endpoint = process.env.S3_ENDPOINT;
  if (!s3Endpoint) throw new Error("S3_ENDPOINT is required");
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
  };
}
