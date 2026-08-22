import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import test from "node:test";
import type { Pool } from "pg";
import { createDatabase } from "./db.js";
import { PgSandboxControllerStore } from "./sandbox-controller-store.js";

const adminUrl = process.env.BLAZN_SANDBOX_CONTROLLER_TEST_ADMIN_DATABASE_URL;
const controllerUrl = process.env.BLAZN_SANDBOX_CONTROLLER_TEST_DATABASE_URL;

test("PostgreSQL sandbox controller claims, fences, retries, completes, and enqueues expiry", { skip: !adminUrl || !controllerUrl }, async () => {
  const admin = createDatabase(adminUrl!), controllerOne = createDatabase(controllerUrl!), controllerTwo = createDatabase(controllerUrl!);
  const first = new PgSandboxControllerStore(controllerOne), second = new PgSandboxControllerStore(controllerTwo);
  const userId = randomUUID(), workspaceId = randomUUID();
  try {
    await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,'Controller Test','salt','hash')", [userId, `${userId}@example.test`]);
    await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,$2,'Controller Test',$3)", [workspaceId, `controller-${userId.slice(0, 8)}`, userId]);
    await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner')", [workspaceId, userId]);

    const staleStateSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "ready" });
    const staleStateOperationId = await insertOperation(admin, workspaceId, staleStateSandboxId, userId, "stop");
    const staleVersionSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped" });
    const staleVersionOperationId = await insertOperation(admin, workspaceId, staleVersionSandboxId, userId, "stop");
    const createSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const createOperationId = await insertOperation(admin, workspaceId, createSandboxId, userId, "create");
    const claims = await Promise.all([first.claim("controller-a", 30), second.claim("controller-b", 30)]);
    const claimed = claims.find((value) => value !== undefined)!;
    assert.equal(claims.filter(Boolean).length, 1, "SKIP LOCKED claim was not exclusive");
    assert.equal(claimed.operationId, createOperationId);
    const quarantined = await admin.query("SELECT o.id,o.status,r.error,s.state,s.version FROM sandbox_operations o JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id JOIN sandboxes s ON s.id=o.sandbox_id WHERE o.id=ANY($1::uuid[]) ORDER BY o.id", [[staleStateOperationId, staleVersionOperationId]]);
    assert.equal(quarantined.rowCount, 2);
    for (const row of quarantined.rows) {
      assert.equal(row.status, "recovery_required");
      assert.equal(row.error.code, "stale_sandbox_operation");
    }
    assert.equal(quarantined.rows.find((row) => row.id === staleStateOperationId)?.state, "ready", "state-mismatched quarantine changed the Sandbox");
    assert.equal(quarantined.rows.find((row) => row.id === staleVersionOperationId)?.state, "stopping", "version-mismatched quarantine changed the Sandbox");
    assert.equal(claimed.attempt, 1);
    assert.equal((await admin.query("SELECT state FROM sandboxes WHERE id=$1", [createSandboxId])).rows[0]?.state, "queued");
    assert.equal(claimed.templateDigest, `sha256:${"a".repeat(64)}`);
    assert.deepEqual(claimed.command, ["/bin/true"]);
    assert.deepEqual(claimed.sources.map((source) => [source.name, source.commit]), [["source", "1".repeat(40)]]);
    assert.equal(await first.renew(createOperationId, "controller-a", randomUUID(), 30), undefined);

    const owner = claims[0] ? first : second;
    const worker = claims[0] ? "controller-a" : "controller-b";
    assert.equal(await owner.bindBackend(createOperationId, worker, claimed.leaseToken, {
      uid: "backend-create", resourceVersion: "resource-create", admissionId: "admission-create",
    }), true);
    const boundVersion = Number((await admin.query("SELECT version FROM sandboxes WHERE id=$1", [createSandboxId])).rows[0]?.version);
    assert.equal(await owner.bindBackend(createOperationId, worker, claimed.leaseToken, {
      uid: "backend-create", resourceVersion: "resource-create", admissionId: "admission-create",
    }), true);
    assert.equal(Number((await admin.query("SELECT version FROM sandboxes WHERE id=$1", [createSandboxId])).rows[0]?.version), boundVersion, "backend bind replay changed Sandbox version");
    assert.equal(await owner.complete(createOperationId, worker, claimed.leaseToken, successCreate("wrong-backend")), false);
    assert.equal(await owner.complete(createOperationId, worker, claimed.leaseToken, successCreate("backend-create")), true);
    assert.equal(await owner.renew(createOperationId, worker, claimed.leaseToken, 30), undefined, "terminal lease renewed");
    const completed = await admin.query("SELECT o.status,s.state,s.backend_uid,s.admission_id,r.result,j.completed_at AS job_completed_at FROM sandbox_operations o JOIN sandboxes s ON s.id=o.sandbox_id JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id JOIN sandbox_reconcile_jobs j ON j.operation_id=o.id WHERE o.id=$1", [createOperationId]);
    assert.equal(completed.rows[0]?.status, "succeeded");
    assert.equal(completed.rows[0]?.state, "ready");
    assert.equal(completed.rows[0]?.backend_uid, "backend-create");
    assert.equal(completed.rows[0]?.admission_id, "admission-create");
    assert.deepEqual(completed.rows[0]?.result, { artifactIds: [], warnings: [] });
    assert.ok(completed.rows[0]?.job_completed_at);

    const staleSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const staleOperationId = await insertOperation(admin, workspaceId, staleSandboxId, userId, "create");
    const stale = await first.claim("controller-stale", 30); assert.equal(stale?.operationId, staleOperationId);
    await admin.query("UPDATE sandbox_reconcile_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1", [staleOperationId]);
    const recovered = await second.claim("controller-recovery", 30); assert.equal(recovered?.operationId, staleOperationId); assert.equal(recovered?.attempt, 2);
    assert.equal(await first.renew(staleOperationId, "controller-stale", stale!.leaseToken, 30), undefined);
    assert.equal(await first.bindBackend(staleOperationId, "controller-stale", stale!.leaseToken, { uid: "stale", resourceVersion: "stale", admissionId: "stale" }), false);
    assert.equal(await second.retry(staleOperationId, "controller-recovery", recovered!.leaseToken, 1,
      { code: "backend_temporarily_unavailable", message: "backend is temporarily unavailable", requestId: randomUUID() }), "retry_scheduled");

    for (let expectedAttempt = 3; expectedAttempt <= 5; expectedAttempt++) {
      await admin.query("UPDATE sandbox_reconcile_jobs SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1", [staleOperationId]);
      const retryClaim = await first.claim("controller-retry", 30); assert.equal(retryClaim?.attempt, expectedAttempt);
      const outcome = await first.retry(staleOperationId, "controller-retry", retryClaim!.leaseToken, 1,
        { code: "backend_temporarily_unavailable", message: "backend is temporarily unavailable", requestId: randomUUID() });
      assert.equal(outcome, expectedAttempt === 5 ? "recovery_required" : "retry_scheduled");
    }
    const recovery = await admin.query("SELECT o.status,s.state,r.error,j.completed_at FROM sandbox_operations o JOIN sandboxes s ON s.id=o.sandbox_id JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id JOIN sandbox_reconcile_jobs j ON j.operation_id=o.id WHERE o.id=$1", [staleOperationId]);
    assert.equal(recovery.rows[0]?.status, "recovery_required"); assert.equal(recovery.rows[0]?.state, "failed");
    assert.equal(recovery.rows[0]?.error.code, "backend_temporarily_unavailable"); assert.ok(recovery.rows[0]?.completed_at);

    const exhaustedSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const exhaustedOperationId = await insertOperation(admin, workspaceId, exhaustedSandboxId, userId, "create");
    const exhausted = await first.claim("controller-exhausted", 30); assert.equal(exhausted?.operationId, exhaustedOperationId);
    await admin.query("UPDATE sandbox_reconcile_jobs SET attempt_count=5,lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1", [exhaustedOperationId]);
    assert.equal(await second.claim("controller-after-exhaustion", 30), undefined);
    const exhaustedState = await admin.query("SELECT o.status,s.state,r.error FROM sandbox_operations o JOIN sandboxes s ON s.id=o.sandbox_id JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id WHERE o.id=$1", [exhaustedOperationId]);
    assert.equal(exhaustedState.rows[0]?.status, "recovery_required"); assert.equal(exhaustedState.rows[0]?.state, "failed");
    assert.equal(exhaustedState.rows[0]?.error.code, "lease_attempts_exhausted");
    assert.equal(await first.complete(exhaustedOperationId, "controller-exhausted", exhausted!.leaseToken, successCreate("never-bound")), false);

    const stopSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped", backend: ["backend-stop", "resource-stop", "admission-stop"] });
    const stopOperationId = await insertOperation(admin, workspaceId, stopSandboxId, userId, "stop");
    const stop = await first.claim("controller-stop", 30); assert.equal(stop?.operationId, stopOperationId);
    const stopCompletion = { status: "succeeded" as const, expectedBackendUid: "backend-stop", expectedBackendResourceVersion: "resource-stop",
      expectedAdmissionId: "admission-stop", cleanupComplete: true, artifactExportComplete: true, grantsRevoked: true,
      backendDestroyed: true, artifactIds: [], warningCodes: [], error: null };
    assert.equal(await first.complete(stopOperationId, "controller-stop", stop!.leaseToken, { ...stopCompletion, expectedAdmissionId: "substituted" }), false);
    assert.equal(await first.complete(stopOperationId, "controller-stop", stop!.leaseToken, stopCompletion), true);
    const stopped = await admin.query("SELECT state,backend_uid,admission_id,stopped_at FROM sandboxes WHERE id=$1", [stopSandboxId]);
    assert.equal(stopped.rows[0]?.state, "stopped"); assert.equal(stopped.rows[0]?.backend_uid, null); assert.equal(stopped.rows[0]?.admission_id, null); assert.ok(stopped.rows[0]?.stopped_at);

    const duplicateSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped" });
    await insertOperation(admin, workspaceId, duplicateSandboxId, userId, "stop");
    await assert.rejects(insertOperation(admin, workspaceId, duplicateSandboxId, userId, "delete"), hasCode("23505"));

    const expiredSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "ready", expired: true, backend: ["backend-expired", "resource-expired", "admission-expired"] });
    const expiryRaces = await Promise.all([first.enqueueExpired(10), second.enqueueExpired(10)]);
    assert.equal(expiryRaces.flat().filter((value) => value.sandboxId === expiredSandboxId).length, 1);
    const expiry = await admin.query("SELECT o.type,o.status,o.idempotency_key,s.state,s.desired_state FROM sandbox_operations o JOIN sandboxes s ON s.id=o.sandbox_id WHERE o.sandbox_id=$1", [expiredSandboxId]);
    assert.equal(expiry.rows[0]?.type, "stop"); assert.equal(expiry.rows[0]?.status, "pending");
    assert.match(expiry.rows[0]?.idempotency_key, /^expiry-/); assert.equal(expiry.rows[0]?.state, "stopping"); assert.equal(expiry.rows[0]?.desired_state, "stopped");

    const eventSequences = await admin.query<{ sequence: string }>("SELECT sequence::text FROM sandbox_events WHERE sandbox_id=$1 ORDER BY sequence", [staleSandboxId]);
    assert.deepEqual(eventSequences.rows.map((row) => Number(row.sequence)), [0, 1, 2, 3, 4, 5, 6, 7, 8]);
    await assert.rejects(controllerOne.query("SELECT * FROM sandbox_reconcile_jobs"), hasCode("42501"));
    await assert.rejects(controllerOne.query("UPDATE sandboxes SET state='deleted' WHERE id=$1", [expiredSandboxId]), hasCode("42501"));
    await assert.rejects(controllerOne.query("INSERT INTO sandbox_events(id,workspace_id,sandbox_id,sequence,type) VALUES($1,$2,$3,999,'unsafe')", [randomUUID(), workspaceId, expiredSandboxId]), hasCode("42501"));
  } finally {
    await admin.query("DELETE FROM workspaces WHERE id=$1", [workspaceId]).catch(() => {});
    await admin.query("DELETE FROM users WHERE id=$1", [userId]).catch(() => {});
    await Promise.all([admin.end(), controllerOne.end(), controllerTwo.end()]);
  }
});

