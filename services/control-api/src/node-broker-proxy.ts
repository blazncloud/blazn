import { request } from "node:http";

export interface BrokerProxyReply { status: number; body: Buffer; retryAfter?: string }
export interface NodeBrokerProxy { issue(body: Record<string, unknown>, idempotencyKey: string, proof: string, signal: AbortSignal): Promise<BrokerProxyReply>; health(signal: AbortSignal): Promise<void> }

const brokerOrigin = "http://127.0.0.1:8081";
const maxBytes = 16 * 1024;
const statuses = new Set([200, 400, 401, 404, 405, 409, 413, 429, 500, 502, 503, 504]);

export class LoopbackNodeBrokerProxy implements NodeBrokerProxy {
  constructor(origin = brokerOrigin, private readonly timeoutMs = 5_000) {
    if (origin !== brokerOrigin || timeoutMs < 1 || timeoutMs > 10_000) throw new Error("Node broker proxy configuration is invalid");
  }

  async issue(body: Record<string, unknown>, idempotencyKey: string, proof: string, signal: AbortSignal): Promise<BrokerProxyReply> {
    const payload = Buffer.from(JSON.stringify(body));
    return this.call("POST", "/v1/node-service/join-credentials", payload, { "content-type": "application/json", "idempotency-key": idempotencyKey, "x-blazn-node-proof": proof }, signal);
  }

  async health(signal: AbortSignal): Promise<void> {
    const reply = await this.call("GET", "/healthz", Buffer.alloc(0), {}, signal);
    if (reply.status !== 200 || reply.body.toString("utf8") !== '{"status":"ok"}') throw new Error("Node broker health response is invalid");
  }

  private call(method: "GET" | "POST", path: string, payload: Buffer, headers: Record<string, string>, signal: AbortSignal): Promise<BrokerProxyReply> {
    return new Promise((resolve, reject) => {
      const req = request(`${brokerOrigin}${path}`, { method, signal, headers: { ...headers, "content-length": String(payload.length), connection: "close" } }, (response) => {
        const chunks: Buffer[] = []; let size = 0;
        response.on("data", (chunk: Buffer) => { size += chunk.length; if (size > maxBytes) req.destroy(new Error("Node broker response is too large")); else chunks.push(chunk); });
        response.on("end", () => {
          const contentType = response.headers["content-type"];
          const retry = response.headers["retry-after"];
          if (!statuses.has(response.statusCode ?? 0) || contentType !== "application/json" || (Array.isArray(retry) ? retry.length !== 1 : false)) return reject(new Error("Node broker response contract is invalid"));
          const body = Buffer.concat(chunks); try { const parsed: unknown = JSON.parse(body.toString("utf8")); if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(); } catch { return reject(new Error("Node broker response JSON is invalid")); }
          resolve({ status: response.statusCode!, body, ...(typeof retry === "string" ? { retryAfter: retry } : {}) });
        });
      });
      req.setTimeout(this.timeoutMs, () => req.destroy(new Error("Node broker proxy deadline exceeded")));
      req.once("error", reject); req.end(payload);
    });
  }
}
