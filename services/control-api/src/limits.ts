import type { IncomingMessage } from "node:http";
import { HttpError } from "./http.js";

interface WindowEntry { count: number; resetAt: number }
const windows = new Map<string, WindowEntry>();

export function remoteIdentity(request: IncomingMessage): string {
  return request.socket.remoteAddress ?? "unknown";
}

export function enforceLimit(scope: string, identity: string, maximum: number, windowMilliseconds: number): void {
  const key = `${scope}:${identity}`;
  const now = Date.now();
  const previous = windows.get(key);
  const entry = !previous || previous.resetAt <= now ? { count: 0, resetAt: now + windowMilliseconds } : previous;
  entry.count += 1;
  windows.set(key, entry);
  if (entry.count > maximum) throw new HttpError(429, "rate_limited", "too many authentication attempts; retry later");
  if (windows.size > 10_000) {
    for (const [candidate, value] of windows) if (value.resetAt <= now) windows.delete(candidate);
  }
}