async function seedSandbox(admin: Pool, workspaceId: string, userId: string, options: {
  state: "requested" | "ready" | "stopping"; desiredState?: "ready" | "stopped"; expired?: boolean;
  backend?: [uid: string, resourceVersion: string, admissionId: string];
}): Promise<string> {
  const sandboxId = randomUUID(), templateId = randomUUID(), versionId = randomUUID(), suffix = sandboxId.slice(0, 8);
  const spec = { version: "1", variants: [{ name: "linux-amd64", architecture: "amd64",
    imageIndex: `registry.invalid/poc@sha256:${"b".repeat(64)}`, imageDigest: `registry.invalid/poc@sha256:${"c".repeat(64)}`,
    placementProfile: "poc-linux-amd64-v1", command: ["/bin/true"], resources: { requests: { cpu: "100m", memory: "128Mi", ephemeralStorage: "1Gi" }, limits: { cpu: "500m", memory: "512Mi", ephemeralStorage: "2Gi" } } }],
    repositories: [{ name: "source", url: "https://github.com/blazncloud/blazn.git", destination: "/workspace/src/blazn", writable: true }], artifacts: [] };
  const createdAt = options.expired ? new Date(Date.now() - 120_000) : new Date();
  const expiresAt = options.expired ? new Date(Date.now() - 60_000) : new Date(Date.now() + 600_000);
  await admin.query("BEGIN");
  try {
    await admin.query("INSERT INTO sandbox_templates(id,workspace_id,name,draft_spec,draft_digest,created_by) VALUES($1,$2,$3,$4,$5,$6)", [templateId, workspaceId, `template-${suffix}`, spec, "a".repeat(64), userId]);
    await admin.query("INSERT INTO sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by) VALUES($1,$2,$3,'1',$4,$5,$6,$7)", [versionId, workspaceId, templateId, Buffer.from(JSON.stringify(spec)), spec, "a".repeat(64), userId]);
    await admin.query("INSERT INTO sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile,command,resources) VALUES($1,$2,$3,'linux-amd64','amd64',$4,$5,'poc-linux-amd64-v1',$6::jsonb,$7::jsonb)", [versionId, workspaceId, templateId, `registry.invalid/poc@sha256:${"b".repeat(64)}`, `registry.invalid/poc@sha256:${"c".repeat(64)}`, JSON.stringify(["/bin/true"]), JSON.stringify(spec.variants[0]!.resources)]);
    await admin.query("INSERT INTO sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable) VALUES($1,$2,$3,'source','https://github.com/blazncloud/blazn.git','/workspace/src/blazn',true)", [versionId, workspaceId, templateId]);
    await admin.query("INSERT INTO sandbox_template_version_status(version_id,workspace_id,template_id,status,changed_by) VALUES($1,$2,$3,'published',$4)", [versionId, workspaceId, templateId, userId]);
    await admin.query("UPDATE sandbox_templates SET current_published_version_id=$1 WHERE id=$2", [versionId, templateId]);
    await admin.query(`INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,
      variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,backend_uid,backend_resource_version,
      queue_name,admission_id,artifact_contract_digest,isolation,approved_non_sensitive,expires_at,created_at,updated_at)
      VALUES($1,$2,$3,$4,$5,$6,'1',$7,'linux-amd64',$8,$9,'amd64','direct',$10,$11,$12,$13,'blazn-poc-sandboxes',$14,$15,
      'approved-non-sensitive-poc',true,$16,$17,$17)`, [sandboxId, workspaceId, userId, templateId, versionId, `template-${suffix}`, "a".repeat(64),
      `registry.invalid/poc@sha256:${"b".repeat(64)}`, `registry.invalid/poc@sha256:${"c".repeat(64)}`, options.state,
      options.desiredState ?? "ready", options.backend?.[0] ?? null, options.backend?.[1] ?? null, options.backend?.[2] ?? null,
      "d".repeat(64), expiresAt, createdAt]);
    await admin.query("INSERT INTO sandbox_sources(sandbox_id,workspace_id,template_version_id,repository_name,commit) VALUES($1,$2,$3,'source',$4)", [sandboxId, workspaceId, versionId, "1".repeat(40)]);
    await admin.query("COMMIT"); return sandboxId;
  } catch (error) { await admin.query("ROLLBACK"); throw error; }
}

async function insertOperation(admin: Pool, workspaceId: string, sandboxId: string, userId: string, type: "create" | "stop" | "delete"): Promise<string> {
  const id = randomUUID();
  await admin.query("INSERT INTO sandbox_operations(id,workspace_id,sandbox_id,type,status,expected_sandbox_version,requested_by,idempotency_key,request_digest) SELECT $1,$2,$3,$4,'pending',version,$5,$6,$7 FROM sandboxes WHERE id=$3", [id, workspaceId, sandboxId, type, userId, `${type}-${id}`, "e".repeat(64)]);
  return id;
}

function successCreate(uid: string) {
  return { status: "succeeded" as const, expectedBackendUid: uid, expectedBackendResourceVersion: "resource-create",
    expectedAdmissionId: "admission-create", cleanupComplete: false, artifactExportComplete: false, grantsRevoked: false,
    backendDestroyed: false, artifactIds: [], warningCodes: [], error: null };
}

function hasCode(code: string): (error: unknown) => boolean {
  return (error) => !!error && typeof error === "object" && "code" in error && error.code === code;
}
