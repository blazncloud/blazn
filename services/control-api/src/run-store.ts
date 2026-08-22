import type { PoolClient, QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { IdempotencyReceipt } from "./workspace-store.js";
import type { Artifact, ArtifactStatus, Run, RunAccess, RunReceipt, RunStatus } from "./run-types.js";

export interface RunTransaction {
  lockIdempotency(principalId:string,operation:string,key:string):Promise<void>;
  getIdempotency(principalId:string,operation:string,key:string):Promise<IdempotencyReceipt|undefined>;
  putIdempotency(principalId:string,operation:string,key:string,receipt:IdempotencyReceipt):Promise<void>;
  getAccess(workspaceId:string,projectId:string,userId:string,lock?:boolean):Promise<RunAccess|undefined>;
  createRun(input:{id:string;workspaceId:string;projectId:string;kind:string;proofClass:string;planDigest:string;outputNames:string[];requestedBy:string;inputArtifactIds:string[]}):Promise<Run>;
  getRun(workspaceId:string,projectId:string,runId:string,lock?:boolean):Promise<Run|undefined>;
  listRuns(workspaceId:string,projectId:string,status:RunStatus|"all",cursor?:string):Promise<{items:Run[];nextCursor:string|null}>;
  cancelRun(run:Run):Promise<Run|undefined>;
  getArtifact(workspaceId:string,projectId:string,artifactId:string):Promise<Artifact|undefined>;
  listArtifacts(workspaceId:string,projectId:string,status:ArtifactStatus|"all",cursor?:string):Promise<{items:Artifact[];nextCursor:string|null}>;
  insertAudit(id:string,workspaceId:string,actorUserId:string,type:string,payload:unknown):Promise<void>;
}
export interface RunStore { transaction<T>(action:(transaction:RunTransaction)=>Promise<T>):Promise<T> }

export class PgRunStore implements RunStore {
  constructor(private readonly database:Database){}
  async transaction<T>(action:(transaction:RunTransaction)=>Promise<T>):Promise<T>{
    const client=await this.database.connect(); try{await client.query("BEGIN");const result=await action(new PgRunTransaction(client));await client.query("COMMIT");return result;}catch(error){await client.query("ROLLBACK");throw error;}finally{client.release();}
  }
}

class PgRunTransaction implements RunTransaction {
  constructor(private readonly client:PoolClient){}
  async lockIdempotency(principalId:string,operation:string,key:string){await this.client.query("SELECT pg_advisory_xact_lock(hashtextextended($1,0))",[`${principalId}\n${operation}\n${key}`]);}
  async getIdempotency(principalId:string,operation:string,key:string){const result=await this.client.query("SELECT workspace_id,target_key,request_digest,response_status,response_body FROM workspace_idempotency_receipts WHERE principal_id=$1 AND operation=$2 AND idempotency_key=$3",[principalId,operation,key]);const row=result.rows[0];return row?{workspaceId:row.workspace_id,targetKey:row.target_key,requestDigest:row.request_digest.trim(),responseStatus:row.response_status,responseBody:row.response_body}:undefined;}
  async putIdempotency(principalId:string,operation:string,key:string,receipt:IdempotencyReceipt){await this.client.query("INSERT INTO workspace_idempotency_receipts(principal_id,workspace_id,operation,target_key,idempotency_key,request_digest,response_status,response_body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)",[principalId,receipt.workspaceId,operation,receipt.targetKey,key,receipt.requestDigest,receipt.responseStatus,receipt.responseBody]);}
  async getAccess(workspaceId:string,projectId:string,userId:string,lock=false):Promise<RunAccess|undefined>{
    const suffix=lock?" FOR SHARE OF w,m":"";const result=await this.client.query(`SELECT w.status AS workspace_status,m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id AND m.user_id=$2 AND m.status='active' WHERE w.id=$1${suffix}`,[workspaceId,userId]);const row=result.rows[0];if(!row)return undefined;const project=await this.client.query(`SELECT status FROM projects WHERE workspace_id=$1 AND id=$2${lock?" FOR SHARE":""}`,[workspaceId,projectId]);const access:RunAccess={workspaceStatus:row.workspace_status,role:row.role};if(project.rows[0])access.projectStatus=project.rows[0].status;return access;
  }
  async createRun(input:{id:string;workspaceId:string;projectId:string;kind:string;proofClass:string;planDigest:string;outputNames:string[];requestedBy:string;inputArtifactIds:string[]}):Promise<Run>{
    const inserted=await this.client.query("INSERT INTO runs(id,workspace_id,project_id,kind,proof_class,plan_digest,output_names,requested_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *",[input.id,input.workspaceId,input.projectId,input.kind,input.proofClass,input.planDigest,input.outputNames,input.requestedBy]);
    if(input.inputArtifactIds.length){const linked=await this.client.query(`INSERT INTO run_input_artifacts(run_id,workspace_id,project_id,artifact_id,ordinal) SELECT $1,$2,$3,v.artifact_id,v.ordinal FROM unnest($4::uuid[]) WITH ORDINALITY AS v(artifact_id,ordinal) JOIN artifacts a ON a.id=v.artifact_id AND a.workspace_id=$2 AND a.project_id=$3 AND a.status='ready' RETURNING artifact_id`,[input.id,input.workspaceId,input.projectId,input.inputArtifactIds]);if(linked.rowCount!==input.inputArtifactIds.length)throw new RunInputArtifactError();}
    await this.insertEvent(input.id,input.workspaceId,input.projectId,"run.queued",{});return runRow(inserted.rows[0]!,input.inputArtifactIds,null);
  }
  async getRun(workspaceId:string,projectId:string,runId:string,lock=false):Promise<Run|undefined>{if(lock){const locked=await this.client.query("SELECT id FROM runs WHERE workspace_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE",[workspaceId,projectId,runId]);if(!locked.rows[0])return undefined;}const result=await this.client.query(runSelect+" WHERE r.workspace_id=$1 AND r.project_id=$2 AND r.id=$3 GROUP BY r.id,rr.receipt",[workspaceId,projectId,runId]);return result.rows[0]?runRow(result.rows[0]):undefined;}
  async listRuns(workspaceId:string,projectId:string,status:RunStatus|"all",cursor=""){const result=await this.client.query(runSelect+" WHERE r.workspace_id=$1 AND r.project_id=$2 AND ($3='all' OR r.status=$3) AND ($4='' OR r.id::text>$4) GROUP BY r.id,rr.receipt ORDER BY r.id LIMIT 101",[workspaceId,projectId,status,cursor]);const items=result.rows.slice(0,100).map(row=>runRow(row));return{items,nextCursor:result.rows.length>100?items.at(-1)?.id??null:null};}
  async cancelRun(run:Run):Promise<Run|undefined>{
    const receipt:RunReceipt={schemaVersion:"blazn.run/receipt/v1alpha1",proofClass:run.proofClass,outcome:"cancelled",planDigest:run.planDigest,artifactIds:[],summary:{steps:0,warnings:[]}};
    await this.client.query("INSERT INTO run_receipts(run_id,workspace_id,project_id,proof_class,outcome,plan_digest,receipt) VALUES($1,$2,$3,$4,'cancelled',$5,$6)",[run.id,run.workspaceId,run.projectId,run.proofClass,run.planDigest,receipt]);
    const result=await this.client.query("UPDATE runs SET status='cancelled',version=version+1,completed_at=clock_timestamp(),error_code=NULL WHERE id=$1 AND workspace_id=$2 AND project_id=$3 AND version=$4 AND status IN ('queued','running') RETURNING *",[run.id,run.workspaceId,run.projectId,run.version]);if(!result.rows[0])return undefined;
    await this.insertEvent(run.id,run.workspaceId,run.projectId,"run.cancelled",{expectedVersion:run.version});return runRow(result.rows[0],run.inputArtifactIds,receipt);
  }
  async getArtifact(workspaceId:string,projectId:string,artifactId:string){const result=await this.client.query("SELECT * FROM artifacts WHERE workspace_id=$1 AND project_id=$2 AND id=$3",[workspaceId,projectId,artifactId]);return result.rows[0]?artifactRow(result.rows[0]):undefined;}
  async listArtifacts(workspaceId:string,projectId:string,status:ArtifactStatus|"all",cursor=""){const result=await this.client.query("SELECT * FROM artifacts WHERE workspace_id=$1 AND project_id=$2 AND ($3='all' OR status=$3) AND ($4='' OR id::text>$4) ORDER BY id LIMIT 101",[workspaceId,projectId,status,cursor]);const items=result.rows.slice(0,100).map(artifactRow);return{items,nextCursor:result.rows.length>100?items.at(-1)?.id??null:null};}
  async insertAudit(id:string,workspaceId:string,actorUserId:string,type:string,payload:unknown){await this.client.query("INSERT INTO workspace_audit_events(id,workspace_id,actor_user_id,event_type,payload) VALUES($1,$2,$3,$4,$5)",[id,workspaceId,actorUserId,type,payload]);}
  private async insertEvent(runId:string,workspaceId:string,projectId:string,type:string,payload:unknown){await this.client.query("INSERT INTO run_events(run_id,workspace_id,project_id,sequence,type,payload) SELECT $1,$2,$3,coalesce(max(sequence)+1,0),$4,$5 FROM run_events WHERE run_id=$1",[runId,workspaceId,projectId,type,payload]);}
}

export class RunInputArtifactError extends Error {}
const runSelect=`SELECT r.*,rr.receipt,coalesce(array_agg(ri.artifact_id ORDER BY ri.ordinal) FILTER (WHERE ri.artifact_id IS NOT NULL),'{}'::uuid[]) AS input_artifact_ids FROM runs r LEFT JOIN run_receipts rr ON rr.run_id=r.id LEFT JOIN run_input_artifacts ri ON ri.run_id=r.id`;
function runRow(row:QueryResultRow,inputIds?:string[],receipt?:RunReceipt|null):Run{const placement:Record<string,string>={};if(row.node_id)placement.nodeId=row.node_id;if(row.sandbox_id)placement.sandboxId=row.sandbox_id;if(row.model_route_id)placement.modelRouteId=row.model_route_id;const value:Run={id:row.id,workspaceId:row.workspace_id,projectId:row.project_id,kind:row.kind,proofClass:row.proof_class,status:row.status,version:Number(row.version),planDigest:row.plan_digest,inputArtifactIds:inputIds??row.input_artifact_ids??[],outputNames:row.output_names,requestedBy:row.requested_by,placement:Object.keys(placement).length?placement:null,receipt:receipt===undefined?(row.receipt??null):receipt,createdAt:timestamp(row.created_at)};if(row.started_at)value.startedAt=timestamp(row.started_at);if(row.completed_at)value.completedAt=timestamp(row.completed_at);if(row.error_code)value.errorCode=row.error_code;return value;}
function artifactRow(row:QueryResultRow):Artifact{const value:Artifact={id:row.id,workspaceId:row.workspace_id,projectId:row.project_id,kind:row.kind,mediaType:row.media_type,name:row.name,status:row.status,version:Number(row.version),createdBy:row.created_by,createdAt:timestamp(row.created_at),updatedAt:timestamp(row.updated_at),downloadAvailable:row.status==="ready"};if(row.source_run_id)value.sourceRunId=row.source_run_id;if(row.digest)value.digest=row.digest;if(row.size_bytes!==null&&row.size_bytes!==undefined)value.sizeBytes=Number(row.size_bytes);return value;}
function timestamp(value:Date|string):string{return value instanceof Date?value.toISOString():new Date(value).toISOString();}
