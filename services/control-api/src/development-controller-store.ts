import type { QueryResultRow } from "pg";
import type { Database } from "./db.js";

export interface DevelopmentControllerWorkItem {
  buildId:string;workspaceId:string;projectId:string;runId:string;buildVersion:number;
  leaseToken:string;leaseExpiresAt:string;attempt:number;
  requestedBy:string;source:{repository:string;commit:string};projectManifestDigest:string;
  projectSnapshot:Record<string,unknown>;planDigest:string;createdAt:string;
}

export interface DevelopmentControllerStore {
  claim(workerId:string,leaseSeconds:number):Promise<DevelopmentControllerWorkItem|undefined>;
  renew(buildId:string,workerId:string,leaseToken:string,leaseSeconds:number):Promise<string|undefined>;
  resolve(buildId:string,workerId:string,leaseToken:string):Promise<DevelopmentControllerWorkItem|undefined>;
  finalize(buildId:string,workerId:string,leaseToken:string,expectedVersion:number,execution:{nodeId:string;sandboxId:string},document:Record<string,unknown>):Promise<boolean>;
}

export class PgDevelopmentControllerStore implements DevelopmentControllerStore {
  constructor(private readonly database:Database){}
  async claim(workerId:string,leaseSeconds:number){
    const result=await this.database.query("SELECT * FROM development_controller_claim($1,$2)",[workerId,leaseSeconds]);
    return result.rows[0]?workItem(result.rows[0]):undefined;
  }
  async renew(buildId:string,workerId:string,leaseToken:string,leaseSeconds:number){
    const result=await this.database.query<{renewed_until:Date|string|null}>(
      "SELECT development_controller_renew($1,$2,$3,$4) AS renewed_until",[buildId,workerId,leaseToken,leaseSeconds]);
    const value=result.rows[0]?.renewed_until;return value?timestamp(value):undefined;
  }
  async resolve(buildId:string,workerId:string,leaseToken:string){
    const result=await this.database.query("SELECT * FROM development_controller_resolve($1,$2,$3)",[buildId,workerId,leaseToken]);
    return result.rows[0]?workItem(result.rows[0]):undefined;
  }
  async finalize(buildId:string,workerId:string,leaseToken:string,expectedVersion:number,execution:{nodeId:string;sandboxId:string},document:Record<string,unknown>){
    const result=await this.database.query<{completed:boolean}>(
      "SELECT development_controller_finalize_v1($1,$2,$3,$4,$5,$6,$7::jsonb) AS completed",
      [buildId,workerId,leaseToken,expectedVersion,execution.nodeId,execution.sandboxId,JSON.stringify(document)]);
    return result.rows[0]?.completed===true;
  }
}

function workItem(row:QueryResultRow):DevelopmentControllerWorkItem {
  if(!row.project_snapshot||typeof row.project_snapshot!=="object"||Array.isArray(row.project_snapshot))throw new Error("development controller project snapshot is invalid");
  const item:DevelopmentControllerWorkItem={buildId:row.build_id,workspaceId:row.workspace_id,projectId:row.project_id,
    runId:row.run_id,buildVersion:safeInteger(row.build_version,"build version"),leaseToken:row.lease_token,
    leaseExpiresAt:timestamp(row.lease_expires_at),attempt:safeInteger(row.attempt,"attempt"),requestedBy:row.requested_by,
    source:{repository:row.source_repository,commit:row.source_commit},projectManifestDigest:row.project_manifest_digest,
    projectSnapshot:row.project_snapshot,planDigest:row.plan_digest,createdAt:timestamp(row.created_at)};
  for(const value of [item.buildId,item.workspaceId,item.projectId,item.runId,item.leaseToken,item.requestedBy])if(typeof value!=="string"||!value)throw new Error("development controller identity is invalid");
  if(typeof item.source.repository!=="string"||typeof item.source.commit!=="string"||typeof item.projectManifestDigest!=="string"||typeof item.planDigest!=="string")throw new Error("development controller input is invalid");
  return item;
}
function safeInteger(value:unknown,label:string){const parsed=Number(value);if(!Number.isSafeInteger(parsed)||parsed<1)throw new Error(`development controller ${label} is invalid`);return parsed;}
function timestamp(value:Date|string){const result=value instanceof Date?value:new Date(value);if(Number.isNaN(result.valueOf()))throw new Error("development controller timestamp is invalid");return result.toISOString();}
