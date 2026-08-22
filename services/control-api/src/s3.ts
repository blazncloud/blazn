import { createHash, createHmac } from "node:crypto";

function sha256(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

function hmac(key: Buffer | string, value: string): Buffer {
  return createHmac("sha256", key).update(value).digest();
}

export async function verifyBucket(endpoint: string, region: string, bucket: string, accessKey: string, secretKey: string): Promise<void> {
  const base = new URL(endpoint);
  if (base.protocol !== "http:" && base.protocol !== "https:") throw new Error("S3 endpoint must use HTTP or HTTPS");
  const path = `${base.pathname.replace(/\/$/, "")}/${encodeURIComponent(bucket)}`.replace(/^([^/])/, "/$1");
  const now = new Date();
  const amzDate = now.toISOString().replace(/[:-]|\.\d{3}/g, "");
  const date = amzDate.slice(0, 8);
  const payloadHash = sha256("");
  const canonicalHeaders = `host:${base.host}\nx-amz-content-sha256:${payloadHash}\nx-amz-date:${amzDate}\n`;
  const signedHeaders = "host;x-amz-content-sha256;x-amz-date";
  const canonicalRequest = `HEAD\n${path}\n\n${canonicalHeaders}\n${signedHeaders}\n${payloadHash}`;
  const scope = `${date}/${region}/s3/aws4_request`;
  const stringToSign = `AWS4-HMAC-SHA256\n${amzDate}\n${scope}\n${sha256(canonicalRequest)}`;
  const dateKey = hmac(`AWS4${secretKey}`, date);
  const regionKey = hmac(dateKey, region);
  const serviceKey = hmac(regionKey, "s3");
  const signingKey = hmac(serviceKey, "aws4_request");
  const signature = createHmac("sha256", signingKey).update(stringToSign).digest("hex");
  const authorization = `AWS4-HMAC-SHA256 Credential=${accessKey}/${scope}, SignedHeaders=${signedHeaders}, Signature=${signature}`;
  const response = await fetch(new URL(path, base), { method: "HEAD", headers: { authorization, "x-amz-content-sha256": payloadHash, "x-amz-date": amzDate }, signal: AbortSignal.timeout(2_000) });
  if (!response.ok) throw new Error(`S3 bucket verification returned HTTP ${response.status}`);
}
