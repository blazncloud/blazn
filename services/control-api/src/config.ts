import { readFileSync } from "node:fs";

function valueOrFile(name: string): string {
  const direct = process.env[name];
  if (direct) return direct;
  const file = process.env[`${name}_FILE`];
  if (!file) throw new Error(`${name} or ${name}_FILE is required`);
  return readFileSync(file, "utf8").trim();
}

function positiveInteger(name: string, fallback: number): number {
  const raw = process.env[name];
  if (!raw) return fallback;
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) throw new Error(`${name} must be a positive integer`);
  return parsed;
}

export interface Config {
  port: number;
  databaseUrl: string;
  publicUrl: string;
  bootstrapSecret: string;
  sessionTtlSeconds: number;
  deviceCodeTtlSeconds: number;
  s3Endpoint?: string;
}

export function loadConfig(): Config {
  const s3Endpoint = process.env.S3_ENDPOINT;
  return {
    port: positiveInteger("PORT", 8080),
    databaseUrl: valueOrFile("DATABASE_URL"),
    publicUrl: (process.env.PUBLIC_URL ?? "http://127.0.0.1:8080").replace(/\/$/, ""),
    bootstrapSecret: valueOrFile("BLAZN_BOOTSTRAP_SECRET"),
    sessionTtlSeconds: positiveInteger("SESSION_TTL_SECONDS", 60 * 60 * 24 * 30),
    deviceCodeTtlSeconds: positiveInteger("DEVICE_CODE_TTL_SECONDS", 600),
    ...(s3Endpoint ? { s3Endpoint } : {}),
  };
}
