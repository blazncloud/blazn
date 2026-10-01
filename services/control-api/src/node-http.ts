import type { IncomingMessage, ServerResponse } from "node:http";
import { jsonBody, sendJson } from "./http.js";
import type { NodeService } from "./node-service.js";
import { NodeHttpError, type NodeArchitecture, type NodeOperationType, type NodePlatform, type NodePrincipal, type NodeView } from "./node-types.js";
import type { BrokerProxyReply, NodeBrokerProxy } from "./node-broker-proxy.js";

const UUID=/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class NodeHttpRouter {
  constructor(private readonly service:NodeService,private readonly broker?:NodeBrokerProxy,private readonly limitJoin?:(request:IncomingMessage)=>Promise<void>,private readonly pollIntervalMs=1_000,private readonly maxLifetimeMs=15*60_000){}
  // The retired node has already left the cluster; ask the broker to delete
  // its Node object. Retirement is authoritative in the database, so a
  // cluster cleanup failure is logged and the retry of the same request (a
  // replay) attempts it again.
  // An activated node is labeled with its workspace so the sandbox controller
  // can pin the workspace's sandboxes to it. Activation is already durable;
  // a failed assignment is retried here and on any replay of the activation,
  // and until it succeeds the node simply receives no sandboxes.
  private async assignWorkspace(node:NodeView|undefined):Promise<void>{
    const binding=node?.kubernetesBinding;if(!node||!binding||!this.broker?.assign)return;
    for(let attempt=1;attempt<=3;attempt++){
      try{await this.broker.assign({clusterId:binding.clusterId,nodeName:binding.nodeName,nodeUid:binding.nodeUid,workspaceId:node.workspaceId},AbortSignal.timeout(15_000));return;}
      catch(error){if(attempt===3){process.stderr.write(`control-api node workspace assignment failed nodeId=${node.id} reason=${JSON.stringify(error instanceof Error?error.message.slice(0,200):"unknown")}\n`);return;}await new Promise(resolve=>setTimeout(resolve,1_000*attempt));}
    }
  }
  // reconcilePlacement brings each bound node's placement-hold taint in line
  // with its state: a paused, quarantined, draining or offline node is held,
  // so no new sandbox is scheduled on it, and an active node is released.
  // It is idempotent; failures are retried on the next pass.
  async reconcilePlacement(nodeId?:string):Promise<void>{
    if(!this.broker?.hold)return;
    for(const drift of await this.service.placementDrift(50,nodeId)){
      try{
        await this.broker.hold({clusterId:drift.clusterId,nodeName:drift.nodeName,nodeUid:drift.nodeUid,holdReason:drift.desired??""},AbortSignal.timeout(15_000));
        await this.service.recordPlacementHold({nodeId:drift.nodeId,workspaceId:drift.workspaceId,prior:drift.applied,applied:drift.desired});
      }catch(error){process.stderr.write(`control-api node placement hold failed nodeId=${drift.nodeId} hold=${drift.desired??"none"} reason=${JSON.stringify(error instanceof Error?error.message.slice(0,200):"unknown")}\n`);}
    }
  }

  private placementTimer?:NodeJS.Timeout|undefined;
  startPlacementReconciler(intervalMs=30_000):void{
    if(!this.broker?.hold||this.placementTimer)return;
    let running=false;
    this.placementTimer=setInterval(()=>{if(running)return;running=true;this.reconcilePlacement().catch(error=>process.stderr.write(`control-api node placement reconcile failed reason=${JSON.stringify(error instanceof Error?error.message.slice(0,200):"unknown")}\n`)).finally(()=>{running=false;});},intervalMs);
    this.placementTimer.unref();
  }
  stopPlacementReconciler():void{if(this.placementTimer)clearInterval(this.placementTimer);this.placementTimer=undefined;}


  private async removeRetiredWorker(node:NodeView):Promise<void>{
    const binding=node.kubernetesBinding;if(!binding||!this.broker?.retire)return;
    try{await this.broker.retire({clusterId:binding.clusterId,nodeName:binding.nodeName,nodeUid:binding.nodeUid},AbortSignal.timeout(20_000));}
    catch(error){process.stderr.write(`control-api retired worker cleanup failed nodeId=${node.id} reason=${JSON.stringify(error instanceof Error?error.message.slice(0,200):"unknown")}\n`);}
  }
  matches(path:string):boolean{return path.startsWith("/v1/nodes/")||/^\/v1\/workspaces\/[^/]+\/nodes$/.test(path)||/^\/v1\/workspaces\/[^/]+\/node-enrollments$/.test(path)||path==="/v1/node-service/join-credentials"||/^\/v1\/node-enrollments\/[^/]+\/exchange$/.test(path)||path==="/v1/node-service/heartbeats"||path==="/v1/node-service/activations"||path==="/v1/node-service/retirements"||/^\/v1\/node-service\/join-credentials\/[^/]+\/consume$/.test(path);}
  async handle(request:IncomingMessage,response:ServerResponse,url:URL,authenticate:()=>Promise<NodePrincipal>):Promise<void>{
    const path=url.pathname;
    if(path==="/v1/node-service/join-credentials"){
      if(url.search!=="")throw new NodeHttpError("invalid_request","query parameters are not accepted");
      if(request.method!=="POST")throw method();if(!this.broker)throw new NodeHttpError("node_broker_unavailable","Node broker is unavailable");if(request.headers.authorization!==undefined)throw new NodeHttpError("unauthorized","user credentials are not accepted");
      const content=request.headersDistinct["content-type"]??[];if(content.length!==1||content[0]!=="application/json")throw new NodeHttpError("invalid_request","content-type must be application/json");
      const body=await jsonBody(request,16*1024);exact(body,["enrollmentId","planId","planDigest","nodeId","machineFingerprint","nodePublicKeyFingerprint"]);for(const [key,max] of [["enrollmentId",64],["planId",64],["planDigest",71],["nodeId",64],["machineFingerprint",64],["nodePublicKeyFingerprint",71]] as const)string(body[key],key,max);
      const key=idempotency(request),nodeProof=proof(request);if(this.limitJoin)await this.limitJoin(request);let result:BrokerProxyReply;try{result=await this.broker.issue(body,key,nodeProof,AbortSignal.timeout(25_000));}catch{throw new NodeHttpError("node_broker_unavailable","Node broker is unavailable");}if(result.retryAfter)response.setHeader("retry-after",result.retryAfter);response.writeHead(result.status,{"content-type":"application/json","content-length":result.body.length,"cache-control":"no-store"});response.end(result.body);return;
    }
    const createEnrollment=path.match(/^\/v1\/workspaces\/([^/]+)\/node-enrollments$/);
    if(createEnrollment){if(request.method!=="POST")throw method();const principal=await authenticate();const body=await jsonBody(request);exact(body,["name","mode","platform"],["architecture"]);const result=await this.service.createEnrollment(principal,uuid(createEnrollment[1]!,"workspaceId"),idempotency(request),{name:string(body.name,"name",128),mode:one(body.mode,"mode",["fresh","adopt"]),platform:one(body.platform,"platform",["linux","macos"]) as NodePlatform,...(body.architecture===undefined?{}:{architecture:one(body.architecture,"architecture",["amd64","arm64"]) as NodeArchitecture})});return sendJson(response,201,result);}
    const exchange=path.match(/^\/v1\/node-enrollments\/([^/]+)\/exchange$/);
    if(exchange){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["token","machineFingerprint","nodePublicKey","platform","architecture"],["kubernetesBinding"]);const binding=body.kubernetesBinding===undefined?undefined:object(body.kubernetesBinding,"kubernetesBinding");if(binding)exact(binding,["clusterId","nodeName","nodeUid","resourceVersion"]);const result=await this.service.exchangeEnrollment(uuid(exchange[1]!,"enrollmentId"),{token:string(body.token,"token",128),machineFingerprint:string(body.machineFingerprint,"machineFingerprint",64),nodePublicKey:string(body.nodePublicKey,"nodePublicKey",64),platform:one(body.platform,"platform",["linux","macos"]) as NodePlatform,architecture:one(body.architecture,"architecture",["amd64","arm64"]) as NodeArchitecture,...(binding?{kubernetesBinding:{clusterId:string(binding.clusterId,"clusterId",128),nodeName:string(binding.nodeName,"nodeName",253),nodeUid:string(binding.nodeUid,"nodeUid",128),resourceVersion:string(binding.resourceVersion,"resourceVersion",128)}}:{})});return sendJson(response,200,result);}
    const list=path.match(/^\/v1\/workspaces\/([^/]+)\/nodes$/);
    if(list){if(request.method!=="GET")throw method();return sendJson(response,200,await this.service.listNodes(await authenticate(),uuid(list[1]!,"workspaceId")));}
    if(path==="/v1/node-service/heartbeats"){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["nodeId","identityGeneration","bootId","sequence","sentAt","priorKubernetesResourceVersion","capabilityDigest","capability"]);await this.service.heartbeat({nodeId:uuid(string(body.nodeId,"nodeId",64),"nodeId"),identityGeneration:integer(body.identityGeneration,"identityGeneration",1),bootId:string(body.bootId,"bootId",128),sequence:integer(body.sequence,"sequence",0),sentAt:string(body.sentAt,"sentAt",64),priorKubernetesResourceVersion:string(body.priorKubernetesResourceVersion,"priorKubernetesResourceVersion",128),capabilityDigest:string(body.capabilityDigest,"capabilityDigest",71),capability:object(body.capability,"capability")},proof(request));response.writeHead(204,{"cache-control":"no-store"});response.end();return;}
    if(path==="/v1/node-service/retirements"){if(request.method!=="POST")throw method();const body=await jsonBody(request,256*1024);exact(body,["receipt"]);const node=await this.service.retire({receipt:object(body.receipt,"receipt")},idempotency(request),proof(request));await this.removeRetiredWorker(node);return sendJson(response,200,node);}
    if(path==="/v1/node-service/activations"){if(request.method!=="POST")throw method();const body=await jsonBody(request,256*1024);exact(body,["expectedVersion","receipt","kubernetesBinding"]);const binding=object(body.kubernetesBinding,"kubernetesBinding");exact(binding,["clusterId","nodeName","nodeUid","resourceVersion"]);const result=await this.service.activate({expectedVersion:integer(body.expectedVersion,"expectedVersion",1),receipt:object(body.receipt,"receipt"),kubernetesBinding:{clusterId:string(binding.clusterId,"clusterId",128),nodeName:string(binding.nodeName,"nodeName",253),nodeUid:string(binding.nodeUid,"nodeUid",128),resourceVersion:string(binding.resourceVersion,"resourceVersion",128)}},idempotency(request),proof(request));await this.assignWorkspace(result.node);return sendJson(response,200,result);}
    const consume=path.match(/^\/v1\/node-service\/join-credentials\/([^/]+)\/consume$/);
    if(consume){if(request.method!=="POST")throw method();const key=idempotency(request);const body=await jsonBody(request);exact(body,["nodeId","enrollmentId","planId","joinedNodeUid","joinedNodeName","resourceVersion","clusterId"]);const issuanceId=uuid(consume[1]!,"issuanceId"),input={nodeId:uuid(string(body.nodeId,"nodeId",64),"nodeId"),enrollmentId:uuid(string(body.enrollmentId,"enrollmentId",64),"enrollmentId"),planId:uuid(string(body.planId,"planId",64),"planId"),joinedNodeUid:string(body.joinedNodeUid,"joinedNodeUid",128),joinedNodeName:string(body.joinedNodeName,"joinedNodeName",253),resourceVersion:string(body.resourceVersion,"resourceVersion",128),clusterId:string(body.clusterId,"clusterId",128)},nodeProof=proof(request);const replay=await this.service.replayConsumedJoin(issuanceId,key,input,nodeProof);if(replay)return sendJson(response,200,replay);if(!this.broker?.observe)throw new NodeHttpError("node_broker_unavailable","Node broker join observation is unavailable");try{await this.broker.observe(issuanceId,{clusterId:input.clusterId,nodeName:input.joinedNodeName,nodeUid:input.joinedNodeUid,resourceVersion:input.resourceVersion},AbortSignal.timeout(25_000));}catch{throw new NodeHttpError("node_broker_unavailable","Joined worker observation failed");}const result=await this.service.consumeJoin(issuanceId,key,input,nodeProof);return sendJson(response,200,result);}
    const operations=path.match(/^\/v1\/nodes\/([^/]+)\/operations$/);
    if(operations){if(request.method!=="POST")throw method();const body=await jsonBody(request);exact(body,["type","expectedVersion","parameters"]);const result=await this.service.createOperation(await authenticate(),uuid(operations[1]!,"nodeId"),idempotency(request),{type:one(body.type,"type",["pause","resume","quarantine","label","cordon","uncordon","rotate_identity","repair","update","drain","remove"]) as NodeOperationType,expectedVersion:integer(body.expectedVersion,"expectedVersion",1),parameters:object(body.parameters,"parameters")});if(["pause","resume","quarantine"].includes(result.type))await this.reconcilePlacement(result.nodeId).catch(()=>{});return sendJson(response,202,result);}
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
