import { randomUUID } from "node:crypto";
import { canonicalJson, enrollmentToken, publicKeyFingerprint, renderedDigest, requestDigest, sha256Hex, verifyNodePlanSignature, verifyNodeProof } from "./node-crypto.js";
import type { NodePlanFactory } from "./node-plan.js";
import type { NodeIdempotencyReceipt, NodeStore, NodeTransaction } from "./node-store.js";
import { nodeRoleAllows, NodeHttpError, type ExchangeNodeEnrollmentResponse, type KubernetesBinding, type NodeArchitecture, type NodeEvent, type NodeOperationType, type NodeOperationView, type NodePlanSigningKey, type NodePlatform, type NodePrincipal, type NodeView } from "./node-types.js";

export interface CreateEnrollmentInput { name: string; mode: "fresh" | "adopt"; platform: NodePlatform; architecture?: NodeArchitecture }
export interface ExchangeEnrollmentInput { token: string; machineFingerprint: string; nodePublicKey: string; platform: NodePlatform; architecture: NodeArchitecture; kubernetesBinding?: KubernetesBinding }
export interface HeartbeatInput { nodeId: string; identityGeneration: number; bootId: string; sequence: number; sentAt: string; capabilityDigest: string; capability: Record<string, unknown> }

export class NodeService {
  constructor(private readonly store: NodeStore, private readonly enrollmentKey: () => Promise<Buffer>, private readonly planFactory: NodePlanFactory, private readonly now: () => Date = () => new Date()) {}

  async createEnrollment(principal: NodePrincipal, workspaceId: string, idempotencyKey: string, input: CreateEnrollmentInput) {
    validIdempotency(idempotencyKey); validName(input.name); validPlatform(input.platform); if (input.architecture) validArchitecture(input.architecture);
    if (input.mode !== "fresh" && input.mode !== "adopt") invalid("mode is invalid");
    if (input.mode === "fresh" && (input.platform !== "linux" || (input.architecture && input.architecture !== "amd64"))) invalid("fresh POC enrollment requires linux/amd64");
    const digest = requestDigest(input); const key = await this.enrollmentKey();
    return this.store.transaction(async (tx) => {
      await tx.lockIdempotency(principal.userId,"node.enrollment.create",idempotencyKey);
      await authorize(tx,principal,workspaceId,true,true);
      const receipt=await tx.getIdempotency(principal.userId,"node.enrollment.create",idempotencyKey);
      if(receipt){ verifyReceipt(receipt,workspaceId,`workspace:${workspaceId}`,digest); const stored=receipt.responseBody as {id:string;tokenKeyId:"node-enrollment/v1";planSigningKey:NodePlanSigningKey;expiresAt:string};const enrollment=await tx.enrollmentById(stored.id);if(!enrollment||!sameSigningKey(enrollment.planSigningKey,stored.planSigningKey))throw new Error("enrollment signing trust does not match its idempotency receipt");return {...stored,token:enrollmentToken(key,workspaceId,stored.id,principal.userId,idempotencyKey),replayed:true}; }
      const planSigningKey=validSigningKey(await this.planFactory.signingKey());
      const id=randomUUID(); const expiresAt=new Date(this.now().getTime()+15*60_000); const token=enrollmentToken(key,workspaceId,id,principal.userId,idempotencyKey);
      await tx.insertEnrollment({id,workspaceId,name:input.name.trim(),mode:input.mode,platform:input.platform,architecture:input.architecture??null,tokenHash:sha256Hex(token),idempotencyKey,createdBy:principal.userId,planSigningKey,expiresAt});
      const stored={id,tokenKeyId:"node-enrollment/v1" as const,planSigningKey,expiresAt:expiresAt.toISOString()};
      await tx.putIdempotency(principal.userId,"node.enrollment.create",idempotencyKey,{workspaceId,targetKey:`workspace:${workspaceId}`,requestDigest:digest,responseStatus:201,responseBody:stored});
      return {...stored,token,replayed:false};
    });
  }

