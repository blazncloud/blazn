import { request } from "node:http";
import { NODE_ERROR_STATUS, type NodeErrorCode } from "./node-types.js";

export interface BrokerProxyReply { status: number; body: Buffer; retryAfter?: string }
export interface NodeBrokerProxy { issue(body: Record<string, unknown>, idempotencyKey: string, proof: string, signal: AbortSignal): Promise<BrokerProxyReply>; observe?(issuanceId:string,body:Record<string,unknown>,signal:AbortSignal):Promise<void>; health(signal: AbortSignal): Promise<void> }

const brokerOrigin = "http://127.0.0.1:8081";
const maxBytes = 16 * 1024;
const statuses = new Set([200, 400, 401, 403, 404, 405, 409, 410, 413, 429, 500, 502, 503, 504]);

export class LoopbackNodeBrokerProxy implements NodeBrokerProxy {
  constructor(private readonly timeoutMs = 5_000) {
    if (timeoutMs < 1 || timeoutMs > 10_000) throw new Error("Node broker proxy configuration is invalid");
  }

  async issue(body: Record<string, unknown>, idempotencyKey: string, proof: string, signal: AbortSignal): Promise<BrokerProxyReply> {
    const payload = Buffer.from(JSON.stringify(body));
    if (payload.length > maxBytes) throw new Error("Node broker request is too large");
    return this.call("POST", "/v1/node-service/join-credentials", payload, { "content-type": "application/json", "idempotency-key": idempotencyKey, "x-blazn-node-proof": proof }, signal);
  }

  async health(signal: AbortSignal): Promise<void> {
    const reply = await this.call("GET", "/healthz", Buffer.alloc(0), {}, signal);
    if (reply.status !== 200 || reply.body.toString("utf8") !== '{"status":"ok"}') throw new Error("Node broker health response is invalid");
  }

  async observe(issuanceId:string,body:Record<string,unknown>,signal:AbortSignal):Promise<void>{
    if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(issuanceId))throw new Error("Node broker observation ID is invalid");
    const payload=Buffer.from(JSON.stringify(body));if(payload.length>maxBytes)throw new Error("Node broker request is too large");
    const reply=await this.call("POST",`/v1/node-service/join-observations/${issuanceId}`,payload,{"content-type":"application/json"},signal);
    if(reply.status!==200||reply.body.toString("utf8")!=='{"verified":true}')throw new Error("Node broker rejected the joined worker observation");
  }

  private call(method: "GET" | "POST", path: string, payload: Buffer, headers: Record<string, string>, signal: AbortSignal): Promise<BrokerProxyReply> {
    return new Promise((resolve, reject) => {
      let deadline: ReturnType<typeof setTimeout>;
      const fail=(error:Error)=>{clearTimeout(deadline);reject(error);};
      const req = request(`${brokerOrigin}${path}`, { method, signal, headers: { ...headers, "content-length": String(payload.length), connection: "close" } }, (response) => {
        const chunks: Buffer[] = []; let size = 0;
        response.on("data", (chunk: Buffer) => { size += chunk.length; if (size > maxBytes) req.destroy(new Error("Node broker response is too large")); else chunks.push(chunk); });
        response.on("end", () => {
          const contentType = response.headers["content-type"];
          const retry = response.headers["retry-after"];
          if (!statuses.has(response.statusCode ?? 0) || contentType !== "application/json" || rawHeaderCount(response.rawHeaders,"content-type")!==1 || rawHeaderCount(response.rawHeaders,"retry-after")>1 || rawHeaderCount(response.rawHeaders,"location")!==0 || (Array.isArray(retry) ? retry.length !== 1 : false)) return fail(new Error("Node broker response contract is invalid"));
          const body = Buffer.concat(chunks); try { const parsed:unknown=JSON.parse(body.toString("utf8"));if(path==="/healthz"){if(response.statusCode!==200||JSON.stringify(parsed)!=='{"status":"ok"}')throw new Error();}else if(path.startsWith("/v1/node-service/join-observations/")){if(response.statusCode===200){if(JSON.stringify(parsed)!=='{"verified":true}')throw new Error();}else validateBrokerBody(response.statusCode!,parsed);}else validateBrokerBody(response.statusCode!,parsed); } catch { return fail(new Error("Node broker response JSON is invalid")); }
          if (retry !== undefined && (response.statusCode !== 429 || typeof retry !== "string" || !/^[1-9][0-9]{0,2}$/.test(retry))) return fail(new Error("Node broker retry contract is invalid"));
          clearTimeout(deadline);resolve({ status: response.statusCode!, body, ...(typeof retry === "string" ? { retryAfter: retry } : {}) });
        });
      });
      deadline=setTimeout(()=>req.destroy(new Error("Node broker proxy deadline exceeded")),this.timeoutMs);
      req.once("error",fail);req.end(payload);
    });
  }
}

function validateBrokerBody(status:number,value:unknown):void{
  if(!value||typeof value!=="object"||Array.isArray(value))throw new Error();const body=value as Record<string,unknown>;
  if(status===200){exact(body,["issuanceId","credential","expiresAt","clusterId","workerOnly","replayed"]);if(typeof body.issuanceId!=="string"||!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(body.issuanceId)||typeof body.credential!=="string"||!/^[A-Za-z0-9_-]{43,4096}$/.test(body.credential)||typeof body.expiresAt!=="string"||!validRFC3339(body.expiresAt)||typeof body.clusterId!=="string"||!body.clusterId||body.clusterId.length>128||body.workerOnly!==true||typeof body.replayed!=="boolean")throw new Error();return;}
  exact(body,["code","message","requestId"]);if(typeof body.code!=="string"||!(body.code in NODE_ERROR_STATUS)||NODE_ERROR_STATUS[body.code as NodeErrorCode]!==status||typeof body.message!=="string"||!body.message||body.message.length>1024||typeof body.requestId!=="string"||body.requestId.length<1||body.requestId.length>128)throw new Error();
}
function exact(value:Record<string,unknown>,keys:string[]):void{if(Object.keys(value).length!==keys.length||keys.some(key=>!(key in value))||Object.keys(value).some(key=>!keys.includes(key)))throw new Error();}
function rawHeaderCount(headers:string[],name:string):number{let count=0;for(let index=0;index<headers.length;index+=2)if(headers[index]?.toLowerCase()===name)count++;return count;}
function validRFC3339(value:string):boolean{const match=value.match(/^(\d{4})-(0[1-9]|1[0-2])-([012]\d|3[01])T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d{1,9})?(?:Z|[+-]([01]\d|2[0-3]):[0-5]\d)$/);if(!match||!Number.isFinite(Date.parse(value)))return false;const year=Number(match[1]),month=Number(match[2]),day=Number(match[3]);return day<=new Date(Date.UTC(year,month,0)).getUTCDate();}
