import { randomUUID } from "node:crypto";
import { requestDigest } from "./workspace-crypto.js";
import type { IdempotencyReceipt } from "./workspace-store.js";
import { roleAllows } from "./workspace-types.js";
import { RunInputArtifactError, type RunStore, type RunTransaction } from "./run-store.js";
import { RunHttpError, type ArtifactStatus, type ProofClass, type RunPrincipal, type RunStatus } from "./run-types.js";

export interface CreateRunInput { kind:string;proofClass:ProofClass;planDigest:string;inputArtifactIds:string[];outputNames:string[] }

export class RunService {
  constructor(private readonly store:RunStore){}
  async createRun(principal:RunPrincipal,workspaceId:string,projectId:string,key:string,input:CreateRunInput){
    const normalized=validateCreate(input);const digest=requestDigest({workspaceId,projectId,...normalized});
    return this.idempotent(principal,workspaceId,projectId,"run.create",key,`project:${projectId}`,digest,"operate",async tx=>{
      try{const run=await tx.createRun({id:randomUUID(),workspaceId,projectId,...normalized,requestedBy:principal.userId});await tx.insertAudit(randomUUID(),workspaceId,principal.userId,"run.created",{projectId,runId:run.id,kind:run.kind,proofClass:run.proofClass,planDigest:run.planDigest});return{run};}
      catch(error){if(error instanceof RunInputArtifactError)throw new RunHttpError("artifact_not_found","one or more input Artifacts were unavailable");throw error;}
    },202);
  }
  async listRuns(principal:RunPrincipal,workspaceId:string,projectId:string,status:RunStatus|"all"="all",cursor=""){validRunStatus(status);validCursor(cursor);return this.store.transaction(async tx=>{await this.authorize(tx,principal,workspaceId,projectId,"read",true);return tx.listRuns(workspaceId,projectId,status,cursor);});}
  async getRun(principal:RunPrincipal,workspaceId:string,projectId:string,runId:string){return this.store.transaction(async tx=>{await this.authorize(tx,principal,workspaceId,projectId,"read",true);const run=await tx.getRun(workspaceId,projectId,runId);if(!run)throw new RunHttpError("run_not_found","Run was not found");return{run};});}
  async cancelRun(principal:RunPrincipal,workspaceId:string,projectId:string,runId:string,key:string,expectedVersion:number){
    positive(expectedVersion);const digest=requestDigest({workspaceId,projectId,runId,expectedVersion});return this.idempotent(principal,workspaceId,projectId,"run.cancel",key,`run:${runId}`,digest,"operate",async tx=>{
      const current=await tx.getRun(workspaceId,projectId,runId,true);if(!current)throw new RunHttpError("run_not_found","Run was not found");if(current.version!==expectedVersion)throw new RunHttpError("version_conflict","Run version changed");if(["succeeded","failed","cancelled"].includes(current.status))throw new RunHttpError("run_terminal","Run is already terminal");
      const run=await tx.cancelRun(current);if(!run)throw new RunHttpError("version_conflict","Run version changed");await tx.insertAudit(randomUUID(),workspaceId,principal.userId,"run.cancelled",{projectId,runId,expectedVersion,proofClass:run.proofClass});return{run};
    },200);
  }
  async listArtifacts(principal:RunPrincipal,workspaceId:string,projectId:string,status:ArtifactStatus|"all"="ready",cursor=""){validArtifactStatus(status);validCursor(cursor);return this.store.transaction(async tx=>{await this.authorize(tx,principal,workspaceId,projectId,"read",true);return tx.listArtifacts(workspaceId,projectId,status,cursor);});}
  async getArtifact(principal:RunPrincipal,workspaceId:string,projectId:string,artifactId:string){return this.store.transaction(async tx=>{await this.authorize(tx,principal,workspaceId,projectId,"read",true);const artifact=await tx.getArtifact(workspaceId,projectId,artifactId);if(!artifact)throw new RunHttpError("artifact_not_found","Artifact was not found");return{artifact};});}

  private async authorize(tx:RunTransaction,principal:RunPrincipal,workspaceId:string,projectId:string,capability:"read"|"operate",lock:boolean){const access=await tx.getAccess(workspaceId,projectId,principal.userId,lock);if(!access||access.workspaceStatus!=="active")throw new RunHttpError("workspace_not_found","Workspace was not found");if(!access.projectStatus)throw new RunHttpError("project_not_found","Project was not found");if(access.projectStatus!=="active"&&capability==="operate")throw new RunHttpError("project_not_found","Project was not found");if(!roleAllows(access.role,capability))throw new RunHttpError("permission_denied","Run action is not permitted");}
  private async idempotent<T>(principal:RunPrincipal,workspaceId:string,projectId:string,operation:string,key:string,targetKey:string,digest:string,capability:"read"|"operate",execute:(tx:RunTransaction)=>Promise<T>,status:number):Promise<T>{validKey(key);return this.store.transaction(async tx=>{await tx.lockIdempotency(principal.userId,operation,key);const receipt=await tx.getIdempotency(principal.userId,operation,key);if(receipt){verifyReceipt(receipt,workspaceId,targetKey,digest);await this.authorize(tx,principal,workspaceId,projectId,capability,true);return receipt.responseBody as T;}await this.authorize(tx,principal,workspaceId,projectId,capability,true);const response=await execute(tx);await tx.putIdempotency(principal.userId,operation,key,{workspaceId,targetKey,requestDigest:digest,responseStatus:status,responseBody:response});return response;});}
}

function validateCreate(input:CreateRunInput):CreateRunInput{if(!/^[a-z][a-z0-9.-]{0,95}$/.test(input.kind))invalid("Run kind is invalid");if(!["synthetic","local","sandbox","provider"].includes(input.proofClass))invalid("proofClass is invalid");if(!/^sha256:[0-9a-f]{64}$/.test(input.planDigest))invalid("planDigest is invalid");if(!Array.isArray(input.inputArtifactIds)||input.inputArtifactIds.length>1000||input.inputArtifactIds.some(id=>!uuid(id))||new Set(input.inputArtifactIds).size!==input.inputArtifactIds.length)invalid("inputArtifactIds are invalid");if(!Array.isArray(input.outputNames)||input.outputNames.length>1000||input.outputNames.some(name=>!/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(name))||new Set(input.outputNames).size!==input.outputNames.length)invalid("outputNames are invalid");return{kind:input.kind,proofClass:input.proofClass,planDigest:input.planDigest,inputArtifactIds:[...input.inputArtifactIds],outputNames:[...input.outputNames]};}
function validRunStatus(value:string){if(!["queued","running","succeeded","failed","cancelled","all"].includes(value))invalid("Run status filter is invalid");}
function validArtifactStatus(value:string){if(!["pending","ready","failed","deleted","all"].includes(value))invalid("Artifact status filter is invalid");}
function validCursor(value:string){if(value.length>512||(value!==""&&!uuid(value)))invalid("cursor is invalid");}
function validKey(value:string){if(typeof value!=="string"||value.length<8||value.length>128)invalid("Idempotency-Key is invalid");}
function positive(value:number){if(!Number.isSafeInteger(value)||value<1)invalid("expectedVersion must be a positive integer");}
function uuid(value:string){return/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);}
function verifyReceipt(receipt:IdempotencyReceipt,workspaceId:string,targetKey:string,digest:string){if(receipt.workspaceId!==workspaceId||receipt.targetKey!==targetKey||receipt.requestDigest!==digest)throw new RunHttpError("idempotency_conflict","Idempotency key is bound to another request");}
function invalid(message:string):never{throw new RunHttpError("invalid_request",message);}