  async exchangeEnrollment(enrollmentId:string,input:ExchangeEnrollmentInput):Promise<ExchangeNodeEnrollmentResponse>{
    validUuid(enrollmentId,"enrollmentId"); validPlatform(input.platform); validArchitecture(input.architecture);
    if(!/^[A-Za-z0-9_-]{43,128}$/.test(input.token)) invalid("token is invalid");
    if(!/^[0-9a-f]{64}$/.test(input.machineFingerprint)) invalid("machineFingerprint is invalid");
    let fingerprint:string; try{fingerprint=publicKeyFingerprint(input.nodePublicKey);}catch{invalid("nodePublicKey is invalid");}
    if(input.kubernetesBinding) validateBinding(input.kubernetesBinding);
    return this.store.transaction(async(tx)=>{
      const enrollment=await tx.enrollmentById(enrollmentId,true); if(!enrollment) throw new NodeHttpError("enrollment_not_found","enrollment was not found");
      if(enrollment.tokenHash!==sha256Hex(input.token)) throw new NodeHttpError("enrollment_invalid","enrollment token is invalid");
      if(enrollment.expiresAt.getTime()<=this.now().getTime()) throw new NodeHttpError("enrollment_expired","enrollment has expired");
      if(enrollment.expectedPlatform!==input.platform || (enrollment.expectedArchitecture!==null&&enrollment.expectedArchitecture!==input.architecture)) throw new NodeHttpError("enrollment_invalid","enrollment target does not match this machine");
      if(enrollment.mode==="adopt"&&!input.kubernetesBinding) invalid("adoption requires kubernetesBinding");
      if(enrollment.mode==="fresh"&&input.kubernetesBinding) invalid("fresh enrollment cannot pre-bind Kubernetes");
      if(enrollment.status==="exchanged"){
        if(enrollment.machineBinding!==input.machineFingerprint||enrollment.nodePublicKey!==input.nodePublicKey||enrollment.nodePublicKeyFingerprint!==fingerprint) throw new NodeHttpError("enrollment_consumed","enrollment is bound to another machine identity");
        const replay=await tx.exchangeByEnrollment(enrollment.id); if(!replay) throw new Error("exchanged enrollment has no plan and identity"); return replay;
      }
      if(enrollment.status!=="pending") throw new NodeHttpError("enrollment_consumed","enrollment is no longer exchangeable");
      const configuredSigningKey=validSigningKey(await this.planFactory.signingKey());if(!sameSigningKey(configuredSigningKey,enrollment.planSigningKey))throw new Error("configured Node plan signer does not match enrollment-pinned trust");
      const issuedAt=this.now(),expiresAt=new Date(issuedAt.getTime()+15*60_000),nodeId=randomUUID(),planId=randomUUID();
      const plan=await this.planFactory.create({planId,nodeId,enrollment,architecture:input.architecture,machineFingerprint:input.machineFingerprint,nodePublicKeyFingerprint:fingerprint,issuedAt,expiresAt});
      const digest=requiredRenderedDigest(plan.digest,"plan digest"); const signature=requiredSignature(plan.signature); const signingKeyId=requiredText(plan.signingKeyId,"signingKeyId",128);if(signingKeyId!==enrollment.planSigningKey.keyId||!verifyNodePlanSignature(enrollment.planSigningKey.publicKey,digest,signature))throw new Error("Node install plan signature does not match enrollment-pinned trust");
      const identity=await tx.createExchangedNode({nodeId,identityId:randomUUID(),enrollment,architecture:input.architecture,machineFingerprint:input.machineFingerprint,publicKey:input.nodePublicKey,publicKeyFingerprint:fingerprint,...(input.kubernetesBinding?{kubernetesBinding:input.kubernetesBinding}:{}),planId,plan,planDigest:digest.slice(7),signingKeyId,signature,issuedAt,expiresAt});
      if(identity.publicKeyFingerprint!==`sha256:${fingerprint}`||identity.issuedAt!==issuedAt.toISOString())throw new Error("created Node identity does not match enrollment exchange");
      return {plan,identity};
    });
  }

