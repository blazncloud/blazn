import type { AgentRunControllerStore } from "./agent-run-controller-store.js";

export class AgentRunControllerValidationError extends Error {}

export class AgentRunControllerService {
  constructor(private readonly store:AgentRunControllerStore){}
  enqueue(runId:string,workspaceId:string,agentVersionId:string,harnessProfileId:string){for(const [label,value] of [["Run ID",runId],["Workspace ID",workspaceId],["AgentVersion ID",agentVersionId],["HarnessProfile ID",harnessProfileId]] as const)uuid(value,label);return this.store.enqueue(runId,workspaceId,agentVersionId,harnessProfileId);}
  claim(workerId:string,leaseSeconds:number){worker(workerId);lease(leaseSeconds);return this.store.claim(workerId,leaseSeconds);}
  renew(runId:string,workerId:string,leaseToken:string,leaseSeconds:number){uuid(runId,"Run ID");worker(workerId);uuid(leaseToken,"lease token");lease(leaseSeconds);return this.store.renew(runId,workerId,leaseToken,leaseSeconds);}
  bindSandbox(runId:string,workerId:string,leaseToken:string,expectedRunVersion:number,nodeId:string,sandboxId:string){uuid(runId,"Run ID");worker(workerId);uuid(leaseToken,"lease token");positive(expectedRunVersion,"expected Run version");uuid(nodeId,"Node ID");uuid(sandboxId,"Sandbox ID");return this.store.bindSandbox(runId,workerId,leaseToken,expectedRunVersion,nodeId,sandboxId);}
  retry(runId:string,workerId:string,leaseToken:string,delaySeconds:number,errorCode:string){uuid(runId,"Run ID");worker(workerId);uuid(leaseToken,"lease token");if(!Number.isSafeInteger(delaySeconds)||delaySeconds<0||delaySeconds>3600)invalid("retry delay is invalid");if(!/^[a-z][a-z0-9_]{0,62}$/.test(errorCode))invalid("retry error code is invalid");return this.store.retry(runId,workerId,leaseToken,delaySeconds,errorCode);}
  finalize(runId:string,workerId:string,leaseToken:string,expectedRunVersion:number,outcome:"succeeded"|"failed",errorCode:string|undefined,artifactIds:string[],steps:number,warnings:string[]){uuid(runId,"Run ID");worker(workerId);uuid(leaseToken,"lease token");positive(expectedRunVersion,"expected Run version");if(!["succeeded","failed"].includes(outcome))invalid("outcome is invalid");if((outcome==="failed")!==!!errorCode||errorCode&&!/^[a-z][a-z0-9_]{0,62}$/.test(errorCode))invalid("error code is inconsistent with outcome");if(!Array.isArray(artifactIds)||artifactIds.length>100||new Set(artifactIds).size!==artifactIds.length)invalid("Artifact IDs are invalid");for(const id of artifactIds)uuid(id,"Artifact ID");if(!Number.isSafeInteger(steps)||steps<0)invalid("step count is invalid");if(!Array.isArray(warnings)||warnings.length>100||warnings.some(value=>typeof value!=="string"||value.length<1||value.length>512||value.includes("\0")))invalid("warnings are invalid");return this.store.finalize(runId,workerId,leaseToken,expectedRunVersion,outcome,errorCode,[...artifactIds],steps,[...warnings]);}
}
function uuid(value:string,label:string){if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value))invalid(`${label} is invalid`);}
function worker(value:string){if(!(/^[a-z0-9]$/.test(value)||/^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$/.test(value)))invalid("controller worker ID is invalid");}
function lease(value:number){if(!Number.isSafeInteger(value)||value<10||value>300)invalid("controller lease duration is invalid");}
function positive(value:number,label:string){if(!Number.isSafeInteger(value)||value<1)invalid(`${label} is invalid`);}
function invalid(message:string):never{throw new AgentRunControllerValidationError(message);}
