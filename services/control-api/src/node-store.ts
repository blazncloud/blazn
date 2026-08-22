import type { PoolClient, QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { EnrollmentRecord, KubernetesBinding, NodeEvent, NodeOperationType, NodeOperationView, NodeView } from "./node-types.js";

export interface NodeIdempotencyReceipt { workspaceId: string; targetKey: string; requestDigest: string; responseStatus: number; responseBody: unknown }
export interface NodeAuthority { workspaceId: string; role: "owner" | "administrator" | "operator" | "member" | "viewer"; workspaceStatus: string }
export interface ActiveNodeIdentity { nodeId: string; workspaceId: string; generation: number; publicKey: string; publicKeyFingerprint: string; signingKeyId: string; lifecycleState: string; trustState: string; nodeVersion: number }
export interface HeartbeatState { identityGeneration: number; bootId: string; sequence: number; sentAt: Date; capabilityDigest: string | null }
export interface JoinConsumeReceipt { issuanceId: string; requestDigest: string }

export interface NodeTransaction {
  lockIdempotency(principalId: string, operation: string, key: string): Promise<void>;
  getIdempotency(principalId: string, operation: string, key: string): Promise<NodeIdempotencyReceipt | undefined>;
  putIdempotency(principalId: string, operation: string, key: string, receipt: NodeIdempotencyReceipt): Promise<void>;
  authority(workspaceId: string, userId: string, lockWorkspace?: boolean): Promise<NodeAuthority | undefined>;
  insertEnrollment(input: { id: string; workspaceId: string; name: string; mode: string; platform: string; architecture: string | null; tokenHash: string; idempotencyKey: string; createdBy: string; expiresAt: Date }): Promise<void>;
  enrollmentById(id: string, lock?: boolean): Promise<EnrollmentRecord | undefined>;
  planByEnrollment(enrollmentId: string): Promise<Record<string, unknown> | undefined>;
  createExchangedNode(input: { nodeId: string; identityId: string; enrollment: EnrollmentRecord; architecture: string; machineFingerprint: string; publicKey: string; publicKeyFingerprint: string; kubernetesBinding?: KubernetesBinding; planId: string; plan: Record<string, unknown>; planDigest: string; signingKeyId: string; signature: string; issuedAt: Date; expiresAt: Date }): Promise<void>;
  nodeById(nodeId: string, lock?: boolean): Promise<NodeView | undefined>;
  listNodes(workspaceId: string): Promise<NodeView[]>;
  activeIdentity(nodeId: string, lockNode?: boolean): Promise<ActiveNodeIdentity | undefined>;
  heartbeatState(nodeId: string): Promise<HeartbeatState | undefined>;
  bootObserved(nodeId: string, identityGeneration: number, bootId: string): Promise<boolean>;
  observeBoot(nodeId: string, workspaceId: string, identityGeneration: number, bootId: string, sentAt: Date): Promise<void>;
  recordHeartbeat(input: { nodeId: string; identityGeneration: number; bootId: string; sequence: number; sentAt: Date; capabilityDigest: string; capability: Record<string, unknown>; health: unknown }): Promise<void>;
  insertOperation(input: { id: string; workspaceId: string; nodeId: string; type: NodeOperationType; expectedVersion: number; requestedBy: string; idempotencyKey: string; requestDigest: string; parameters: Record<string, unknown> }): Promise<NodeOperationView>;
  listEvents(nodeId: string, afterId: string): Promise<NodeEvent[]>;
  consumeJoin(input: { issuanceId: string; nodeId: string; enrollmentId: string; planId: string; clusterId: string; nodeName: string; nodeUid: string; resourceVersion: string; idempotencyKey: string; requestDigest: string }): Promise<NodeView>;
}

export interface NodeStore { transaction<T>(action: (tx: NodeTransaction) => Promise<T>): Promise<T> }

export class PgNodeStore implements NodeStore {
  constructor(private readonly database: Database) {}
  async transaction<T>(action: (tx: NodeTransaction) => Promise<T>): Promise<T> {
    const client = await this.database.connect();
    try { await client.query("BEGIN"); const result = await action(new PgNodeTransaction(client)); await client.query("COMMIT"); return result; }
    catch (error) { await client.query("ROLLBACK"); throw error; }
    finally { client.release(); }
  }
}

class PgNodeTransaction implements NodeTransaction {
  constructor(private readonly client: PoolClient) {}
  async lockIdempotency(principalId: string, operation: string, key: string): Promise<void> {
    await this.client.query("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", [`${principalId}\n${operation}\n${key}`]);
  }
  async getIdempotency(principalId: string, operation: string, key: string): Promise<NodeIdempotencyReceipt | undefined> {
    const result = await this.client.query("SELECT workspace_id,target_key,request_digest,response_status,response_body FROM workspace_idempotency_receipts WHERE principal_id=$1 AND operation=$2 AND idempotency_key=$3", [principalId, operation, key]);
    const row = result.rows[0];
    return row ? { workspaceId: row.workspace_id, targetKey: row.target_key, requestDigest: row.request_digest.trim(), responseStatus: row.response_status, responseBody: row.response_body } : undefined;
  }
  async putIdempotency(principalId: string, operation: string, key: string, receipt: NodeIdempotencyReceipt): Promise<void> {
    await this.client.query("INSERT INTO workspace_idempotency_receipts(principal_id,workspace_id,operation,target_key,idempotency_key,request_digest,response_status,response_body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", [principalId, receipt.workspaceId, operation, receipt.targetKey, key, receipt.requestDigest, receipt.responseStatus, receipt.responseBody]);
  }
  async authority(workspaceId: string, userId: string, lockWorkspace = false): Promise<NodeAuthority | undefined> {
    const result = await this.client.query(`SELECT w.id AS workspace_id,w.status AS workspace_status,m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id WHERE w.id=$1 AND m.user_id=$2 AND m.status='active'${lockWorkspace ? " FOR UPDATE OF w" : ""}`, [workspaceId, userId]);
    const row = result.rows[0]; return row ? { workspaceId: row.workspace_id, workspaceStatus: row.workspace_status, role: row.role } : undefined;
  }
  async insertEnrollment(input: { id: string; workspaceId: string; name: string; mode: string; platform: string; architecture: string | null; tokenHash: string; idempotencyKey: string; createdBy: string; expiresAt: Date }): Promise<void> {
    await this.client.query(`INSERT INTO node_enrollments(id,workspace_id,requested_name,mode,expected_platform,expected_architecture,token_hash,token_key_id,idempotency_key,created_by,expires_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,'node-enrollment/v1',$8,$9,$10)`, [input.id,input.workspaceId,input.name,input.mode,input.platform,input.architecture,input.tokenHash,input.idempotencyKey,input.createdBy,input.expiresAt]);
  }
  async enrollmentById(id: string, lock = false): Promise<EnrollmentRecord | undefined> {
    const result = await this.client.query(`SELECT * FROM node_enrollments WHERE id=$1${lock ? " FOR UPDATE" : ""}`, [id]);
    return result.rows[0] ? enrollmentRow(result.rows[0]) : undefined;
  }
  async planByEnrollment(enrollmentId: string): Promise<Record<string, unknown> | undefined> {
    const result = await this.client.query("SELECT canonical_plan FROM node_install_plans WHERE enrollment_id=$1", [enrollmentId]);
    return result.rows[0]?.canonical_plan;
  }
  async createExchangedNode(input: { nodeId: string; identityId: string; enrollment: EnrollmentRecord; architecture: string; machineFingerprint: string; publicKey: string; publicKeyFingerprint: string; kubernetesBinding?: KubernetesBinding; planId: string; plan: Record<string, unknown>; planDigest: string; signingKeyId: string; signature: string; issuedAt: Date; expiresAt: Date }): Promise<void> {
    await this.client.query(`INSERT INTO nodes(id,workspace_id,name,kind,owner_user_id,machine_fingerprint,host_platform,host_architecture,lifecycle_state,trust_state,agent_eligible,service_version,kubernetes_cluster_id,kubernetes_node_name,kubernetes_node_uid,kubernetes_resource_version)
      VALUES($1,$2,$3,'shared',$4,$5,$6,$7,'installing','verifying',false,'pending',$8,$9,$10,$11)`, [input.nodeId,input.enrollment.workspaceId,input.enrollment.requestedName,input.enrollment.createdBy,input.machineFingerprint,input.enrollment.expectedPlatform,input.architecture,input.kubernetesBinding?.clusterId??null,input.kubernetesBinding?.nodeName??null,input.kubernetesBinding?.nodeUid??null,input.kubernetesBinding?.resourceVersion??null]);
    await this.client.query(`INSERT INTO node_identities(id,node_id,public_key_fingerprint,public_key,signing_key_id,generation,status,issued_at,expires_at)
      VALUES($1,$2,$3,$4,'node-identity/v1',1,'active',$5,$6)`, [input.identityId,input.nodeId,input.publicKeyFingerprint,input.publicKey,input.issuedAt,new Date(input.issuedAt.getTime()+30*24*60*60_000)]);
    await this.client.query("UPDATE nodes SET current_identity_generation=1,current_identity_status='active' WHERE id=$1", [input.nodeId]);
    await this.client.query(`INSERT INTO node_install_plans(id,workspace_id,node_id,enrollment_id,approved_by,idempotency_key,plan_digest,signing_key_id,signature,canonical_plan,issued_at,expires_at,status)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'issued')`, [input.planId,input.enrollment.workspaceId,input.nodeId,input.enrollment.id,input.enrollment.createdBy,input.enrollment.idempotencyKey,input.planDigest,input.signingKeyId,input.signature,input.plan,input.issuedAt,input.expiresAt]);
    await this.client.query(`UPDATE node_enrollments SET status='exchanged',machine_binding=$2,node_public_key=$3,node_public_key_fingerprint=$4,exchanged_at=$5,version=version+1 WHERE id=$1`, [input.enrollment.id,input.machineFingerprint,input.publicKey,input.publicKeyFingerprint,input.issuedAt]);
    await this.client.query("INSERT INTO node_audit_events(id,workspace_id,node_id,actor_user_id,event_type,payload) VALUES(gen_random_uuid(),$1,$2,$3,'node.enrollment_exchanged',$4)", [input.enrollment.workspaceId,input.nodeId,input.enrollment.createdBy,{ enrollmentId: input.enrollment.id, planId: input.planId }]);
  }
  async nodeById(nodeId: string, lock = false): Promise<NodeView | undefined> {
    const result = await this.client.query(`${nodeSelect()} WHERE n.id=$1${lock ? " FOR UPDATE OF n" : ""}`, [nodeId]);
    return result.rows[0] ? nodeRow(result.rows[0]) : undefined;
  }
  async listNodes(workspaceId: string): Promise<NodeView[]> {
    const result = await this.client.query(`${nodeSelect()} WHERE n.workspace_id=$1 ORDER BY n.id LIMIT 1001`, [workspaceId]);
    return result.rows.map(nodeRow);
  }
  async activeIdentity(nodeId: string, lockNode = false): Promise<ActiveNodeIdentity | undefined> {
    const result = await this.client.query(`SELECT n.id AS node_id,n.workspace_id,n.lifecycle_state,n.trust_state,n.version AS node_version,i.generation,i.public_key,i.public_key_fingerprint,i.signing_key_id,i.expires_at
      FROM nodes n JOIN node_identities i ON i.node_id=n.id AND i.generation=n.current_identity_generation AND i.status='active' WHERE n.id=$1${lockNode ? " FOR UPDATE OF n,i" : ""}`, [nodeId]);
    const r=result.rows[0];if(!r)return undefined;const clock=await this.client.query<{now:Date}>("SELECT clock_timestamp() AS now");if(r.expires_at.getTime()<=clock.rows[0]!.now.getTime())return undefined;return { nodeId:r.node_id,workspaceId:r.workspace_id,generation:Number(r.generation),publicKey:r.public_key,publicKeyFingerprint:r.public_key_fingerprint.trim(),signingKeyId:r.signing_key_id,lifecycleState:r.lifecycle_state,trustState:r.trust_state,nodeVersion:Number(r.node_version) };
  }
  async heartbeatState(nodeId: string): Promise<HeartbeatState | undefined> {
    const result=await this.client.query("SELECT * FROM node_heartbeat_state WHERE node_id=$1",[nodeId]); const r=result.rows[0];
    return r ? {identityGeneration:Number(r.identity_generation),bootId:r.boot_id,sequence:Number(r.sequence),sentAt:r.sent_at,capabilityDigest:r.capability_digest?.trim() ?? null}:undefined;
  }
  async bootObserved(nodeId:string,identityGeneration:number,bootId:string):Promise<boolean>{
    const result=await this.client.query("SELECT 1 FROM node_audit_events WHERE node_id=$1 AND event_type='node.boot_observed' AND payload->>'identityGeneration'=$2 AND payload->>'bootId'=$3 LIMIT 1",[nodeId,String(identityGeneration),bootId]);return !!result.rowCount;
  }
  async observeBoot(nodeId:string,workspaceId:string,identityGeneration:number,bootId:string,sentAt:Date):Promise<void>{
    await this.client.query("INSERT INTO node_audit_events(id,workspace_id,node_id,event_type,payload) VALUES(gen_random_uuid(),$1,$2,'node.boot_observed',$3)",[workspaceId,nodeId,{identityGeneration,bootId,sentAt:sentAt.toISOString()}]);
  }
  async recordHeartbeat(input: { nodeId:string;identityGeneration:number;bootId:string;sequence:number;sentAt:Date;capabilityDigest:string;capability:Record<string,unknown>;health:unknown }): Promise<void> {
    const existing=await this.client.query<{version:string;digest:string}>("SELECT version,digest FROM node_capability_versions WHERE node_id=$1 AND digest=$2",[input.nodeId,input.capabilityDigest]);
    const version=Number(input.capability.version);
    if (!existing.rows[0]) await this.client.query("INSERT INTO node_capability_versions(id,node_id,version,digest,payload,observed_at) VALUES(gen_random_uuid(),$1,$2,$3,$4,$5)",[input.nodeId,version,input.capabilityDigest,input.capability,input.sentAt]);
    else if (Number(existing.rows[0].version)!==version) throw Object.assign(new Error("capability digest is bound to another version"),{code:"23505"});
    await this.client.query(`INSERT INTO node_heartbeat_state(node_id,identity_generation,boot_id,sequence,sent_at,received_at,capability_digest,health) VALUES($1,$2,$3,$4,$5,now(),$6,$7)
      ON CONFLICT(node_id) DO UPDATE SET identity_generation=EXCLUDED.identity_generation,boot_id=EXCLUDED.boot_id,sequence=EXCLUDED.sequence,sent_at=EXCLUDED.sent_at,received_at=now(),capability_digest=EXCLUDED.capability_digest,health=EXCLUDED.health`,[input.nodeId,input.identityGeneration,input.bootId,input.sequence,input.sentAt,input.capabilityDigest,input.health]);
    await this.client.query("UPDATE nodes SET last_heartbeat_at=now(),offline_after=now()+interval '90 seconds',current_capability_version=$2,updated_at=now() WHERE id=$1",[input.nodeId,version]);
  }
  async insertOperation(input: { id:string;workspaceId:string;nodeId:string;type:NodeOperationType;expectedVersion:number;requestedBy:string;idempotencyKey:string;requestDigest:string;parameters:Record<string,unknown> }): Promise<NodeOperationView> {
    const active=await this.client.query("SELECT 1 FROM node_operations WHERE node_id=$1 AND status IN ('pending','running') LIMIT 1",[input.nodeId]);
    if(active.rowCount) throw Object.assign(new Error("node already has an active operation"),{nodeCode:"state_conflict"});
    const result=await this.client.query(`INSERT INTO node_operations(id,workspace_id,node_id,type,status,expected_node_version,requested_by,idempotency_key,request_digest,parameters)
      VALUES($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9) RETURNING *`,[input.id,input.workspaceId,input.nodeId,input.type,input.expectedVersion,input.requestedBy,input.idempotencyKey,input.requestDigest,input.parameters]);
    await this.client.query("INSERT INTO node_operation_events(id,operation_id,sequence,type,payload) VALUES(gen_random_uuid(),$1,0,'operation.pending',$2)",[input.id,{type:input.type}]);
    return operationRow(result.rows[0]);
  }
  async listEvents(nodeId:string,afterId:string):Promise<NodeEvent[]> {
    if(afterId){const cursor=await this.client.query("SELECT 1 FROM node_operation_events e JOIN node_operations o ON o.id=e.operation_id WHERE e.id=$1 AND o.node_id=$2",[afterId,nodeId]);if(!cursor.rowCount)throw Object.assign(new Error("event cursor does not belong to this node"),{nodeCode:"invalid_request"});}
    const result=await this.client.query(`SELECT e.id,e.type,e.payload,e.created_at FROM node_operation_events e JOIN node_operations o ON o.id=e.operation_id WHERE o.node_id=$1 AND ($2='' OR (e.created_at,e.id)>(SELECT e2.created_at,e2.id FROM node_operation_events e2 JOIN node_operations o2 ON o2.id=e2.operation_id WHERE o2.node_id=$1 AND e2.id::text=$2)) ORDER BY e.created_at,e.id LIMIT 100`,[nodeId,afterId]);
    return result.rows.map((r)=>({id:r.id,type:r.type,payload:r.payload,createdAt:r.created_at.toISOString()}));
  }
  async consumeJoin(input:{issuanceId:string;nodeId:string;enrollmentId:string;planId:string;clusterId:string;nodeName:string;nodeUid:string;resourceVersion:string;idempotencyKey:string;requestDigest:string}):Promise<NodeView> {
    const result=await this.client.query(`SELECT id,workspace_id,enrollment_id,plan_id,node_id,expires_at,consumed_at,revoked_at FROM node_join_issuances WHERE id=$1 FOR UPDATE`,[input.issuanceId]);
    const row=result.rows[0];
    if (!row || row.node_id!==input.nodeId || row.enrollment_id!==input.enrollmentId || row.plan_id!==input.planId || row.revoked_at || row.expires_at.getTime()<=Date.now()) throw Object.assign(new Error("join credential is invalid"),{nodeCode:"join_credential_invalid"});
    const replayResult=await this.client.query("SELECT payload FROM node_audit_events WHERE node_id=$1 AND event_type='node.join_consumed' AND payload->>'idempotencyKey'=$2 ORDER BY created_at DESC LIMIT 1",[input.nodeId,input.idempotencyKey]);
    const replay=replayResult.rows[0]?.payload as Record<string,unknown>|undefined;
    if(replay){if(replay.issuanceId!==input.issuanceId||replay.requestDigest!==input.requestDigest)throw Object.assign(new Error("idempotency key is bound to another join consumption"),{nodeCode:"idempotency_conflict"});const replayNode=await this.nodeById(input.nodeId);if(!replayNode)throw new Error("joined node disappeared");return replayNode;}
    if (row.consumed_at) {
      throw Object.assign(new Error("join credential is consumed"),{nodeCode:"join_credential_consumed"});
    }
    await this.client.query("UPDATE nodes SET kubernetes_cluster_id=$2,kubernetes_node_name=$3,kubernetes_node_uid=$4,kubernetes_resource_version=$5,lifecycle_state='verifying',trust_state='verifying',version=version+1,updated_at=now() WHERE id=$1",[input.nodeId,input.clusterId,input.nodeName,input.nodeUid,input.resourceVersion]);
    await this.client.query("UPDATE node_join_issuances SET consumed_at=now(),joined_node_uid=$2 WHERE id=$1",[input.issuanceId,input.nodeUid]);
    await this.client.query("UPDATE node_enrollments SET status='consumed',consumed_by_node_id=$2,consumed_at=now(),version=version+1 WHERE id=$1 AND status='exchanged'",[input.enrollmentId,input.nodeId]);
    await this.client.query("UPDATE node_install_plans SET status='accepted',accepted_at=now() WHERE id=$1 AND status='issued'",[input.planId]);
    await this.client.query("INSERT INTO node_audit_events(id,workspace_id,node_id,event_type,payload) VALUES(gen_random_uuid(),$1,$2,'node.join_consumed',$3)",[row.workspace_id,input.nodeId,{issuanceId:input.issuanceId,idempotencyKey:input.idempotencyKey,requestDigest:input.requestDigest}]);
    const node=await this.nodeById(input.nodeId); if(!node) throw new Error("joined node disappeared"); return node;
  }
}

function nodeSelect():string{return `SELECT n.*,i.generation AS identity_generation,i.public_key_fingerprint,i.status AS identity_status,i.issued_at AS identity_issued_at,i.expires_at AS identity_expires_at FROM nodes n LEFT JOIN node_identities i ON i.node_id=n.id AND i.generation=n.current_identity_generation`}
function nodeRow(r:QueryResultRow):NodeView{return {id:r.id,workspaceId:r.workspace_id,name:r.name,kind:r.kind,platform:r.host_platform,architecture:r.host_architecture,lifecycleState:r.lifecycle_state,trustState:r.trust_state,agentEligible:r.agent_eligible,version:Number(r.version),capabilityVersion:r.current_capability_version===null?null:Number(r.current_capability_version),identity:r.identity_generation===null?null:{generation:Number(r.identity_generation),publicKeyFingerprint:`sha256:${r.public_key_fingerprint.trim()}`,status:r.identity_status,issuedAt:r.identity_issued_at.toISOString(),expiresAt:r.identity_expires_at.toISOString()},kubernetesBinding:r.kubernetes_node_uid===null?null:{clusterId:r.kubernetes_cluster_id,nodeName:r.kubernetes_node_name,nodeUid:r.kubernetes_node_uid,resourceVersion:r.kubernetes_resource_version},createdAt:r.created_at.toISOString(),updatedAt:r.updated_at.toISOString()}}
function enrollmentRow(r:QueryResultRow):EnrollmentRecord{return{id:r.id,workspaceId:r.workspace_id,requestedName:r.requested_name,mode:r.mode,expectedPlatform:r.expected_platform,expectedArchitecture:r.expected_architecture,tokenHash:r.token_hash.trim(),tokenKeyId:r.token_key_id,idempotencyKey:r.idempotency_key,createdBy:r.created_by,expiresAt:r.expires_at,status:r.status,machineBinding:r.machine_binding?.trim()??null,nodePublicKey:r.node_public_key,nodePublicKeyFingerprint:r.node_public_key_fingerprint?.trim()??null,consumedByNodeId:r.consumed_by_node_id,version:Number(r.version)}}
function operationRow(r:QueryResultRow):NodeOperationView{return{id:r.id,nodeId:r.node_id,type:r.type,status:r.status,expectedNodeVersion:Number(r.expected_node_version),result:r.result,error:r.error,receipt:null,createdAt:r.created_at.toISOString()}}
