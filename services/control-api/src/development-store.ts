import type {PoolClient,QueryResultRow} from "pg";
import type {Database} from "./db.js";
import type {IdempotencyReceipt} from "./workspace-store.js";
import type {DevelopmentAccess,DevelopmentBuildRecord,DevelopmentBuildStatus,DevelopmentProjectRecord} from "./development-types.js";

export interface DevelopmentTransaction {
  lockIdempotency(principalId:string,operation:string,key:string):Promise<void>;
  getIdempotency(principalId:string,operation:string,key:string):Promise<IdempotencyReceipt|undefined>;
  putIdempotency(principalId:string,operation:string,key:string,receipt:IdempotencyReceipt):Promise<void>;
  getAccess(workspaceId:string,projectId:string,userId:string,lock?:boolean):Promise<DevelopmentAccess|undefined>;
  getProject(workspaceId:string,projectId:string,lock?:boolean):Promise<DevelopmentProjectRecord|undefined>;
  putProject(input:{workspaceId:string;projectId:string;expectedVersion:number;manifest:Record<string,unknown>;manifestDigest:string;templateVersionId:string;templateDigest:string;publicationTemplateId:string;createdBy:string}):Promise<DevelopmentProjectRecord|undefined>;
  createBuild(input:{id:string;workspaceId:string;projectId:string;runId:string;requestedBy:string;repository:string;commit:string;projectManifestDigest:string;planDigest:string}):Promise<DevelopmentBuildRecord>;
  getBuild(workspaceId:string,projectId:string,id:string):Promise<DevelopmentBuildRecord|undefined>;
  listBuilds(workspaceId:string,projectId:string,status:DevelopmentBuildStatus|"all",cursor:string):Promise<{items:DevelopmentBuildRecord[];nextCursor:string|null}>;
  insertAudit(id:string,workspaceId:string,actorUserId:string,type:string,payload:unknown):Promise<void>;
}
export interface DevelopmentStore {transaction<T>(action:(transaction:DevelopmentTransaction)=>Promise<T>):Promise<T>}

export class PgDevelopmentStore implements DevelopmentStore {
  constructor(private readonly database:Database){}
  async transaction<T>(action:(transaction:DevelopmentTransaction)=>Promise<T>):Promise<T>{const client=await this.database.connect();try{await client.query("BEGIN");const result=await action(new PgDevelopmentTransaction(client));await client.query("COMMIT");return result;}catch(error){await client.query("ROLLBACK");throw error;}finally{client.release();}}
}

