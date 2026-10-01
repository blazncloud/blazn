import { readFileSync } from "node:fs";
import { request } from "node:http";
import { NODE_ERROR_STATUS, type NodeErrorCode } from "./node-types.js";

export interface BrokerProxyReply { status: number; body: Buffer; retryAfter?: string }
export interface NodeBrokerProxy { issue(body: Record<string, unknown>, idempotencyKey: string, proof: string, signal: AbortSignal): Promise<BrokerProxyReply>; observe?(issuanceId:string,body:Record<string,unknown>,signal:AbortSignal):Promise<void>; retire?(body:{clusterId:string;nodeName:string;nodeUid:string},signal:AbortSignal):Promise<boolean>; assign?(body:{clusterId:string;nodeName:string;nodeUid:string;workspaceId:string},signal:AbortSignal):Promise<boolean>; hold?(body:{clusterId:string;nodeName:string;nodeUid:string;holdReason:string},signal:AbortSignal):Promise<boolean>; health(signal: AbortSignal): Promise<void> }

const loopbackOrigin = "http://127.0.0.1:8081";
export const NODE_BROKER_CALLER_HEADER = "x-blazn-broker-caller";
const callerKeyPattern = /^[A-Za-z0-9_-]{43,128}$/;

export interface NodeBrokerProxyOptions { origin?: string; callerKey?: string }

// A broker origin other than the fixed loopback sidecar must be a private
// IPv4 http origin with an explicit port, and every call must then carry the
// shared caller key so that other hosts on the private network cannot use the
// broker's unauthenticated observation route.
export function nodeBrokerOrigin(value: string): { origin: string; loopback: boolean } {
  let parsed: URL;
  try { parsed = new URL(value); } catch { throw new Error("BLAZN_NODE_BROKER_URL is invalid"); }
  const octets = parsed.hostname.split(".").map(Number);
  const ipv4 = octets.length === 4 && octets.every((octet, index) => Number.isInteger(octet) && octet >= 0 && octet <= 255 && String(octet) === parsed.hostname.split(".")[index]);
  const loopback = ipv4 && octets[0] === 127;
  const privateHost = ipv4 && (octets[0] === 10 || (octets[0] === 172 && octets[1]! >= 16 && octets[1]! <= 31) || (octets[0] === 192 && octets[1] === 168));
  if (parsed.protocol !== "http:" || parsed.username || parsed.password || parsed.search || parsed.hash || parsed.pathname !== "/" || !parsed.port || !(loopback || privateHost)) throw new Error("BLAZN_NODE_BROKER_URL is invalid");
  return { origin: parsed.origin, loopback };
}
const maxBytes = 16 * 1024;
const statuses = new Set([200, 400, 401, 403, 404, 405, 409, 410, 413, 429, 500, 502, 503, 504]);

export class LoopbackNodeBrokerProxy implements NodeBrokerProxy {
  private readonly origin: string;
  private readonly callerKey?: string;

  constructor(private readonly timeoutMs = 5_000, options: NodeBrokerProxyOptions = {}) {
    if (timeoutMs < 1 || timeoutMs > 30_000) throw new Error("Node broker proxy configuration is invalid");
    const target = nodeBrokerOrigin(options.origin ?? loopbackOrigin);
    if (options.callerKey !== undefined && !callerKeyPattern.test(options.callerKey)) throw new Error("Node broker caller key is invalid");
    if (!target.loopback && options.callerKey === undefined) throw new Error("Node broker caller key is required for a non-loopback broker");
    this.origin = target.origin;
    if (options.callerKey !== undefined) this.callerKey = options.callerKey;
  }

