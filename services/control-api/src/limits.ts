import { createHash } from "node:crypto";
import type { IncomingMessage } from "node:http";
import { isIP } from "node:net";
import type { Database } from "./db.js";
import { HttpError } from "./http.js";

function isLoopback(address: string): boolean {
  return address === "127.0.0.1" || address === "::1" || address === "::ffff:127.0.0.1";
}

export function remoteIdentity(request: IncomingMessage): string {
  const direct = request.socket.remoteAddress ?? "unknown";
  const forwarded = request.headers["x-forwarded-for"];
  if (isLoopback(direct) && typeof forwarded === "string") {
    const candidate = forwarded.split(",", 1)[0]?.trim() ?? "";
    if (isIP(candidate)) return candidate;
  }
  return direct;
}

export async function enforceLimit(database: Database, scope: string, identity: string, maximum: number, windowSeconds: number): Promise<void> {
  const key = createHash("sha256").update(`${scope}:${identity}`).digest("hex");
  const result = await database.query<{ count: number; retry_after: number }>(`INSERT INTO auth_rate_limits(key, window_start, count)
    VALUES ($1, now(), 1)
    ON CONFLICT (key) DO UPDATE SET
      window_start = CASE WHEN auth_rate_limits.window_start + ($2 * interval '1 second') <= now() THEN now() ELSE auth_rate_limits.window_start END,
      count = CASE WHEN auth_rate_limits.window_start + ($2 * interval '1 second') <= now() THEN 1 ELSE auth_rate_limits.count + 1 END
    RETURNING count, greatest(1, ceil(extract(epoch FROM (window_start + ($2 * interval '1 second') - now()))))::int AS retry_after`, [key, windowSeconds]);
  const row = result.rows[0];
  if (row && row.count > maximum) throw new HttpError("rate_limited", "too many authentication attempts; retry later", row.retry_after);
  if (Math.random() < 0.01) await database.query("DELETE FROM auth_rate_limits WHERE window_start < now() - interval '1 day'");
}
