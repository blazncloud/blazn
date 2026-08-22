import type { PoolClient, QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { BrokerBinding, JoinCredentialRequest, StoredJoinIssuance } from "./node-broker-types.js";

export interface NewJoinIssuance extends StoredJoinIssuance { credentialHash: string }

export interface NodeBrokerTransaction {
  lockNode(nodeId:string):Promise<void>;
  binding(request:JoinCredentialRequest):Promise<BrokerBinding|undefined>;
  issuance(request:JoinCredentialRequest):Promise<StoredJoinIssuance|undefined>;
  insertIssuance(value:NewJoinIssuance):Promise<void>;
}

export interface NodeBrokerStore { transaction<T>(action:(tx:NodeBrokerTransaction)=>Promise<T>):Promise<T> }

export class PgNodeBrokerStore implements NodeBrokerStore {
  constructor(private readonly database:Database){}
  async transaction<T>(action:(tx:NodeBrokerTransaction)=>Promise<T>):Promise<T>{const client=await this.database.connect();try{await client.query("BEGIN");const result=await action(new PgNodeBrokerTransaction(client));await client.query("COMMIT");return result;}catch(error){await client.query("ROLLBACK");throw error;}finally{client.release();}}
}

class PgNodeBrokerTransaction implements NodeBrokerTransaction {
  constructor(private readonly client:PoolClient){}
  async lockNode(nodeId:string):Promise<void>{await this.client.query("SELECT pg_advisory_xact_lock(hashtextextended($1,0))",[`blazn-node-join-v1\n${nodeId}`]);}
  async binding(request:JoinCredentialRequest):Promise<BrokerBinding|undefined>{const result=await this.client.query(`SELECT clock_timestamp() AS database_now,e.workspace_id,e.id AS enrollment_id,e.status AS enrollment_status,e.expires_at AS enrollment_expires_at,e.created_by AS enrollment_created_by,e.node_public_key,e.node_public_key_fingerprint,e.machine_binding,
      e.plan_signing_key_id,e.plan_signing_public_key,p.id AS plan_id,p.plan_digest,p.status AS plan_status,p.approved_by AS plan_approved_by,p.expires_at AS plan_expires_at,p.canonical_plan,n.id AS node_id,n.machine_fingerprint AS node_machine_fingerprint,n.lifecycle_state,n.trust_state
      FROM node_enrollments e JOIN node_install_plans p ON p.enrollment_id=e.id AND p.workspace_id=e.workspace_id
      JOIN nodes n ON n.id=p.node_id AND n.workspace_id=p.workspace_id WHERE e.id=$1 AND p.id=$2 AND n.id=$3`,[request.enrollmentId,request.planId,request.nodeId]);const r=result.rows[0];return r?bindingRow(r):undefined;}
  async issuance(request:JoinCredentialRequest):Promise<StoredJoinIssuance|undefined>{const result=await this.client.query(`SELECT j.*,p.canonical_plan->'cluster'->>'id' AS cluster_id FROM node_join_issuances j JOIN node_install_plans p ON p.id=j.plan_id WHERE j.enrollment_id=$1 OR j.plan_id=$2 OR j.node_id=$3 ORDER BY j.issued_at LIMIT 2`,[request.enrollmentId,request.planId,request.nodeId]);if(result.rows.length>1)throw new Error("multiple join issuances exist for one Node binding");return result.rows[0]?issuanceRow(result.rows[0]):undefined;}
  async insertIssuance(v:NewJoinIssuance):Promise<void>{await this.client.query(`INSERT INTO node_join_issuances(id,workspace_id,enrollment_id,plan_id,node_id,node_public_key_fingerprint,machine_fingerprint,credential_hash,credential_ciphertext,credential_key_id,idempotency_key,request_digest,issued_at,expires_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,[v.id,v.workspaceId,v.enrollmentId,v.planId,v.nodeId,v.nodePublicKeyFingerprint.slice(7),v.machineFingerprint,v.credentialHash,v.credentialCiphertext,v.credentialKeyId,v.idempotencyKey,v.requestDigest,v.issuedAt,v.expiresAt]);}
}

function bindingRow(r:QueryResultRow):BrokerBinding{return{databaseNow:r.database_now,workspaceId:r.workspace_id,enrollmentId:r.enrollment_id,enrollmentStatus:r.enrollment_status,enrollmentExpiresAt:r.enrollment_expires_at,enrollmentCreatedBy:r.enrollment_created_by,nodePublicKey:r.node_public_key,nodePublicKeyFingerprint:`sha256:${r.node_public_key_fingerprint.trim()}`,machineFingerprint:r.machine_binding?.trim()??"",nodeId:r.node_id,nodeMachineFingerprint:r.node_machine_fingerprint.trim(),nodeLifecycleState:r.lifecycle_state,nodeTrustState:r.trust_state,planId:r.plan_id,planDigest:`sha256:${r.plan_digest.trim()}`,planStatus:r.plan_status,planApprovedBy:r.plan_approved_by,planExpiresAt:r.plan_expires_at,canonicalPlan:r.canonical_plan,planSigningKeyId:r.plan_signing_key_id,planSigningPublicKey:r.plan_signing_public_key.trim()};}
function issuanceRow(r:QueryResultRow):StoredJoinIssuance{return{id:r.id,workspaceId:r.workspace_id,enrollmentId:r.enrollment_id,planId:r.plan_id,nodeId:r.node_id,clusterId:r.cluster_id,machineFingerprint:r.machine_fingerprint.trim(),nodePublicKeyFingerprint:`sha256:${r.node_public_key_fingerprint.trim()}`,credentialCiphertext:r.credential_ciphertext,credentialKeyId:r.credential_key_id,idempotencyKey:r.idempotency_key,requestDigest:r.request_digest.trim(),issuedAt:r.issued_at,expiresAt:r.expires_at,consumedAt:r.consumed_at,revokedAt:r.revoked_at};}