  static fromEnvironment(env: NodeJS.ProcessEnv = process.env): LoopbackNodeBrokerProxy {
    const keyFile = env.BLAZN_NODE_BROKER_CALLER_KEY_FILE;
    const callerKey = keyFile ? readFileSync(keyFile, "utf8").trim() : undefined;
    // Join issuance runs MicroK8s readiness plus add-node on a loaded control
    // plane (observed 5-10s); stay inside the CLI's 30s request deadline.
    return new LoopbackNodeBrokerProxy(25_000, { ...(env.BLAZN_NODE_BROKER_URL ? { origin: env.BLAZN_NODE_BROKER_URL } : {}), ...(callerKey !== undefined ? { callerKey } : {}) });
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

  async retire(body:{clusterId:string;nodeName:string;nodeUid:string},signal:AbortSignal):Promise<boolean>{
    const payload=Buffer.from(JSON.stringify({clusterId:body.clusterId,nodeName:body.nodeName,nodeUid:body.nodeUid}));if(payload.length>maxBytes)throw new Error("Node broker request is too large");
    const reply=await this.call("POST","/v1/node-service/node-retirements",payload,{"content-type":"application/json"},signal);
    const text=reply.body.toString("utf8");
    if(reply.status!==200||(text!=='{"deleted":true}'&&text!=='{"deleted":false}'))throw new Error("Node broker rejected the retired worker removal");
    return text==='{"deleted":true}';
  }

  async assign(body:{clusterId:string;nodeName:string;nodeUid:string;workspaceId:string},signal:AbortSignal):Promise<boolean>{
    const payload=Buffer.from(JSON.stringify({clusterId:body.clusterId,nodeName:body.nodeName,nodeUid:body.nodeUid,workspaceId:body.workspaceId}));if(payload.length>maxBytes)throw new Error("Node broker request is too large");
    const reply=await this.call("POST","/v1/node-service/node-assignments",payload,{"content-type":"application/json"},signal);
    const text=reply.body.toString("utf8");
    if(reply.status!==200||(text!=='{"assigned":true}'&&text!=='{"assigned":false}'))throw new Error("Node broker rejected the worker workspace assignment");
    return text==='{"assigned":true}';
  }

  async hold(body:{clusterId:string;nodeName:string;nodeUid:string;holdReason:string},signal:AbortSignal):Promise<boolean>{
    const payload=Buffer.from(JSON.stringify({clusterId:body.clusterId,nodeName:body.nodeName,nodeUid:body.nodeUid,holdReason:body.holdReason}));if(payload.length>maxBytes)throw new Error("Node broker request is too large");
    const reply=await this.call("POST","/v1/node-service/node-placement-holds",payload,{"content-type":"application/json"},signal);
    const text=reply.body.toString("utf8");
    if(reply.status!==200||(text!=='{"changed":true}'&&text!=='{"changed":false}'))throw new Error("Node broker rejected the worker placement hold");
    return text==='{"changed":true}';
  }

  private call(method: "GET" | "POST", path: string, payload: Buffer, headers: Record<string, string>, signal: AbortSignal): Promise<BrokerProxyReply> {
    return new Promise((resolve, reject) => {
      let deadline: ReturnType<typeof setTimeout>;
      const fail=(error:Error)=>{clearTimeout(deadline);reject(error);};
      const req = request(`${this.origin}${path}`, { method, signal, headers: { ...headers, ...(this.callerKey ? { [NODE_BROKER_CALLER_HEADER]: this.callerKey } : {}), "content-length": String(payload.length), connection: "close" } }, (response) => {
        const chunks: Buffer[] = []; let size = 0;
        response.on("data", (chunk: Buffer) => { size += chunk.length; if (size > maxBytes) req.destroy(new Error("Node broker response is too large")); else chunks.push(chunk); });
        response.on("end", () => {
          const contentType = response.headers["content-type"];
          const retry = response.headers["retry-after"];
          if (!statuses.has(response.statusCode ?? 0) || contentType !== "application/json" || rawHeaderCount(response.rawHeaders,"content-type")!==1 || rawHeaderCount(response.rawHeaders,"retry-after")>1 || rawHeaderCount(response.rawHeaders,"location")!==0 || (Array.isArray(retry) ? retry.length !== 1 : false)) return fail(new Error("Node broker response contract is invalid"));
          const body = Buffer.concat(chunks); try { const parsed:unknown=JSON.parse(body.toString("utf8"));if(path==="/healthz"){if(response.statusCode!==200||JSON.stringify(parsed)!=='{"status":"ok"}')throw new Error();}else if(path.startsWith("/v1/node-service/join-observations/")){if(response.statusCode===200){if(JSON.stringify(parsed)!=='{"verified":true}')throw new Error();}else validateBrokerBody(response.statusCode!,parsed);}else if(path==="/v1/node-service/node-assignments"){if(response.statusCode===200){const text=JSON.stringify(parsed);if(text!=='{"assigned":true}'&&text!=='{"assigned":false}')throw new Error();}else validateBrokerBody(response.statusCode!,parsed);}else if(path==="/v1/node-service/node-placement-holds"){if(response.statusCode===200){const text=JSON.stringify(parsed);if(text!=='{"changed":true}'&&text!=='{"changed":false}')throw new Error();}else validateBrokerBody(response.statusCode!,parsed);}else if(path==="/v1/node-service/node-retirements"){if(response.statusCode===200){const text=JSON.stringify(parsed);if(text!=='{"deleted":true}'&&text!=='{"deleted":false}')throw new Error();}else validateBrokerBody(response.statusCode!,parsed);}else validateBrokerBody(response.statusCode!,parsed); } catch { return fail(new Error("Node broker response JSON is invalid")); }
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