  async listNodes(principal:NodePrincipal,workspaceId:string):Promise<{items:NodeView[]}>{
    return this.store.transaction(async tx=>{await authorize(tx,principal,workspaceId,false,false);return{items:await tx.listNodes(workspaceId)}});
  }
  async getNode(principal:NodePrincipal,nodeId:string):Promise<NodeView>{
    return this.store.transaction(async tx=>{const node=await requiredNode(tx,nodeId,false);await authorize(tx,principal,node.workspaceId,false,false);return node;});
  }
  async eventBatch(principal:NodePrincipal,nodeId:string,afterId=""):Promise<NodeEvent[]>{
    return this.store.transaction(async tx=>{const node=await requiredNode(tx,nodeId,false);await authorize(tx,principal,node.workspaceId,false,false);return tx.listEvents(nodeId,afterId);}).catch(mapStoreError);
  }

  async createOperation(principal:NodePrincipal,nodeId:string,idempotencyKey:string,input:{type:NodeOperationType;expectedVersion:number;parameters:Record<string,unknown>}):Promise<NodeOperationView>{
    validIdempotency(idempotencyKey); validOperation(input); const digest=requestDigest(input);
    return this.store.transaction(async tx=>{
      await tx.lockIdempotency(principal.userId,`node.operation.${input.type}`,idempotencyKey);
      const node=await requiredNode(tx,nodeId,true); await authorize(tx,principal,node.workspaceId,true,true);
      const receipt=await tx.getIdempotency(principal.userId,`node.operation.${input.type}`,idempotencyKey);
      if(receipt){verifyReceipt(receipt,node.workspaceId,`node:${nodeId}`,digest);return receipt.responseBody as NodeOperationView;}
      if(node.version!==input.expectedVersion) throw new NodeHttpError("version_conflict","node version changed");
      validateOperationState(node,input.type,input.parameters);
      const operation=await tx.insertOperation({id:randomUUID(),workspaceId:node.workspaceId,nodeId,type:input.type,expectedVersion:input.expectedVersion,requestedBy:principal.userId,idempotencyKey,requestDigest:digest,parameters:input.parameters});
      await tx.putIdempotency(principal.userId,`node.operation.${input.type}`,idempotencyKey,{workspaceId:node.workspaceId,targetKey:`node:${nodeId}`,requestDigest:digest,responseStatus:202,responseBody:operation});
      return operation;
    }).catch(mapStoreError);
  }

