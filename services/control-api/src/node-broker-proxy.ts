import { request } from "node:http";
import { NODE_ERROR_STATUS, type NodeErrorCode } from "./node-types.js";

export interface BrokerProxyReply { status: number; body: Buffer; retryAfter?: string }
export interface NodeBrokerProxy { issue(body: Record<string, unknown>, idempotencyKey: string, proof: string, signal: AbortSignal): Promise<BrokerProxyReply>; health(signal: AbortSignal): Promise<void> }

const brokerOrigin = "http://127.0.0.1:8081";
const maxBytes = 16 * 1024;
const statuses = new Set([200, 400, 401, 403, 404, 405, 409, 410, 413, 429, 500, 502, 503, 504]);

export class LoopbackNodeBrokerProxy implements NodeBrokerProxy {
  constructor(private readonly timeoutMs = 5_000) {
    if (timeoutMs < 1 || timeoutMs > 10_000) throw new Error("Node broker proxy configuration is invalid");
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
          const body = Buffer.concat(chunks); try { validateBrokerBody(response.statusCode!, JSON.parse(body.toString("utf8"))); } catch { return reject(new Error("Node broker response JSON is invalid")); }
          if (retry !== undefined && (response.statusCode !== 429 || typeof retry !== "string" || !/^[1-9][0-9]{0,2}$/.test(retry))) return reject(new Error("Node broker retry contract is invalid"));
          resolve({ status: response.statusCode!, body, ...(typeof retry === "string" ? { retryAfter: retry } : {}) });
        });
      });
      req.setTimeout(this.timeoutMs, () => req.destroy(new Error("Node broker proxy deadline exceeded")));
      req.once("error", reject); req.end(payload);
    });
  }
}

function validateBrokerBody(status:number,value:unknown):void{
  if(!value||typeof value!=="object"||Array.isArray(value))throw new Error();const body=value as Record<string,unknown>;
  if(status===200){exact(body,["issuanceId","credential","expiresAt","clusterId","workerOnly","replayed"]);if(typeof body.issuanceId!=="string"||!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(body.issuanceId)||typeof body.credential!=="string"||!/^[A-Za-z0-9_-]{43,4096}$/.test(body.credential)||typeof body.expiresAt!=="string"||!Number.isFinite(Date.parse(body.expiresAt))||typeof body.clusterId!=="string"||!body.clusterId||body.clusterId.length>128||body.workerOnly!==true||typeof body.replayed!=="boolean")throw new Error();return;}
  exact(body,["code","message","requestId"]);if(typeof body.code!=="string"||!(body.code in NODE_ERROR_STATUS)||NODE_ERROR_STATUS[body.code as NodeErrorCode]!==status||typeof body.message!=="string"||!body.message||body.message.length>512||typeof body.requestId!=="string"||!/^[0-9a-f-]{36}$/.test(body.requestId))throw new Error();
}
function exact(value:Record<string,unknown>,keys:string[]):void{if(Object.keys(value).length!==keys.length||keys.some(key=>!(key in value))||Object.keys(value).some(key=>!keys.includes(key)))throw new Error();}
