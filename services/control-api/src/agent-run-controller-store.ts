import type { QueryResultRow } from "pg";
import type { Database } from "./db.js";

export interface AgentRunWorkItem {
  runId:string;workspaceId:string;projectId:string;runVersion:number;leaseToken:string;leaseExpiresAt:string;attempt:number;
  requestedBy:string;planDigest:string;agentVersionId:string;agentVersionDigest:string;agentVersion:Record<string,unknown>;
  harnessDefinitionId:string;harnessVersionId:string;harnessVersionDigest:string;harnessVersion:Record<string,unknown>;
  harnessProfileId:string;harnessProfileDigest:string;harnessProfile:Record<string,unknown>;templateVersionId:string;templateDigest:string;
  modelRouteId:string;modelRouteVersion:number;modelProtocol:string;boundSandboxId?:string;boundNodeId?:string;
}
export type AgentRunRetryOutcome="fenced"|"terminal"|"retry_scheduled"|"failed";
export interface AgentRunControllerStore {
  enqueue(runId:string,workspaceId:string,agentVersionId:string,harnessProfileId:string):Promise<boolean>;
  claim(workerId:string,leaseSeconds:number):Promise<AgentRunWorkItem|undefined>;
  renew(runId:string,workerId:string,leaseToken:string,leaseSeconds:number):Promise<string|undefined>;
  bindSandbox(runId:string,workerId:string,leaseToken:string,expectedRunVersion:number,nodeId:string,sandboxId:string):Promise<boolean>;
  retry(runId:string,workerId:string,leaseToken:string,delaySeconds:number,errorCode:string):Promise<AgentRunRetryOutcome>;
  finalize(runId:string,workerId:string,leaseToken:string,expectedRunVersion:number,outcome:"succeeded"|"failed",
    errorCode:string|undefined,artifactIds:string[],steps:number,warnings:string[]):Promise<boolean>;
}

export class PgAgentRunControllerStore implements AgentRunControllerStore {
  constructor(private readonly database:Database){}
  async enqueue(runId:string,workspaceId:string,agentVersionId:string,harnessProfileId:string){
    const result=await this.database.query<{accepted:boolean}>("SELECT agent_run_enqueue($1,$2,$3,$4) AS accepted",[runId,workspaceId,agentVersionId,harnessProfileId]);
    return result.rows[0]?.accepted===true;
  }
  async claim(workerId:string,leaseSeconds:number){
    const result=await this.database.query("SELECT * FROM agent_run_controller_claim($1,$2)",[workerId,leaseSeconds]);
    return result.rows[0]?workItem(result.rows[0]):undefined;
  }
  async renew(runId:string,workerId:string,leaseToken:string,leaseSeconds:number){
    const result=await this.database.query<{renewed_until:Date|string|null}>("SELECT agent_run_controller_renew($1,$2,$3,$4) AS renewed_until",[runId,workerId,leaseToken,leaseSeconds]);
    const value=result.rows[0]?.renewed_until;return value?timestamp(value):undefined;
  }
  async bindSandbox(runId:string,workerId:string,leaseToken:string,expectedRunVersion:number,nodeId:string,sandboxId:string){
    const result=await this.database.query<{bound:boolean}>("SELECT agent_run_controller_bind_sandbox($1,$2,$3,$4,$5,$6) AS bound",[runId,workerId,leaseToken,expectedRunVersion,nodeId,sandboxId]);
    return result.rows[0]?.bound===true;
  }
  async retry(runId:string,workerId:string,leaseToken:string,delaySeconds:number,errorCode:string){
    const result=await this.database.query<{outcome:AgentRunRetryOutcome}>("SELECT agent_run_controller_retry($1,$2,$3,$4,$5) AS outcome",[runId,workerId,leaseToken,delaySeconds,errorCode]);
    return result.rows[0]?.outcome??"fenced";
  }
  async finalize(runId:string,workerId:string,leaseToken:string,expectedRunVersion:number,outcome:"succeeded"|"failed",errorCode:string|undefined,artifactIds:string[],steps:number,warnings:string[]){
    const result=await this.database.query<{completed:boolean}>("SELECT agent_run_controller_finalize($1,$2,$3,$4,$5,$6,$7,$8,$9) AS completed",[runId,workerId,leaseToken,expectedRunVersion,outcome,errorCode??null,artifactIds,steps,warnings]);
    return result.rows[0]?.completed===true;
  }
}

function workItem(row:QueryResultRow):AgentRunWorkItem{
  const item:AgentRunWorkItem={runId:text(row.run_id),workspaceId:text(row.workspace_id),projectId:text(row.project_id),runVersion:integer(row.run_version,"Run version"),
    leaseToken:text(row.lease_token),leaseExpiresAt:timestamp(row.lease_expires_at),attempt:integer(row.attempt,"attempt"),requestedBy:text(row.requested_by),
    planDigest:text(row.plan_digest),agentVersionId:text(row.agent_version_id),agentVersionDigest:text(row.agent_version_digest),agentVersion:document(row.agent_version,"AgentVersion"),
    harnessDefinitionId:text(row.harness_definition_id),harnessVersionId:text(row.harness_version_id),harnessVersionDigest:text(row.harness_version_digest),
    harnessVersion:document(row.harness_version,"HarnessVersion"),harnessProfileId:text(row.harness_profile_id),harnessProfileDigest:text(row.harness_profile_digest),
    harnessProfile:document(row.harness_profile,"HarnessProfile"),templateVersionId:text(row.template_version_id),templateDigest:text(row.template_digest),
    modelRouteId:text(row.model_route_id),modelRouteVersion:integer(row.model_route_version,"model route version"),modelProtocol:text(row.model_protocol)};
  if(row.bound_sandbox_id)item.boundSandboxId=text(row.bound_sandbox_id);if(row.bound_node_id)item.boundNodeId=text(row.bound_node_id);
  for(const [label,value] of Object.entries({run:item.runId,workspace:item.workspaceId,project:item.projectId,lease:item.leaseToken,requester:item.requestedBy,
    agentVersion:item.agentVersionId,harnessDefinition:item.harnessDefinitionId,harnessVersion:item.harnessVersionId,harnessProfile:item.harnessProfileId,
    template:item.templateVersionId,modelRoute:item.modelRouteId}))if(!value)throw new Error(`Agent Run controller ${label} identity is invalid`);
  for(const digest of [item.planDigest,item.agentVersionDigest,item.harnessVersionDigest,item.harnessProfileDigest,item.templateDigest])
    if(!/^sha256:[0-9a-f]{64}$/.test(digest))throw new Error("Agent Run controller digest is invalid");
  return item;
}
function text(value:unknown){return typeof value==="string"?value:"";}
function integer(value:unknown,label:string){const result=Number(value);if(!Number.isSafeInteger(result)||result<1)throw new Error(`Agent Run controller ${label} is invalid`);return result;}
function timestamp(value:unknown){const result=value instanceof Date?value:new Date(String(value));if(Number.isNaN(result.valueOf()))throw new Error("Agent Run controller timestamp is invalid");return result.toISOString();}
function document(value:unknown,label:string):Record<string,unknown>{if(!value||typeof value!=="object"||Array.isArray(value))throw new Error(`Agent Run controller ${label} is invalid`);return value as Record<string,unknown>;}
