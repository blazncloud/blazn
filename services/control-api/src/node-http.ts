import type { IncomingMessage, ServerResponse } from "node:http";
import { jsonBody, sendJson } from "./http.js";
import type { NodeService } from "./node-service.js";
import { NodeHttpError, type NodeArchitecture, type NodeOperationType, type NodePlatform, type NodePrincipal } from "./node-types.js";

const UUID=/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class NodeHttpRouter {
  constructor(private readonly service:NodeService,private readonly pollIntervalMs=1_000,private readonly maxLifetimeMs=15*60_000){}
  matches(path:string):boolean{return path.startsWith("/v1/nodes/")||/^\/v1\/workspaces\/[^/]+\/nodes$/.test(path)||/^\/v1\/workspaces\/[^/]+\/node-enrollments$/.test(path)||/^\/v1\/node-enrollments\/[^/]+\/exchange$/.test(path)||path==="/v1/node-service/heartbeats"||/^\/v1\/node-service\/join-credentials\/[^/]+\/consume$/.test(path);}
  async handle(request:IncomingMessage,response:ServerResponse,url:URL,authenticate:()=>Promise<NodePrincipal>):Promise<void>{
    const path=url.pathname;
    const createEnrollment=path.match(/^\/v1\/workspaces\/([^/]+)\/node-enrollments$/);
    if(createEnrollment){if(request.method!=="POST")throw method();const principal=await authenticate();const body=await jsonBody(request);exact(body,["name","mode","platform"],["architecture"]);const result=await this.service.createEnrollment(principal,uuid(createEnrollment[1]!,"workspaceId"),idempotency(request),{name:string(body.name,"name",128),mode:one(body.mode,"mode",["fresh","adopt"]),platform:one(body.platform,"platform",["linux","macos"]) as NodePlatform,...(body.architecture===undefined?{}:{architecture:one(body.architecture,"architecture",["amd64","arm64"]) as NodeArchitecture})});return sendJson(response,201,result);}
    const exchange=path.match(/^\/v1\/node-enrollments\/([^/]+)\/exchange$/);
    if(exchange){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["token","machineFingerprint","nodePublicKey","platform","architecture"],["kubernetesBinding"]);const binding=body.kubernetesBinding===undefined?undefined:object(body.kubernetesBinding,"kubernetesBinding");if(binding)exact(binding,["clusterId","nodeName","nodeUid","resourceVersion"]);const result=await this.service.exchangeEnrollment(uuid(exchange[1]!,"enrollmentId"),{token:string(body.token,"token",128),machineFingerprint:string(body.machineFingerprint,"machineFingerprint",64),nodePublicKey:string(body.nodePublicKey,"nodePublicKey",64),platform:one(body.platform,"platform",["linux","macos"]) as NodePlatform,architecture:one(body.architecture,"architecture",["amd64","arm64"]) as NodeArchitecture,...(binding?{kubernetesBinding:{clusterId:string(binding.clusterId,"clusterId",128),nodeName:string(binding.nodeName,"nodeName",253),nodeUid:string(binding.nodeUid,"nodeUid",128),resourceVersion:string(binding.resourceVersion,"resourceVersion",128)}}:{})});return sendJson(response,200,result);}
    const list=path.match(/^\/v1\/workspaces\/([^/]+)\/nodes$/);
    if(list){if(request.method!=="GET")throw method();return sendJson(response,200,await this.service.listNodes(await authenticate(),uuid(list[1]!,"workspaceId")));}
    if(path==="/v1/node-service/heartbeats"){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["nodeId","identityGeneration","bootId","sequence","sentAt","capabilityDigest","capability"]);await this.service.heartbeat({nodeId:uuid(string(body.nodeId,"nodeId",64),"nodeId"),identityGeneration:integer(body.identityGeneration,"identityGeneration",1),bootId:string(body.bootId,"bootId",128),sequence:integer(body.sequence,"sequence",0),sentAt:string(body.sentAt,"sentAt",64),capabilityDigest:string(body.capabilityDigest,"capabilityDigest",71),capability:object(body.capability,"capability")},proof(request));response.writeHead(204,{"cache-control":"no-store"});return response.end();}
    const consume=path.match(/^\/v1\/node-service\/join-credentials\/([^/]+)\/consume$/);
    if(consume){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["nodeId","enrollmentId","planId","joinedNodeUid","joinedNodeName","resourceVersion","clusterId"]);const result=await this.service.consumeJoin(uuid(consume[1]!,"issuanceId"),{nodeId:uuid(string(body.nodeId,"nodeId",64),"nodeId"),enrollmentId:uuid(string(body.enrollmentId,"enrollmentId",64),"enrollmentId"),planId:uuid(string(body.planId,"planId",64),"planId"),joinedNodeUid:string(body.joinedNodeUid,"joinedNodeUid",128),joinedNodeName:string(body.joinedNodeName,"joinedNodeName",253),resourceVersion:string(body.resourceVersion,"resourceVersion",128),clusterId:string(body.clusterId,"clusterId",128)},proof(request));return sendJson(response,200,result);}
    const operations=path.match(/^\/v1\/nodes\/([^/]+)\/operations$/);
    if(operations){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["type","expectedVersion","parameters"]);const result=await this.service.createOperation(await authenticate(),uuid(operations[1]!,"nodeId"),idempotency(request),{type:one(body.type,"type",["pause","resume","label","cordon","uncordon","rotate_identity","repair","update","drain","remove"]) as NodeOperationType,expectedVersion:integer(body.expectedVersion,"expectedVersion",1),parameters:object(body.parameters,"parameters")});return sendJson(response,202,result);}
    const events=path.match(/^\/v1\/nodes\/([^/]+)\/events$/);
    if(events){if(request.method!=="GET")throw method();return this.stream(request,response,uuid(events[1]!,"nodeId"),authenticate);}
    const node=path.match(/^\/v1\/nodes\/([^/]+)$/);
    if(node){if(request.method!=="GET")throw method();return sendJson(response,200,await this.service.getNode(await authenticate(),uuid(node[1]!,"nodeId")));}
    throw new NodeHttpError("node_not_found","node route was not found");
  }
  private async stream(request:IncomingMessage,response:ServerResponse,nodeId:string,authenticate:()=>Promise<NodePrincipal>):Promise<void>{
    const headers=request.headersDistinct["last-event-id"]??[];if(headers.length>1||(headers[0]&&!UUID.test(headers[0])))throw new NodeHttpError("invalid_request","Last-Event-ID is invalid");let cursor=headers[0]??"";let principal=await authenticate();await this.service.eventBatch(principal,nodeId,cursor);const started=Date.now();response.writeHead(200,{"content-type":"text/event-stream","cache-control":"no-store",connection:"keep-alive"});response.write(`event: ready\nid: ${cursor}\ndata: {}\n\n`);
    while(!response.destroyed&&!response.writableEnded&&Date.now()-started<this.maxLifetimeMs){await new Promise(r=>setTimeout(r,this.pollIntervalMs));if(response.destroyed||response.writableEnded)break;try{principal=await authenticate();const events=await this.service.eventBatch(principal,nodeId,cursor);for(const event of events){cursor=event.id;if(!response.write(`event: ${event.type}\nid: ${event.id}\ndata: ${JSON.stringify(event.payload)}\n\n`))await onceDrain(response);}if(events.length===0)response.write(": heartbeat\n\n");}catch{break;}}
    if(!response.destroyed&&!response.writableEnded)response.end();
  }
}