  async heartbeat(input:HeartbeatInput,proof:string):Promise<void>{
    validateHeartbeat(input); const sentAt=new Date(input.sentAt); if(Math.abs(this.now().getTime()-sentAt.getTime())>5*60_000) throw new NodeHttpError("heartbeat_skew","heartbeat timestamp exceeds allowed clock skew");
    const digest=renderedDigest("blazn-node-capability-v1",input.capability); if(digest!==input.capabilityDigest) throw new NodeHttpError("capability_digest_invalid","capability digest does not match payload");
    rejectSecrets(input.capability); validateCapability(input.capability);
    await this.store.transaction(async tx=>{
      const identity=await tx.activeIdentity(input.nodeId,true); if(!identity||identity.trustState==="revoked"||identity.lifecycleState==="removed") throw new NodeHttpError("identity_rejected","node identity is not active");
      if(identity.generation!==input.identityGeneration||!verifyNodeProof(identity.publicKey,"blazn-node-heartbeat-v1",input,proof)) throw new NodeHttpError("identity_rejected","node proof could not be verified");
      const node=await requiredNode(tx,input.nodeId,false);const worker=input.capability.worker as Record<string,unknown>;const binding=worker.kubernetesBinding as KubernetesBinding;
      if(!node.kubernetesBinding||canonicalJson(node.kubernetesBinding)!==canonicalJson(binding))throw new NodeHttpError("state_conflict","heartbeat Kubernetes binding does not match the enrolled node");
      const prior=await tx.heartbeatState(input.nodeId);
      if(prior&&prior.identityGeneration===input.identityGeneration){
        if(prior.bootId===input.bootId&&input.sequence<=prior.sequence) throw new NodeHttpError("heartbeat_replay","heartbeat sequence was already observed");
        if(prior.bootId!==input.bootId&&(input.sequence!==0||sentAt.getTime()<=prior.sentAt.getTime()||await tx.bootObserved(input.nodeId,input.identityGeneration,input.bootId))) throw new NodeHttpError("heartbeat_replay","new boot epoch is invalid or already observed");
      }
      const version=input.capability.version;
      if(typeof version!=="number"||!Number.isSafeInteger(version)||version<1) invalid("capability version is invalid");
      if(node.capabilityVersion!==null&&(version<node.capabilityVersion||(version===node.capabilityVersion&&prior?.capabilityDigest!==digest.slice(7))))throw new NodeHttpError("state_conflict","capability version cannot roll back or change content");
      if(!prior||prior.bootId!==input.bootId)await tx.observeBoot(input.nodeId,node.workspaceId,input.identityGeneration,input.bootId,sentAt);
      await tx.recordHeartbeat({nodeId:input.nodeId,identityGeneration:input.identityGeneration,bootId:input.bootId,sequence:input.sequence,sentAt,capabilityDigest:digest.slice(7),capability:input.capability,health:capabilityHealth(input.capability)});
    }).catch(mapStoreError);
  }

  async consumeJoin(issuanceId:string,idempotencyKey:string,input:{nodeId:string;enrollmentId:string;planId:string;joinedNodeUid:string;joinedNodeName:string;resourceVersion:string;clusterId:string},proof:string):Promise<NodeView>{
    validIdempotency(idempotencyKey);
    validUuid(issuanceId,"issuanceId"); for(const field of ["nodeId","enrollmentId","planId"] as const) validUuid(input[field],field); validateBinding({clusterId:input.clusterId,nodeName:input.joinedNodeName,nodeUid:input.joinedNodeUid,resourceVersion:input.resourceVersion});
    return this.store.transaction(async tx=>{const identity=await tx.activeIdentity(input.nodeId,true);if(!identity||identity.trustState==="revoked"||identity.lifecycleState==="removed"||!verifyNodeProof(identity.publicKey,"blazn-node-join-v1",input,proof))throw new NodeHttpError("identity_rejected","node identity is not active or proof could not be verified");
      return tx.consumeJoin({issuanceId,nodeId:input.nodeId,enrollmentId:input.enrollmentId,planId:input.planId,clusterId:input.clusterId,nodeName:input.joinedNodeName,nodeUid:input.joinedNodeUid,resourceVersion:input.resourceVersion,idempotencyKey,requestDigest:requestDigest({issuanceId,...input})});}).catch(mapStoreError);
  }
}

