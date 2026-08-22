import assert from "node:assert/strict";
import { generateKeyPairSync, randomUUID, sign } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { createDatabase } from "./db.js";
import { canonicalJson, publicKeyFingerprint } from "./node-crypto.js";
import { FileNodePlanSigner } from "./node-crypto.js";
import { TemplateNodePlanFactory } from "./node-plan.js";
import { NodeBrokerService } from "./node-broker-service.js";
import { PgNodeBrokerStore } from "./node-broker-store.js";
import type {
  JoinCredentialRequest,
  WorkerCredentialIssuer,
} from "./node-broker-types.js";
import type { EnrollmentRecord } from "./node-types.js";
import { NodeHttpError } from "./node-types.js";

const adminUrl = process.env.NODE_TEST_ADMIN_DATABASE_URL,
  brokerUrl = process.env.NODE_TEST_BROKER_DATABASE_URL,
  repoRoot = process.env.NODE_TEST_REPO_ROOT;

test(
  "PostgreSQL broker serializes issuance, replays ciphertext, and stores no plaintext",
  { skip: !adminUrl || !brokerUrl || !repoRoot },
  async () => {
    const admin = createDatabase(adminUrl!),
      broker = createDatabase(brokerUrl!),
      root = await mkdtemp(path.join(os.tmpdir(), "blazn-broker-pg-")),
      userId = randomUUID(),
      workspaceId = randomUUID(),
      enrollmentId = randomUUID(),
      planId = randomUUID(),
      nodeId = randomUUID(),
      identityPair = generateKeyPairSync("ed25519"),
      identityPublicKey = identityPair.publicKey.export({ format: "jwk" }).x!,
      identityFingerprint = `sha256:${publicKeyFingerprint(identityPublicKey)}`,
      machineFingerprint = "a".repeat(64),
      planPair = generateKeyPairSync("ed25519"),
      der = planPair.privateKey.export({ format: "der", type: "pkcs8" }),
      planKeyFile = path.join(root, "plan-seed");
    try {
      await writeFile(
        planKeyFile,
        `${der.subarray(der.length - 32).toString("base64url")}\n`,
        { mode: 0o600 },
      );
      const planSigner = new FileNodePlanSigner(
          "control-plane-node-plan/v1",
          planKeyFile,
        ),
        planSigningKey = await planSigner.publicKey(),
        enrollment: EnrollmentRecord = {
          id: enrollmentId,
          workspaceId,
          requestedName: "worker-pg",
          mode: "fresh",
          expectedPlatform: "linux",
          expectedArchitecture: "amd64",
          tokenHash: "b".repeat(64),
          tokenKeyId: "node-enrollment/v1",
          idempotencyKey: "enroll-pg-key",
          createdBy: userId,
          planSigningKey,
          expiresAt: new Date("2030-01-01T00:15:00.000Z"),
          status: "exchanged",
          machineBinding: machineFingerprint,
          nodePublicKey: identityPublicKey,
          nodePublicKeyFingerprint: identityFingerprint.slice(7),
          consumedByNodeId: null,
          version: 2,
        },
        factory = new TemplateNodePlanFactory(
          path.join(
            repoRoot!,
            "infra/node/templates/node-install-plan-template-v1.json",
          ),
          planSigner,
        ),
        plan = await factory.create({
          planId,
          nodeId,
          enrollment,
          architecture: "amd64",
          machineFingerprint,
          nodePublicKeyFingerprint: identityFingerprint.slice(7),
          issuedAt: new Date("2029-01-01T00:00:00.000Z"),
          expiresAt: new Date("2029-01-01T00:15:00.000Z"),
        });
      await admin.query(
        "INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,'Broker PG','salt','hash')",
        [userId, `broker-${userId}@example.test`],
      );
      await admin.query(
        "INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,$2,'Broker PG',$3)",
        [workspaceId, `broker-${userId.slice(0, 8)}`, userId],
      );
      await admin.query(
        `INSERT INTO node_enrollments(id,workspace_id,requested_name,mode,expected_platform,expected_architecture,token_hash,token_key_id,idempotency_key,created_by,expires_at,plan_signing_key_id,plan_signing_public_key,plan_signing_key_fingerprint,status,machine_binding,node_public_key,node_public_key_fingerprint,exchanged_at)
      VALUES($1,$2,'worker-pg','fresh','linux','amd64',$3,'node-enrollment/v1','enroll-pg-key',$4,$5,$6,$7,$8,'exchanged',$9,$10,$11,now())`,
        [
          enrollmentId,
          workspaceId,
          "b".repeat(64),
          userId,
          new Date("2030-01-01T00:15:00.000Z"),
          planSigningKey.keyId,
          planSigningKey.publicKey,
          planSigningKey.fingerprint.slice(7),
          machineFingerprint,
          identityPublicKey,
          identityFingerprint.slice(7),
        ],
      );
      await admin.query(
        `INSERT INTO nodes(id,workspace_id,name,kind,owner_user_id,machine_fingerprint,host_platform,host_architecture,lifecycle_state,trust_state,agent_eligible,service_version) VALUES($1,$2,'worker-pg','shared',$3,$4,'linux','amd64','installing','verifying',false,'pending')`,
        [nodeId, workspaceId, userId, machineFingerprint],
      );
      await admin.query(
        `INSERT INTO node_install_plans(id,workspace_id,node_id,enrollment_id,approved_by,idempotency_key,plan_digest,signing_key_id,signature,canonical_plan,issued_at,expires_at,status) VALUES($1,$2,$3,$4,$5,'enroll-pg-key',$6,$7,$8,$9,$10,$11,'issued')`,
        [
          planId,
          workspaceId,
          nodeId,
          enrollmentId,
          userId,
          String(plan.digest).slice(7),
          String(plan.signingKeyId),
          String(plan.signature),
          plan,
          new Date(String(plan.issuedAt)),
          new Date(String(plan.expiresAt)),
        ],
      );
      const request: JoinCredentialRequest = {
          enrollmentId,
          planId,
          planDigest: String(plan.digest),
          nodeId,
          machineFingerprint,
          nodePublicKeyFingerprint: identityFingerprint,
        },
        proof = sign(
          null,
          Buffer.from(`blazn-node-join-v1\n${canonicalJson(request)}`),
          identityPair.privateKey,
        ).toString("base64url");
      let issues = 0;
      const credential = "z".repeat(43),
        issuer: WorkerCredentialIssuer = {
          issue: async (input) => {
            issues++;
            return {
              providerHandle: input.issuanceId,
              credential,
              clusterId: input.clusterId,
              clusterHealthy: true,
              workerOnly: true,
              expiresAt: new Date(Date.now() + Math.min(240, input.ttlSeconds) * 1000),
            };
          },
          revoke: async () => {},
        },
        service = new NodeBrokerService(
          new PgNodeBrokerStore(broker),
          async () => Buffer.alloc(32, 7),
          issuer,
          1_000,
        );
      const race = await Promise.all([
        service.issue("broker-join-key", request, proof),
        service.issue("broker-join-key", request, proof),
      ]);
      assert.equal(issues, 1);
      assert.equal(race[0].credential, credential);
      assert.equal(race[1].credential, credential);
      assert.equal(race.filter((v) => v.replayed).length, 1);
      const rows = await admin.query<{
        plaintext: boolean;
        key_id: string;
        ciphertext_length: number;
      }>(
        "SELECT position(convert_to($1,'UTF8') in credential_ciphertext)>0 AS plaintext,credential_key_id AS key_id,octet_length(credential_ciphertext) AS ciphertext_length FROM node_join_issuances WHERE node_id=$2",
        [credential, nodeId],
      );
      assert.equal(rows.rows[0]?.plaintext, false);
      assert.equal(rows.rows[0]?.key_id, "node-join-credential/v1");
      assert.ok(Number(rows.rows[0]?.ciphertext_length) > credential.length);
      await assert.rejects(
        () => service.issue("different-key", request, proof),
        (e: unknown) =>
          e instanceof NodeHttpError && e.code === "idempotency_conflict",
      );
      const audit = await admin.query<{ payload: string }>(
        "SELECT payload::text FROM node_audit_events WHERE workspace_id=$1",
        [workspaceId],
      );
      assert.equal(
        audit.rows.some((r) => r.payload.includes(credential)),
        false,
      );
    } finally {
      await admin
        .query("DELETE FROM workspaces WHERE id=$1", [workspaceId])
        .catch(() => {});
      await admin
        .query("DELETE FROM users WHERE id=$1", [userId])
        .catch(() => {});
      await broker.end();
      await admin.end();
      await rm(root, { recursive: true });
    }
  },
);