class PgDevelopmentTransaction implements DevelopmentTransaction {
  constructor(private readonly client:PoolClient){}
  async lockIdempotency(p:string,o:string,k:string){await this.client.query("SELECT pg_advisory_xact_lock(hashtextextended($1,0))",[`${p}\n${o}\n${k}`]);}
  async getIdempotency(p:string,o:string,k:string){const result=await this.client.query("SELECT workspace_id,target_key,request_digest,response_status,response_body FROM workspace_idempotency_receipts WHERE principal_id=$1 AND operation=$2 AND idempotency_key=$3",[p,o,k]);const row=result.rows[0];return row?{workspaceId:row.workspace_id,targetKey:row.target_key,requestDigest:row.request_digest.trim(),responseStatus:row.response_status,responseBody:row.response_body}:undefined;}
  async putIdempotency(p:string,o:string,k:string,r:IdempotencyReceipt){await this.client.query("INSERT INTO workspace_idempotency_receipts(principal_id,workspace_id,operation,target_key,idempotency_key,request_digest,response_status,response_body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)",[p,r.workspaceId,o,r.targetKey,k,r.requestDigest,r.responseStatus,r.responseBody]);}
  async getAccess(w:string,p:string,u:string,lock=false){const suffix=lock?" FOR UPDATE OF w,m":"";const result=await this.client.query(`SELECT w.status workspace_status,m.role,p.status project_status FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id LEFT JOIN projects p ON p.workspace_id=w.id AND p.id=$2 WHERE w.id=$1 AND m.user_id=$3 AND m.status='active'${suffix}`,[w,p,u]);const row=result.rows[0];return row?{workspaceStatus:row.workspace_status,role:row.role,...(row.project_status?{projectStatus:row.project_status}:{})}:undefined;}
  async getProject(w:string,p:string,lock=false){const result=await this.client.query(`SELECT * FROM development_projects WHERE workspace_id=$1 AND project_id=$2${lock?" FOR UPDATE":""}`,[w,p]);return result.rows[0]?projectRow(result.rows[0]):undefined;}
  async putProject(i:{workspaceId:string;projectId:string;expectedVersion:number;manifest:Record<string,unknown>;manifestDigest:string;templateVersionId:string;templateDigest:string;publicationTemplateId:string;createdBy:string}){const raw=i.templateDigest.slice(7);if(i.expectedVersion===0){const result=await this.client.query(`INSERT INTO development_projects(project_id,workspace_id,manifest,manifest_digest,template_version_id,template_version,template_digest,publication_template_id,created_by)
      SELECT $1,$2,$3,$4,v.id,v.version,v.content_digest,$6,$7 FROM sandbox_template_versions v JOIN sandbox_template_version_status s ON s.version_id=v.id
      WHERE v.id=$5 AND v.workspace_id=$2 AND v.template_id=$6 AND v.content_digest=$8 AND s.status='published'
      ON CONFLICT (project_id) DO NOTHING RETURNING *`,[i.projectId,i.workspaceId,i.manifest,i.manifestDigest,i.templateVersionId,i.publicationTemplateId,i.createdBy,raw]);return result.rows[0]?projectRow(result.rows[0]):undefined;}const result=await this.client.query(`UPDATE development_projects project SET manifest=$3,manifest_digest=$4,template_version_id=v.id,template_version=v.version,template_digest=v.content_digest,publication_template_id=$6,version=project.version+1,updated_at=clock_timestamp()
      FROM sandbox_template_versions v JOIN sandbox_template_version_status s ON s.version_id=v.id
      WHERE project.workspace_id=$1 AND project.project_id=$2 AND project.version=$7 AND v.id=$5 AND v.workspace_id=$1 AND v.template_id=$6 AND v.content_digest=$8 AND s.status='published' RETURNING project.*`,[i.workspaceId,i.projectId,i.manifest,i.manifestDigest,i.templateVersionId,i.publicationTemplateId,i.expectedVersion,raw]);return result.rows[0]?projectRow(result.rows[0]):undefined;}
  async createBuild(i:{id:string;workspaceId:string;projectId:string;runId:string;requestedBy:string;repository:string;commit:string;projectManifestDigest:string;planDigest:string}){await this.client.query("INSERT INTO runs(id,workspace_id,project_id,kind,proof_class,plan_digest,requested_by) VALUES($1,$2,$3,'development.build','sandbox',$4,$5)",[i.runId,i.workspaceId,i.projectId,i.planDigest,i.requestedBy]);const result=await this.client.query("INSERT INTO development_builds(id,workspace_id,project_id,run_id,requested_by,source_repository,source_commit,project_manifest_digest,plan_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *",[i.id,i.workspaceId,i.projectId,i.runId,i.requestedBy,i.repository,i.commit,i.projectManifestDigest,i.planDigest]);return buildRow(result.rows[0]!);}
  async getBuild(w:string,p:string,id:string){const result=await this.client.query("SELECT * FROM development_builds WHERE workspace_id=$1 AND project_id=$2 AND id=$3",[w,p,id]);return result.rows[0]?buildRow(result.rows[0]):undefined;}
  async listBuilds(w:string,p:string,status:DevelopmentBuildStatus|"all",cursor:string){const result=await this.client.query("SELECT * FROM development_builds WHERE workspace_id=$1 AND project_id=$2 AND ($3='all' OR status=$3) AND ($4='' OR id::text>$4) ORDER BY id LIMIT 101",[w,p,status,cursor]);const items=result.rows.slice(0,100).map(buildRow);return{items,nextCursor:result.rows.length>100?items.at(-1)?.id??null:null};}
  async insertAudit(id:string,w:string,u:string,type:string,payload:unknown){await this.client.query("INSERT INTO workspace_audit_events(id,workspace_id,actor_user_id,event_type,payload) VALUES($1,$2,$3,$4,$5)",[id,w,u,type,payload]);}
}
function projectRow(row:QueryResultRow):DevelopmentProjectRecord{return{workspaceId:row.workspace_id,projectId:row.project_id,version:Number(row.version),manifest:row.manifest,manifestDigest:row.manifest_digest,createdBy:row.created_by,createdAt:timestamp(row.created_at),updatedAt:timestamp(row.updated_at)};}
function buildRow(row:QueryResultRow):DevelopmentBuildRecord{const value:DevelopmentBuildRecord={schemaVersion:"blazn.dev/build-status/v1alpha1",id:row.id,workspaceId:row.workspace_id,projectId:row.project_id,runId:row.run_id,version:Number(row.version),status:row.status,requestedBy:row.requested_by,source:{repository:row.source_repository,commit:row.source_commit},projectManifestDigest:row.project_manifest_digest,planDigest:row.plan_digest,publication:{eligible:row.publication_eligible,refusalReasons:row.refusal_reasons,published:null},finalDocument:row.final_document,createdAt:timestamp(row.created_at)};if(row.started_at)value.startedAt=timestamp(row.started_at);if(row.completed_at)value.completedAt=timestamp(row.completed_at);if(row.error_code)value.errorCode=row.error_code;return value;}
function timestamp(value:Date|string){return value instanceof Date?value.toISOString():new Date(value).toISOString();}
