import type { IncomingMessage,ServerResponse } from "node:http";

const route=/^\/v1\/sandbox-access-grants\/([0-9a-f-]{36})\/(exec|file)$/;
const maximumRequest=8*1024*1024+64*1024;
// Exec may return independently bounded stdout and stderr streams, both base64
// encoded inside JSON. Keep the public proxy's response ceiling above that
// frozen wire shape without widening upload requests.
const maximumResponse=24*1024*1024+64*1024;

export class SandboxAccessProxy {
  private constructor(private readonly origin:string){}
  static fromEnvironment(value:string|undefined=process.env.BLAZN_SANDBOX_ACCESS_URL){
    if(!value)return undefined;
    const parsed=new URL(value);
    if(parsed.protocol!=="http:"||parsed.username||parsed.password||parsed.search||parsed.hash||parsed.pathname!=="/"||!privateHost(parsed.hostname))throw new Error("BLAZN_SANDBOX_ACCESS_URL is invalid");
    return new SandboxAccessProxy(parsed.origin);
  }
  matches(path:string){return route.test(path);}
  async handle(request:IncomingMessage,response:ServerResponse,url:URL){
    const match=route.exec(url.pathname);if(!match||url.search)throw new Error("sandbox access route is invalid");
    const body=request.method==="GET"?undefined:await boundedBody(request);
    const headers:Record<string,string>={};
    for(const name of ["authorization","content-type","accept","x-blazn-sandbox-path","x-content-size","x-content-sha256"]){const value=request.headers[name];if(typeof value==="string")headers[name]=value;}
    const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),60_000);
    let upstream:Response;
    const init:RequestInit={method:request.method??"GET",headers,signal:controller.signal};if(body!==undefined)init.body=body;
    try{upstream=await fetch(`${this.origin}/internal/v1/sandbox-access-grants/${match[1]}/${match[2]}`,init);}
    catch{response.writeHead(503,{"content-type":"application/json","cache-control":"no-store"});response.end(JSON.stringify({code:"sandbox_access_unavailable",message:"sandbox access is unavailable"}));return;}
    finally{clearTimeout(timer);}
    const payload=Buffer.from(await upstream.arrayBuffer());
    if(payload.length>maximumResponse){response.writeHead(502,{"content-type":"application/json","cache-control":"no-store"});response.end(JSON.stringify({code:"sandbox_access_invalid",message:"sandbox access response exceeded its bound"}));return;}
    const returned:Record<string,string>={"cache-control":"no-store"};
    for(const name of ["content-type","x-content-size","x-content-sha256"]){const value=upstream.headers.get(name);if(value)returned[name]=value;}
    returned["content-length"]=String(payload.length);
    response.writeHead(upstream.status,returned);response.end(payload);
  }
}

function privateHost(host:string){
  return /^10\./.test(host)||/^192\.168\./.test(host)||/^172\.(?:1[6-9]|2[0-9]|3[01])\./.test(host);
}

async function boundedBody(request:IncomingMessage){
  const chunks:Buffer[]=[];let size=0;
  for await(const chunk of request){const value=Buffer.isBuffer(chunk)?chunk:Buffer.from(chunk);size+=value.length;if(size>maximumRequest)throw new Error("sandbox access request exceeded its bound");chunks.push(value);}
  return Buffer.concat(chunks,size);
}
