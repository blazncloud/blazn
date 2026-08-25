import { verifyDevelopmentFinalization } from "./development-contract.js";
import type { DevelopmentControllerStore,DevelopmentControllerWorkItem } from "./development-controller-store.js";

export class DevelopmentControllerValidationError extends Error {}

export class DevelopmentControllerService {
  constructor(private readonly store:DevelopmentControllerStore){}
  async claim(workerId:string,leaseSeconds:number){validateWorker(workerId);validateLease(leaseSeconds);return this.store.claim(workerId,leaseSeconds);}
  async renew(buildId:string,workerId:string,leaseToken:string,leaseSeconds:number){validateUUID(buildId,"Build ID");validateWorker(workerId);validateUUID(leaseToken,"lease token");validateLease(leaseSeconds);return this.store.renew(buildId,workerId,leaseToken,leaseSeconds);}
  async finalize(buildId:string,workerId:string,leaseToken:string,expectedVersion:number,execution:{nodeId:string;sandboxId:string},document:Record<string,unknown>):Promise<boolean>{
    validateUUID(buildId,"Build ID");validateWorker(workerId);validateUUID(leaseToken,"lease token");
    validateUUID(execution.nodeId,"execution Node ID");validateUUID(execution.sandboxId,"execution Sandbox ID");
    if(!Number.isSafeInteger(expectedVersion)||expectedVersion<1)invalid("expected Build version is invalid");
    if(!plainObject(document))invalid("terminal Build document is invalid");
    const claimed=await this.store.resolve(buildId,workerId,leaseToken);if(!claimed)return false;
    validateTerminalBinding(claimed,expectedVersion,document);
    const forbidden=redactionViolation(document);if(forbidden)invalid(`terminal Build document contains forbidden ${forbidden}`);
    const errors=verifyDevelopmentFinalization(claimed.projectSnapshot,document);if(errors.length)invalid(errors.join("; "));
    return this.store.finalize(buildId,workerId,leaseToken,expectedVersion,execution,document);
  }
}

function validateTerminalBinding(claim:DevelopmentControllerWorkItem,expectedVersion:number,document:Record<string,unknown>){
  const status=document.status,publication=object(document.publication),reasons=publication?publication.refusalReasons:undefined;
  const source=object(document.source);
  if(expectedVersion!==claim.buildVersion||document.version!==expectedVersion+1)invalid("terminal Build version is not bound to the active claim");
  if(document.schemaVersion!=="blazn.dev/build/v1alpha1"||document.id!==claim.buildId||document.workspaceId!==claim.workspaceId||document.projectId!==claim.projectId||document.runId!==claim.runId)invalid("terminal Build identity is not bound to the active claim");
  if(!source||source.repository!==claim.source.repository||source.commit!==claim.source.commit||document.projectManifestDigest!==claim.projectManifestDigest||document.planDigest!==claim.planDigest)invalid("terminal Build inputs are not bound to the active claim");
  if(!["succeeded","failed","cancelled"].includes(String(status)))invalid("terminal Build status is invalid");
  if(!publication||typeof publication.eligible!=="boolean"||!Array.isArray(reasons)||reasons.some(value=>typeof value!=="string"))invalid("terminal Build publication decision is invalid");
  if(publication.published!==null)invalid("Build finalization cannot publish a template");
  if(publication.eligible?(status!=="succeeded"||reasons.length!==0):reasons.length===0)invalid("terminal Build publication eligibility is inconsistent");
  if(status==="failed"&&!/^[a-z][a-z0-9_]{0,62}$/.test(String(document.errorCode??"")))invalid("failed Build error code is invalid");
  if(status!=="failed"&&"errorCode" in document)invalid("non-failed Build cannot carry an error code");
  if(!/^sha256:[0-9a-f]{64}$/.test(String(document.receiptDigest??"")))invalid("terminal Build receipt digest is invalid");
}

const forbiddenKeys=new Set(["authorization","credential","credentials","password","secret","secrets","token","accessToken","refreshToken","apiKey","objectKey","signedUrl","buildkitEndpoint","buildkitClientCertificate","registryCredential"]);
function redactionViolation(value:unknown,path="document"):string|undefined{
  if(typeof value==="string"){
    const lower=value.toLowerCase();
    if(/(?:^|[?&])(?:x-amz-signature|x-goog-signature|signature|sig|token|access_token|api_key|credential)=/i.test(value))return `${path} signed URL`;
    if(/^(?:tcp|https?):\/\/.+buildkit/i.test(value)||lower.startsWith("unix://")||lower.startsWith("npipe://"))return `${path} BuildKit address`;
    if(/\b(?:bearer|basic)\s+[a-z0-9+/_=-]+/i.test(value))return `${path} credential`;
    return undefined;
  }
  if(Array.isArray(value)){for(let index=0;index<value.length;index++){const found=redactionViolation(value[index],`${path}[${index}]`);if(found)return found;}return undefined;}
  if(plainObject(value)){for(const [key,child] of Object.entries(value)){const canonical=key.replaceAll("_","").replaceAll("-","").toLowerCase();if([...forbiddenKeys].some(item=>item.replaceAll("_","").replaceAll("-","").toLowerCase()===canonical))return `${path}.${key}`;const found=redactionViolation(child,`${path}.${key}`);if(found)return found;}}
  return undefined;
}
function plainObject(value:unknown):value is Record<string,unknown>{return !!value&&typeof value==="object"&&!Array.isArray(value);}
function object(value:unknown){return plainObject(value)?value:undefined;}
function validateWorker(value:string){if(!/^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$/.test(value)&&!/^[a-z0-9]$/.test(value))invalid("controller worker ID is invalid");}
function validateLease(value:number){if(!Number.isSafeInteger(value)||value<10||value>300)invalid("controller lease duration is invalid");}
function validateUUID(value:string,label:string){if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value))invalid(`${label} is invalid`);}
function invalid(message:string):never{throw new DevelopmentControllerValidationError(message);}