function exact(body:Record<string,unknown>,required:string[],optional:string[]=[]){if(required.some(k=>!(k in body))||Object.keys(body).some(k=>!required.includes(k)&&!optional.includes(k)))throw new NodeHttpError("invalid_request",`request body must contain exactly: ${required.join(", ")}`);}
function string(v:unknown,name:string,max:number):string{if(typeof v!=="string"||!v||v.length>max)throw new NodeHttpError("invalid_request",`${name} is invalid`);return v;}
function object(v:unknown,name:string):Record<string,unknown>{if(!v||typeof v!=="object"||Array.isArray(v))throw new NodeHttpError("invalid_request",`${name} must be an object`);return v as Record<string,unknown>;}
function integer(v:unknown,name:string,min:number):number{if(typeof v!=="number"||!Number.isSafeInteger(v)||v<min)throw new NodeHttpError("invalid_request",`${name} is invalid`);return v;}
function one<T extends string>(v:unknown,name:string,values:readonly T[]):T{if(typeof v!=="string"||!values.includes(v as T))throw new NodeHttpError("invalid_request",`${name} is invalid`);return v as T;}
function uuid(v:string,name:string):string{if(!UUID.test(v))throw new NodeHttpError("invalid_request",`${name} must be a UUID`);return v;}
function idempotency(r:IncomingMessage):string{const v=r.headersDistinct["idempotency-key"]??[];if(v.length!==1||v[0]!.length<8||v[0]!.length>128)throw new NodeHttpError("invalid_request","Idempotency-Key is required");return v[0]!;}
function proof(r:IncomingMessage):string{const v=r.headersDistinct["x-blazn-node-proof"]??[];if(v.length!==1||!/^[A-Za-z0-9_-]{86}$/.test(v[0]!))throw new NodeHttpError("identity_rejected","X-Blazn-Node-Proof is required");return v[0]!;}
function method(){return new NodeHttpError("method_not_allowed","method is not allowed for this route");}
function onceDrain(response:ServerResponse):Promise<void>{return new Promise((resolve,reject)=>{response.once("drain",resolve);response.once("close",resolve);response.once("error",reject);});}
