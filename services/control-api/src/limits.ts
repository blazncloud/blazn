import { createHash } from "node:crypto";
import type { IncomingMessage } from "node:http";
import { BlockList, isIP } from "node:net";
import type { Database } from "./db.js";
import { HttpError } from "./http.js";

type AddressFamily = "ipv4" | "ipv6";

interface ParsedAddress {
  address: string;
  family: AddressFamily;
}

function parsedAddress(value: string): ParsedAddress | undefined {
  const trimmed = value.trim();
  const mapped = trimmed.toLowerCase().match(/^::ffff:(\d{1,3}(?:\.\d{1,3}){3})$/)?.[1];
  const address = mapped ?? trimmed;
  const family = isIP(address);
  if (family === 4) return { address, family: "ipv4" };
  if (family === 6) return { address: address.toLowerCase(), family: "ipv6" };
  return undefined;
}

export class TrustedProxyPolicy {
  private readonly blocks = new BlockList();

  constructor(cidrs: readonly string[], readonly hops: number) {
    if (!Number.isSafeInteger(hops) || hops < 1 || hops > 8) throw new Error("trusted proxy hops must be between 1 and 8");
    if (cidrs.length === 0) throw new Error("at least one trusted proxy CIDR is required");
    for (const cidr of cidrs) {
      const separator = cidr.lastIndexOf("/");
      if (separator <= 0 || separator === cidr.length - 1) throw new Error(`invalid trusted proxy CIDR: ${cidr}`);
      const address = parsedAddress(cidr.slice(0, separator));
      const prefixText = cidr.slice(separator + 1);
      if (!address || !/^[0-9]+$/.test(prefixText)) throw new Error(`invalid trusted proxy CIDR: ${cidr}`);
      const prefix = Number(prefixText);
      const maximum = address.family === "ipv4" ? 32 : 128;
      if (!Number.isSafeInteger(prefix) || prefix < 0 || prefix > maximum) throw new Error(`invalid trusted proxy CIDR: ${cidr}`);
      this.blocks.addSubnet(address.address, prefix, address.family);
    }
  }

  contains(value: string): boolean {
    const address = parsedAddress(value);
    return !!address && this.blocks.check(address.address, address.family);
  }
}

export function remoteIdentity(request: IncomingMessage, policy: TrustedProxyPolicy): string {
  const direct = parsedAddress(request.socket.remoteAddress ?? "");
  if (!direct) return "unknown";
  if (!policy.contains(direct.address)) return direct.address;

  const forwarded = request.headers["x-forwarded-for"];
  if (typeof forwarded !== "string") return direct.address;
  const chain = forwarded.split(",").map(parsedAddress);
  if (chain.length !== policy.hops || chain.some((address) => !address)) return direct.address;

  // The direct peer is the final trusted hop. Every forwarded hop after the
  // selected client must also be a configured proxy; exact length prevents a
  // caller-controlled prefix from changing which address is selected.
  for (const proxy of chain.slice(1)) if (!proxy || !policy.contains(proxy.address)) return direct.address;
  return chain[0]?.address ?? direct.address;
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