async function authorize(tx:NodeTransaction,principal:NodePrincipal,workspaceId:string,mutation:boolean,lock:boolean){const authority=await tx.authority(workspaceId,principal.userId,lock);if(!authority)throw new NodeHttpError("membership_required","active workspace membership is required");if(authority.workspaceStatus!=="active")throw new NodeHttpError("state_conflict","workspace is not active");if(!nodeRoleAllows(authority.role,mutation))throw new NodeHttpError("permission_denied","node action is not permitted");return authority;}
async function requiredNode(tx:NodeTransaction,nodeId:string,lock:boolean){validUuid(nodeId,"nodeId");const node=await tx.nodeById(nodeId,lock);if(!node)throw new NodeHttpError("node_not_found","node was not found");return node;}
function verifyReceipt(r:NodeIdempotencyReceipt,w:string,t:string,d:string){if(r.workspaceId!==w||r.targetKey!==t||r.requestDigest!==d)throw new NodeHttpError("idempotency_conflict","idempotency key is bound to another request");}
function validName(v:string){if(typeof v!=="string"||!v.trim()||v.length>128||!/^[a-z0-9](?:[a-z0-9.-]{0,126}[a-z0-9])?$/.test(v.trim()))invalid("name is invalid");}
function validPlatform(v:unknown):asserts v is NodePlatform{if(v!=="linux"&&v!=="macos")invalid("platform is invalid");}
function validArchitecture(v:unknown):asserts v is NodeArchitecture{if(v!=="amd64"&&v!=="arm64")invalid("architecture is invalid");}
function validUuid(v:string,f:string){if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(v))invalid(`${f} must be a UUID`);}
function validIdempotency(v:string){if(typeof v!=="string"||v.length<8||v.length>128)invalid("Idempotency-Key is invalid");}
function validateBinding(v:KubernetesBinding){for(const [k,max] of [["clusterId",128],["nodeName",253],["nodeUid",128],["resourceVersion",128]] as const){const s=v[k];if(typeof s!=="string"||!s||s.length>max)invalid(`${k} is invalid`);}}
function requiredRenderedDigest(v:unknown,name:string):string{if(typeof v!=="string"||!/^sha256:[0-9a-f]{64}$/.test(v))throw new Error(`${name} is invalid`);return v;}
function requiredSignature(v:unknown):string{if(typeof v!=="string"||!/^[A-Za-z0-9_-]{86}$/.test(v))throw new Error("plan signature is invalid");return v;}
function requiredText(v:unknown,name:string,max:number):string{if(typeof v!=="string"||!v||v.length>max)throw new Error(`${name} is invalid`);return v;}
function validSigningKey(v:NodePlanSigningKey):NodePlanSigningKey{requiredText(v.keyId,"plan signing key ID",128);if(!/^[A-Za-z0-9_-]{43}$/.test(v.publicKey))throw new Error("plan signing public key is invalid");if(v.fingerprint!==`sha256:${publicKeyFingerprint(v.publicKey)}`)throw new Error("plan signing key fingerprint is inconsistent");return v;}
function sameSigningKey(a:NodePlanSigningKey,b:NodePlanSigningKey):boolean{return a.keyId===b.keyId&&a.publicKey===b.publicKey&&a.fingerprint===b.fingerprint;}
function validateHeartbeat(v:HeartbeatInput){validUuid(v.nodeId,"nodeId");if(!Number.isSafeInteger(v.identityGeneration)||v.identityGeneration<1)invalid("identityGeneration is invalid");if(typeof v.bootId!=="string"||!v.bootId||v.bootId.length>128)invalid("bootId is invalid");if(!Number.isSafeInteger(v.sequence)||v.sequence<0)invalid("sequence is invalid");if(!Number.isFinite(Date.parse(v.sentAt)))invalid("sentAt is invalid");requiredRenderedDigest(v.capabilityDigest,"capabilityDigest");if(!v.capability||typeof v.capability!=="object"||Array.isArray(v.capability))invalid("capability is invalid");}
function rejectSecrets(value:unknown):void{if(Array.isArray(value)){for(const item of value)rejectSecrets(item);return;}if(value&&typeof value==="object"){for(const [key,child] of Object.entries(value)){const n=key.toLowerCase().replace(/[^a-z0-9]/g,"");if(["token","invitetoken","accesstoken","refreshtoken","authorization","password","secret","credential"].includes(n))invalid("capability contains a forbidden secret field");rejectSecrets(child);}}}
function validateCapability(c:Record<string,unknown>):void{
  exact(c,["version","host","worker","sandboxBackends","runtimeClasses","localModels"]);positiveInt(c.version,"capability.version");const host=record(c.host,"host"),worker=record(c.worker,"worker");
  exact(host,["platform","architecture","cpuMillis","memoryBytes","diskBytes","accelerators","health"]);validPlatform(host.platform);validArchitecture(host.architecture);for(const k of ["cpuMillis","memoryBytes","diskBytes"])positiveInt(host[k],`host.${k}`);validateHealth(record(host.health,"host.health"));array(host.accelerators,"host.accelerators",16).forEach((item)=>{const a=record(item,"accelerator");exact(a,["kind","count"]);boundedText(a.kind,"accelerator.kind",128);positiveInt(a.count,"accelerator.count");});
  exact(worker,["platform","architecture","allocatableCpuMillis","allocatableMemoryBytes","allocatableDiskBytes","labels","limits","health","kubernetesBinding"]);if(worker.platform!=="linux")invalid("worker.platform must be linux");validArchitecture(worker.architecture);for(const k of ["allocatableCpuMillis","allocatableMemoryBytes","allocatableDiskBytes"])positiveInt(worker[k],`worker.${k}`);const labels=record(worker.labels,"worker.labels");if(Object.keys(labels).length>64)invalid("worker labels are invalid");for(const [k,v] of Object.entries(labels)){if(!/^blazn\.dev\/[a-z0-9][a-z0-9._-]{0,62}$/.test(k)||typeof v!=="string"||v.length>128)invalid("worker labels are invalid");}const limits=record(worker.limits,"worker.limits");exact(limits,["maxConcurrentSandboxes","maxConcurrentAgents"]);for(const k of ["maxConcurrentSandboxes","maxConcurrentAgents"]){if(!Number.isSafeInteger(limits[k])||Number(limits[k])<0||Number(limits[k])>1024)invalid(`worker.limits.${k} is invalid`);}validateHealth(record(worker.health,"worker.health"));validateBinding(record(worker.kubernetesBinding,"worker.kubernetesBinding") as unknown as KubernetesBinding);
  for(const k of ["sandboxBackends","runtimeClasses"]){const values=array(c[k],k,128);if(new Set(values).size!==values.length||values.some(v=>typeof v!=="string"||v.length>128))invalid(`${k} is invalid`);}const models=array(c.localModels,"localModels",32);for(const item of models){const m=record(item,"localModel");exact(m,["routeId","displayName","model","protocol","endpointClass","capabilities","dataBoundary","healthy","maxConcurrency","maxContextTokens","maxOutputTokens"]);validUuid(boundedText(m.routeId,"routeId",64),"routeId");boundedText(m.displayName,"displayName",128);boundedText(m.model,"model",160);if(!["openai-chat","openai-responses"].includes(String(m.protocol))||!["loopback","authenticated_node_tunnel"].includes(String(m.endpointClass))||m.dataBoundary!=="local"||typeof m.healthy!=="boolean")invalid("local model routing metadata is invalid");const caps=array(m.capabilities,"capabilities",4);if(caps.length<1||new Set(caps).size!==caps.length||caps.some(v=>!["text","tools","structured_output","streaming"].includes(String(v))))invalid("local model capabilities are invalid");for(const k of ["maxConcurrency","maxContextTokens","maxOutputTokens"])positiveInt(m[k],`localModel.${k}`);}
}
function validateHealth(h:Record<string,unknown>){exact(h,["status","reasonCodes"]);if(!["healthy","degraded","unavailable"].includes(String(h.status)))invalid("health status is invalid");const reasons=array(h.reasonCodes,"reasonCodes",16);if(new Set(reasons).size!==reasons.length||reasons.some(v=>typeof v!=="string"||!/^[a-z0-9_]{1,64}$/.test(v)))invalid("health reasonCodes are invalid");}
function record(v:unknown,name:string):Record<string,unknown>{if(!v||typeof v!=="object"||Array.isArray(v))invalid(`${name} must be an object`);return v as Record<string,unknown>;}
function array(v:unknown,name:string,max:number):unknown[]{if(!Array.isArray(v)||v.length>max)invalid(`${name} must be an array with at most ${max} items`);return v;}
function positiveInt(v:unknown,name:string){if(typeof v!=="number"||!Number.isSafeInteger(v)||v<1)invalid(`${name} is invalid`);}
function boundedText(v:unknown,name:string,max:number):string{if(typeof v!=="string"||!v||v.length>max)invalid(`${name} is invalid`);return v;}
function capabilityHealth(c:Record<string,unknown>):unknown{const host=c.host,worker=c.worker;return{host:host&&typeof host==="object"?(host as Record<string,unknown>).health:null,worker:worker&&typeof worker==="object"?(worker as Record<string,unknown>).health:null};}
function validOperation(i:{type:NodeOperationType;expectedVersion:number;parameters:Record<string,unknown>}){if(!["pause","resume","label","cordon","uncordon","rotate_identity","repair","update","drain","remove"].includes(i.type))invalid("operation type is invalid");if(!Number.isSafeInteger(i.expectedVersion)||i.expectedVersion<1)invalid("expectedVersion is invalid");if(!i.parameters||typeof i.parameters!=="object"||Array.isArray(i.parameters))invalid("parameters must be an object");rejectSecrets(i.parameters);}
function exact(p:Record<string,unknown>,keys:string[]){if(Object.keys(p).length!==keys.length||keys.some(k=>!(k in p)))invalid("operation parameters are invalid");}
function validateOperationState(node:NodeView,type:NodeOperationType,p:Record<string,unknown>){if(node.lifecycleState==="removed")throw new NodeHttpError("state_conflict","removed node cannot be operated");if(type==="pause"&&node.lifecycleState!=="active")throw new NodeHttpError("state_conflict","only an active node can be paused");if(type==="resume"&&node.lifecycleState!=="paused")throw new NodeHttpError("state_conflict","only a paused node can be resumed");
  if(["pause","resume","rotate_identity","repair"].includes(type))exact(p,[]);
  if(type==="label"){exact(p,["key","value"]);if(typeof p.key!=="string"||!/^blazn\.dev\/[a-z0-9][a-z0-9._-]{0,62}$/.test(p.key)||typeof p.value!=="string"||p.value.length>128)invalid("label parameters are invalid");}
  if(type==="update"){exact(p,["targetVersion"]);if(typeof p.targetVersion!=="string"||!/^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$/.test(p.targetVersion)||p.targetVersion.length>128)invalid("update parameters are invalid");}
  if(["cordon","uncordon","drain","remove"].includes(type)){if(!node.kubernetesBinding)throw new NodeHttpError("state_conflict","operation requires a Kubernetes binding");const base=["clusterId","expectedNodeUid","expectedResourceVersion"];const extra=type==="drain"?["workspaceId","deadlineSeconds"]:type==="remove"?["confirm","preserveHostData"]:[];exact(p,[...base,...extra]);if(p.clusterId!==node.kubernetesBinding.clusterId||p.expectedNodeUid!==node.kubernetesBinding.nodeUid||p.expectedResourceVersion!==node.kubernetesBinding.resourceVersion)throw new NodeHttpError("version_conflict","Kubernetes binding changed");if(type==="drain"&&(p.workspaceId!==node.workspaceId||!Number.isSafeInteger(p.deadlineSeconds)||Number(p.deadlineSeconds)<60||Number(p.deadlineSeconds)>3600))invalid("drain parameters are invalid");if(type==="remove"&&(p.confirm!==true||p.preserveHostData!==true))invalid("remove confirmation is required");}
}
function mapStoreError(error:unknown):never{if(error instanceof NodeHttpError)throw error;if(error&&typeof error==="object"&&"nodeCode" in error)throw new NodeHttpError(error.nodeCode as never,error instanceof Error?error.message:"node request failed");if(error&&typeof error==="object"&&"code" in error&&error.code==="23505")throw new NodeHttpError("state_conflict","conflicting node state");throw error;}
function invalid(message:string):never{throw new NodeHttpError("invalid_request",message);}
