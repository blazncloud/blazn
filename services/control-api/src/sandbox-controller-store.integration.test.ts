import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import test from "node:test";
import type { Pool } from "pg";
import { createDatabase } from "./db.js";
import { PgSandboxControllerStore, type SandboxControllerAdmissionIdentity,
  type SandboxControllerAdmissionObservation, type SandboxControllerSource,
  type SandboxControllerSourceReceipt } from "./sandbox-controller-store.js";

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
    await assert.rejects(controllerOne.query("SELECT * FROM sandbox_controller_claim($1,$2)", ["retired-controller", 30]), hasCode("42501"));
    for (const role of ["blazn_runtime", "blazn_bootstrap", "blazn_node_broker"]) {
      const privilege = await admin.query<{ allowed: boolean }>(`SELECT bool_or(has_function_privilege($1,p.oid,'EXECUTE')) AS allowed
        FROM pg_proc p WHERE p.proname IN ('sandbox_controller_claim_v2','sandbox_controller_bind_backend_v2','sandbox_controller_complete_v2',
          'sandbox_controller_claim_v3','sandbox_controller_bind_backend_v3','sandbox_controller_complete_v3',
          'sandbox_controller_claim_v4','sandbox_controller_bind_backend_v4','sandbox_controller_complete_v4',
          'sandbox_controller_record_source_materialization_v1','sandbox_controller_claim_v5',
          'sandbox_controller_record_artifact_v1','sandbox_controller_complete_artifact_export_v1','sandbox_controller_complete_v5',
          'sandbox_controller_finalize_stopped_delete_v1')`, [role]);
      assert.equal(privilege.rows[0]?.allowed, false, `${role} can execute a controller function`);
    }
    const publicPrivilege = await admin.query<{ count: string }>(`SELECT count(*)::text AS count FROM pg_proc p,
      LATERAL aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) acl
      WHERE p.proname IN ('sandbox_controller_claim_v2','sandbox_controller_bind_backend_v2','sandbox_controller_complete_v2',
        'sandbox_controller_claim_v3','sandbox_controller_bind_backend_v3','sandbox_controller_complete_v3',
        'sandbox_controller_claim_v4','sandbox_controller_bind_backend_v4','sandbox_controller_complete_v4',
        'sandbox_controller_record_source_materialization_v1','sandbox_controller_claim_v5',
        'sandbox_controller_record_artifact_v1','sandbox_controller_complete_artifact_export_v1','sandbox_controller_complete_v5',
          'sandbox_controller_finalize_stopped_delete_v1')
        AND acl.grantee=0 AND acl.privilege_type='EXECUTE'`);
    assert.equal(publicPrivilege.rows[0]?.count, "0", "PUBLIC can execute a controller v2 function");
    for (const signature of [
      "sandbox_controller_claim_v2(text,integer)",
      "sandbox_controller_bind_backend_v2(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text)",
      "sandbox_controller_complete_v2(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)",
      "sandbox_controller_claim_v3(text,integer)",
      "sandbox_controller_bind_backend_v3(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text)",
      "sandbox_controller_complete_v3(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)",
      "sandbox_controller_claim_v4(text,integer)",
      "sandbox_controller_complete_v4(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)",
    ]) {
      const retired = await admin.query<{ allowed: boolean }>("SELECT has_function_privilege('blazn_sandbox_controller',$1,'EXECUTE') AS allowed", [signature]);
      assert.equal(retired.rows[0]?.allowed, false, `controller retained v2 authority ${signature}`);
    }
    for (const signature of [
      "sandbox_controller_bind_backend(uuid,text,uuid,text,text,text)",
      "sandbox_controller_complete(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)",
    ]) {
      const legacy = await admin.query<{ allowed: boolean }>("SELECT has_function_privilege('blazn_sandbox_controller',$1,'EXECUTE') AS allowed", [signature]);
      assert.equal(legacy.rows[0]?.allowed, false, `controller retained scalar authority ${signature}`);
    }

    const staleStateSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "ready" });
    const staleStateOperationId = await insertOperation(admin, workspaceId, staleStateSandboxId, userId, "stop");
    const staleVersionSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped" });
    const staleVersionOperationId = await insertOperation(admin, workspaceId, staleVersionSandboxId, userId, "stop");
    const createSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested", withArtifact: true });
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
    assert.equal(claimed.requestedBy, userId);
    assert.deepEqual(claimed.command, ["/bin/true"]);
    assert.deepEqual(claimed.sources.map((source) => [source.name, source.commit]), [["source", "1".repeat(40)]]);
    assert.deepEqual(claimed.artifacts, [
      { name: "logs", path: "/workspace/artifacts/run.log", mediaType: "text/plain", required: false },
      { name: "patch", path: "/workspace/artifacts/change.patch", mediaType: "text/plain", required: true },
    ]);
    assert.equal(await first.renew(createOperationId, "controller-a", randomUUID(), 30), undefined);

    const owner = claims[0] ? first : second;
    const worker = claims[0] ? "controller-a" : "controller-b";
    const bootstrapObservation = admissionObservation(workspaceId, createSandboxId, "backend-create", "resource-bootstrap", "workload-create");
    const receipt = sourceReceipt(claimed.sources);
    assert.equal(await owner.recordSources(createOperationId, worker, claimed.leaseToken, bootstrapObservation, receipt), true);
    assert.equal(await owner.recordSources(createOperationId, worker, claimed.leaseToken, bootstrapObservation, receipt), true, "source receipt replay failed");
    const createObservation = admissionObservation(workspaceId, createSandboxId, "backend-create", "resource-create", "workload-create");
    for (const substitute of [
      { ...createObservation, workload: { ...createObservation.workload, owner: { ...createObservation.workload.owner, controller: false as true } } },
      { ...createObservation, workload: { ...createObservation.workload, admitted: false as true } },
      { ...createObservation, workload: { ...createObservation.workload, clusterQueue: claimed.queueName } },
      { ...createObservation, workload: { ...createObservation.workload, workspaceId: randomUUID() } },
    ]) await assert.rejects(owner.bindBackend(createOperationId, worker, claimed.leaseToken, substitute));
    assert.equal(await owner.bindBackend(createOperationId, worker, claimed.leaseToken, createObservation), true);
    const storedAdmission = await admin.query("SELECT * FROM sandbox_workload_admissions WHERE sandbox_id=$1", [createSandboxId]);
    assert.equal(storedAdmission.rows[0]?.admission_digest.trim(), createObservation.workload.digest.slice(7));
    assert.equal(storedAdmission.rows[0]?.observation_digest.trim(), createObservation.digest.slice(7));
    assert.equal(storedAdmission.rows[0]?.pod_uid, createObservation.pod.uid);
    assert.equal(storedAdmission.rows[0]?.owner_controller, true);
    assert.equal(storedAdmission.rows[0]?.admitted, true);
    assert.equal(storedAdmission.rows[0]?.condition_status, "True");
    const boundVersion = Number((await admin.query("SELECT version FROM sandboxes WHERE id=$1", [createSandboxId])).rows[0]?.version);
    assert.equal(await owner.bindBackend(createOperationId, worker, claimed.leaseToken, createObservation), true);
    assert.equal(Number((await admin.query("SELECT version FROM sandboxes WHERE id=$1", [createSandboxId])).rows[0]?.version), boundVersion, "backend bind replay changed Sandbox version");
    assert.equal(await owner.complete(createOperationId, worker, claimed.leaseToken, successCreate("wrong-backend", createObservation)), false);
    assert.equal(await owner.complete(createOperationId, worker, claimed.leaseToken,
      { ...successCreate("backend-create", createObservation), expectedObservationDigest: `sha256:${"0".repeat(64)}` }), false);
    assert.equal(await owner.complete(createOperationId, worker, claimed.leaseToken, successCreate("backend-create", createObservation)), true);
    assert.equal(await owner.renew(createOperationId, worker, claimed.leaseToken, 30), undefined, "terminal lease renewed");
    const completed = await admin.query("SELECT o.status,s.state,s.backend_uid,s.admission_id,r.result,r.admission_digest,j.completed_at AS job_completed_at FROM sandbox_operations o JOIN sandboxes s ON s.id=o.sandbox_id JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id JOIN sandbox_reconcile_jobs j ON j.operation_id=o.id WHERE o.id=$1", [createOperationId]);
    assert.equal(completed.rows[0]?.status, "succeeded");
    assert.equal(completed.rows[0]?.state, "ready");
    assert.equal(completed.rows[0]?.backend_uid, "backend-create");
    assert.equal(completed.rows[0]?.admission_id, "workload-create");
    assert.equal(completed.rows[0]?.admission_digest.trim(), createObservation.workload.digest.slice(7));
    assert.deepEqual(completed.rows[0]?.result, { artifactIds: [], warnings: [] });
    assert.ok(completed.rows[0]?.job_completed_at);

    const renewDriftSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const renewDriftOperationId = await insertOperation(admin, workspaceId, renewDriftSandboxId, userId, "create");
    const renewDrift = await first.claim("controller-renew-drift", 30); assert.equal(renewDrift?.operationId, renewDriftOperationId);
    await admin.query("UPDATE sandboxes SET version=version+1 WHERE id=$1", [renewDriftSandboxId]);
    const renewSnapshot = await sandboxSnapshot(admin, renewDriftSandboxId);
    assert.equal(await first.renew(renewDriftOperationId, "controller-renew-drift", renewDrift!.leaseToken, 30), undefined);
    await assertStaleQuarantine(admin, renewDriftOperationId, renewDriftSandboxId, renewSnapshot);

    const bindDriftSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const bindDriftOperationId = await insertOperation(admin, workspaceId, bindDriftSandboxId, userId, "create");
    const bindDrift = await first.claim("controller-bind-drift", 30); assert.equal(bindDrift?.operationId, bindDriftOperationId);
    const bindDriftObservation = admissionObservation(workspaceId, bindDriftSandboxId, "must-not-bind", "must-not-bind", "must-not-bind");
    assert.equal(await first.recordSources(bindDriftOperationId, "controller-bind-drift", bindDrift!.leaseToken,
      bindDriftObservation, sourceReceipt(bindDrift!.sources)), true);
    await admin.query("UPDATE sandboxes SET desired_state='deleted' WHERE id=$1", [bindDriftSandboxId]);
    const bindSnapshot = await sandboxSnapshot(admin, bindDriftSandboxId);
    assert.equal(await first.bindBackend(bindDriftOperationId, "controller-bind-drift", bindDrift!.leaseToken,
      bindDriftObservation), false);
    await assertStaleQuarantine(admin, bindDriftOperationId, bindDriftSandboxId, bindSnapshot);

    const retryDriftSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const retryDriftOperationId = await insertOperation(admin, workspaceId, retryDriftSandboxId, userId, "create");
    const retryDrift = await first.claim("controller-retry-drift", 30); assert.equal(retryDrift?.operationId, retryDriftOperationId);
    await admin.query("UPDATE sandboxes SET version=version+1 WHERE id=$1", [retryDriftSandboxId]);
    const retrySnapshot = await sandboxSnapshot(admin, retryDriftSandboxId);
    assert.equal(await first.retry(retryDriftOperationId, "controller-retry-drift", retryDrift!.leaseToken, 1,
      { code: "backend_temporarily_unavailable", message: "backend is temporarily unavailable", requestId: randomUUID() }), "fenced");
    await assertStaleQuarantine(admin, retryDriftOperationId, retryDriftSandboxId, retrySnapshot);

    const exhaustedDriftSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const exhaustedDriftOperationId = await insertOperation(admin, workspaceId, exhaustedDriftSandboxId, userId, "create");
    const exhaustedDrift = await first.claim("controller-exhausted-drift", 30); assert.equal(exhaustedDrift?.operationId, exhaustedDriftOperationId);
    await admin.query("UPDATE sandbox_reconcile_jobs SET attempt_count=5 WHERE operation_id=$1", [exhaustedDriftOperationId]);
    await admin.query("UPDATE sandboxes SET version=version+1 WHERE id=$1", [exhaustedDriftSandboxId]);
    const exhaustedDriftSnapshot = await sandboxSnapshot(admin, exhaustedDriftSandboxId);
    assert.equal(await first.retry(exhaustedDriftOperationId, "controller-exhausted-drift", exhaustedDrift!.leaseToken, 1,
      { code: "backend_temporarily_unavailable", message: "backend is temporarily unavailable", requestId: randomUUID() }), "fenced");
    await assertStaleQuarantine(admin, exhaustedDriftOperationId, exhaustedDriftSandboxId, exhaustedDriftSnapshot);

    const completeDriftSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const completeDriftOperationId = await insertOperation(admin, workspaceId, completeDriftSandboxId, userId, "create");
    const completeDrift = await first.claim("controller-complete-drift", 30); assert.equal(completeDrift?.operationId, completeDriftOperationId);
    const completeDriftObservation = admissionObservation(workspaceId, completeDriftSandboxId,
      "backend-complete-drift", "resource-complete-drift", "workload-complete-drift");
    assert.equal(await first.recordSources(completeDriftOperationId, "controller-complete-drift", completeDrift!.leaseToken,
      completeDriftObservation, sourceReceipt(completeDrift!.sources)), true);
    assert.equal(await first.bindBackend(completeDriftOperationId, "controller-complete-drift", completeDrift!.leaseToken,
      completeDriftObservation), true);
    await admin.query("UPDATE sandboxes SET version=version+1 WHERE id=$1", [completeDriftSandboxId]);
    const completeSnapshot = await sandboxSnapshot(admin, completeDriftSandboxId);
    assert.equal(await first.complete(completeDriftOperationId, "controller-complete-drift", completeDrift!.leaseToken,
      { ...successCreate("backend-complete-drift", completeDriftObservation), expectedBackendResourceVersion: "resource-complete-drift",
        expectedObservationDigest: `sha256:${"0".repeat(64)}` }), false);
    assert.equal(await first.complete(completeDriftOperationId, "controller-complete-drift", completeDrift!.leaseToken,
      { ...successCreate("backend-complete-drift", completeDriftObservation), expectedBackendResourceVersion: "resource-complete-drift" }), false);
    await assertStaleQuarantine(admin, completeDriftOperationId, completeDriftSandboxId, completeSnapshot);

    const staleSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    const staleOperationId = await insertOperation(admin, workspaceId, staleSandboxId, userId, "create");
    const stale = await first.claim("controller-stale", 30); assert.equal(stale?.operationId, staleOperationId);
    await admin.query("UPDATE sandbox_reconcile_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1", [staleOperationId]);
    const recovered = await second.claim("controller-recovery", 30); assert.equal(recovered?.operationId, staleOperationId); assert.equal(recovered?.attempt, 2);
    assert.equal(await first.renew(staleOperationId, "controller-stale", stale!.leaseToken, 30), undefined);
    assert.equal(await first.bindBackend(staleOperationId, "controller-stale", stale!.leaseToken,
      admissionObservation(workspaceId, staleSandboxId, "stale", "stale", "workload-stale")), false);
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
    assert.equal(await first.complete(exhaustedOperationId, "controller-exhausted", exhausted!.leaseToken, successCreate("never-bound", null)), false);

    const stopSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped", version: 2, backend: ["backend-stop", "resource-stop", "admission-stop"], withArtifact: true });
    const stopOperationId = await insertOperation(admin, workspaceId, stopSandboxId, userId, "stop", 1);
    const stop = await first.claim("controller-stop", 30); assert.equal(stop?.operationId, stopOperationId);
    assert.ok(stop?.admissionObservation);
    const artifactDigest=`sha256:${"d".repeat(64)}`,artifactKey=`workspaces/${workspaceId}/sandboxes/${stopSandboxId}/artifacts/patch`;
    const artifact={name:"patch",path:"/workspace/artifacts/change.patch",mediaType:"text/plain",digest:artifactDigest,size:6,objectKey:artifactKey};
    const artifactId=await first.recordArtifact(stopOperationId,"controller-stop",stop!.leaseToken,stop!.admissionObservation!,artifact);
    assert.match(artifactId??"",/^[0-9a-f-]{36}$/);
    assert.equal(await first.recordArtifact(stopOperationId,"controller-stop",stop!.leaseToken,stop!.admissionObservation!,artifact),artifactId);
    assert.equal(await first.recordArtifact(stopOperationId,"controller-stop",stop!.leaseToken,stop!.admissionObservation!,{...artifact,digest:`sha256:${"e".repeat(64)}`}),undefined);
    assert.equal(await first.completeArtifactExport(stopOperationId,"controller-stop",stop!.leaseToken,stop!.admissionObservation!,[]),false,
      "unaccounted optional artifact completed the phase");
    const artifactWarnings=["optional_artifact_missing_logs"];
    assert.equal(await first.completeArtifactExport(stopOperationId,"controller-stop",stop!.leaseToken,stop!.admissionObservation!,artifactWarnings),true);
    assert.equal(await first.completeArtifactExport(stopOperationId,"controller-stop",stop!.leaseToken,stop!.admissionObservation!,artifactWarnings),true,"artifact phase replay failed");
    const stopCompletion = { status: "succeeded" as const, expectedBackendUid: "backend-stop", expectedBackendResourceVersion: "resource-stop",
      expectedWorkloadDigest: stop!.admissionObservation!.workload.digest,
      expectedObservationDigest: stop!.admissionObservation!.digest, cleanupComplete: true, artifactExportComplete: true, grantsRevoked: true,
      backendDestroyed: true, artifactIds: [artifactId!], warningCodes: artifactWarnings, error: null };
    assert.equal(await first.complete(stopOperationId, "controller-stop", stop!.leaseToken,
      { ...stopCompletion, expectedObservationDigest: `sha256:${"0".repeat(64)}` }), false);
    assert.equal(await first.complete(stopOperationId, "controller-stop", stop!.leaseToken, stopCompletion), true);
    const stopped = await admin.query("SELECT state,backend_uid,admission_id,stopped_at FROM sandboxes WHERE id=$1", [stopSandboxId]);
    assert.equal(stopped.rows[0]?.state, "stopped"); assert.equal(stopped.rows[0]?.backend_uid, null); assert.equal(stopped.rows[0]?.admission_id, null); assert.ok(stopped.rows[0]?.stopped_at);

    // A stopped Sandbox has no live IDs, but retains its immutable admission.
    // Deletion must consume the prior cleanup proof without returning a live
    // work item that the Go controller would reject as inconsistent.
    await admin.query("UPDATE sandboxes SET state='deleting',desired_state='deleted',version=version+1 WHERE id=$1", [stopSandboxId]);
    const stoppedDeleteId = await insertOperation(admin, workspaceId, stopSandboxId, userId, "delete", 3);
    assert.equal(await first.claim("controller-delete-stopped", 30), undefined);
    const deletedAfterStop = await admin.query(`SELECT s.state,r.status,r.cleanup_complete,r.artifact_export_complete,
      r.grants_revoked,r.backend_destroyed,r.result,r.admission_digest
      FROM sandboxes s JOIN sandbox_operations o ON o.sandbox_id=s.id
      JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id WHERE o.id=$1`, [stoppedDeleteId]);
    assert.deepEqual(deletedAfterStop.rows[0], { state: "deleted", status: "succeeded", cleanup_complete: true,
      artifact_export_complete: true, grants_revoked: true, backend_destroyed: true,
      result: { artifactIds: [artifactId!], warnings: artifactWarnings },
      admission_digest: stop!.admissionObservation!.workload.digest.slice(7) });
    assert.equal((await admin.query("SELECT count(*)::int AS count FROM sandbox_events WHERE sandbox_id=$1 AND type='sandbox.deleted'", [stopSandboxId])).rows[0]?.count, 1);
    assert.equal(await first.claim("controller-delete-replay", 30), undefined);

    const unprovenDeleteSandbox = await seedSandbox(admin, workspaceId, userId, { state: "requested" });
    await admin.query("UPDATE sandboxes SET state='deleting',desired_state='deleted',version=2 WHERE id=$1", [unprovenDeleteSandbox]);
    const unprovenDeleteId = await insertOperation(admin, workspaceId, unprovenDeleteSandbox, userId, "delete", 1);
    assert.equal(await first.claim("controller-delete-unproven", 30), undefined);
    const refusedDelete = await admin.query(`SELECT r.status,r.cleanup_complete,r.error->>'code' AS code
      FROM sandbox_operations o JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id WHERE o.id=$1`, [unprovenDeleteId]);
    assert.deepEqual(refusedDelete.rows[0], { status: "recovery_required", cleanup_complete: false, code: "prior_cleanup_unverified" });
    assert.equal((await admin.query("SELECT has_function_privilege('blazn_sandbox_controller','sandbox_controller_finalize_stopped_delete_v1(uuid,text,uuid)','EXECUTE') AS allowed")).rows[0]?.allowed, false);

    // Recover the pre-fix failure mode: the old controller exhausts its claim
    // lease while decoding delete-after-stop, then a fresh delete retries it.
    const retryStoppedSandbox = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped", version: 2,
      backend: ["backend-retry-stop", "resource-retry-stop", "admission-retry-stop"] });
    const retryStopId = await insertOperation(admin, workspaceId, retryStoppedSandbox, userId, "stop", 1);
    const retryStop = await first.claim("controller-retry-stop", 30);
    assert.equal(retryStop?.operationId, retryStopId);
    assert.equal(await first.completeArtifactExport(retryStopId, "controller-retry-stop", retryStop!.leaseToken, retryStop!.admissionObservation!, []), true);
    assert.equal(await first.complete(retryStopId, "controller-retry-stop", retryStop!.leaseToken, {
      ...stopCompletion, expectedBackendUid: "backend-retry-stop", expectedBackendResourceVersion: "resource-retry-stop",
      expectedWorkloadDigest: retryStop!.admissionObservation!.workload.digest,
      expectedObservationDigest: retryStop!.admissionObservation!.digest, artifactIds: [], warningCodes: [],
    }), true);
    await admin.query("UPDATE sandboxes SET state='deleting',desired_state='deleted',version=version+1 WHERE id=$1", [retryStoppedSandbox]);
    const oldDeleteId = await insertOperation(admin, workspaceId, retryStoppedSandbox, userId, "delete", 3);
    await admin.query("SELECT * FROM sandbox_controller_claim_v4('pre-fix-controller',30)");
    await admin.query("UPDATE sandbox_reconcile_jobs SET attempt_count=5,lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1", [oldDeleteId]);
    assert.equal(await first.claim("controller-exhausted-delete", 30), undefined);
    assert.equal((await admin.query("SELECT status FROM sandbox_operations WHERE id=$1", [oldDeleteId])).rows[0]?.status, "recovery_required");
    const retryVersion = await admin.query("UPDATE sandboxes SET state='deleting',desired_state='deleted',version=version+1 WHERE id=$1 RETURNING version-1 AS expected", [retryStoppedSandbox]);
    const newDeleteId = await insertOperation(admin, workspaceId, retryStoppedSandbox, userId, "delete", Number(retryVersion.rows[0].expected));
    assert.equal(await first.claim("controller-recovered-delete", 30), undefined);
    assert.equal((await admin.query("SELECT status FROM sandbox_operations WHERE id=$1", [newDeleteId])).rows[0]?.status, "succeeded");
    assert.equal((await admin.query("SELECT state FROM sandboxes WHERE id=$1", [retryStoppedSandbox])).rows[0]?.state, "deleted");

    const legacySandboxId = await seedSandbox(admin, workspaceId, userId,
      { state: "stopping", desiredState: "stopped", version: 2, backend: ["backend-legacy", "resource-legacy", "admission-legacy"] });
    await admin.query(`UPDATE sandbox_workload_admissions SET pod_api_version=NULL,pod_kind=NULL,pod_namespace=NULL,
      pod_name=NULL,pod_uid=NULL,pod_resource_version=NULL,observation_digest=NULL WHERE sandbox_id=$1`, [legacySandboxId]);
    const legacyOperationId = await insertOperation(admin, workspaceId, legacySandboxId, userId, "stop", 1);
    const legacy = await first.claim("controller-legacy", 30);
    assert.equal(legacy?.operationId, legacyOperationId);
    assert.equal(legacy?.admissionObservation, null);
    assert.match(legacy?.persistedWorkloadDigest ?? "", /^sha256:[0-9a-f]{64}$/);
    const legacyBase = { expectedBackendUid: "backend-legacy", expectedBackendResourceVersion: "resource-legacy",
      expectedWorkloadDigest: legacy!.persistedWorkloadDigest, expectedObservationDigest: null,
      cleanupComplete: false, artifactExportComplete: false, grantsRevoked: false, backendDestroyed: false,
      artifactIds: [], warningCodes: [] };
    assert.equal(await first.complete(legacyOperationId, "controller-legacy", legacy!.leaseToken,
      { ...legacyBase, status: "succeeded", error: null }), false);
    assert.equal(await first.complete(legacyOperationId, "controller-legacy", legacy!.leaseToken,
      { ...legacyBase, status: "recovery_required", error: { code: "legacy_admission_observation",
        message: "legacy admission observation is unavailable", requestId: randomUUID() } }), true);
    const legacyTerminal = await admin.query("SELECT status FROM sandbox_operations WHERE id=$1", [legacyOperationId]);
    assert.equal(legacyTerminal.rows[0]?.status, "recovery_required");

    const duplicateSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "stopping", desiredState: "stopped" });
    await insertOperation(admin, workspaceId, duplicateSandboxId, userId, "stop");
    await assert.rejects(insertOperation(admin, workspaceId, duplicateSandboxId, userId, "delete"), hasCode("23505"));

    const expiredSandboxId = await seedSandbox(admin, workspaceId, userId, { state: "ready", expired: true, backend: ["backend-expired", "resource-expired", "admission-expired"] });
    const expiryRaces = await Promise.all([first.enqueueExpired(10), second.enqueueExpired(10)]);
    assert.equal(expiryRaces.flat().filter((value) => value.sandboxId === expiredSandboxId).length, 1);
    const expiry = await admin.query("SELECT o.type,o.status,o.idempotency_key,s.state,s.desired_state FROM sandbox_operations o JOIN sandboxes s ON s.id=o.sandbox_id WHERE o.sandbox_id=$1 AND o.status='pending'", [expiredSandboxId]);
    assert.equal(expiry.rows[0]?.type, "stop"); assert.equal(expiry.rows[0]?.status, "pending");
    assert.match(expiry.rows[0]?.idempotency_key, /^expiry-/); assert.equal(expiry.rows[0]?.state, "stopping"); assert.equal(expiry.rows[0]?.desired_state, "stopped");

    const eventSequences = await admin.query<{ sequence: string }>("SELECT sequence::text FROM sandbox_events WHERE sandbox_id=$1 ORDER BY sequence", [staleSandboxId]);
    assert.deepEqual(eventSequences.rows.map((row) => Number(row.sequence)), [0, 1, 2, 3, 4, 5, 6, 7, 8]);
    await assert.rejects(controllerOne.query("SELECT * FROM sandbox_reconcile_jobs"), hasCode("42501"));
    await assert.rejects(controllerOne.query("SELECT * FROM sandbox_workload_admissions"), hasCode("42501"));
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
  version?: number; backend?: [uid: string, resourceVersion: string, admissionId: string]; withArtifact?: boolean;
}): Promise<string> {
  const sandboxId = randomUUID(), templateId = randomUUID(), versionId = randomUUID(), suffix = sandboxId.slice(0, 8);
  const spec = { version: "1", variants: [{ name: "linux-amd64", architecture: "amd64",
    imageIndex: `registry.invalid/poc@sha256:${"b".repeat(64)}`, imageDigest: `registry.invalid/poc@sha256:${"c".repeat(64)}`,
    placementProfile: "poc-linux-amd64-v1", command: ["/bin/true"], resources: { requests: { cpu: "100m", memory: "128Mi", ephemeralStorage: "1Gi" }, limits: { cpu: "500m", memory: "512Mi", ephemeralStorage: "2Gi" } } }],
    repositories: [{ name: "source", url: "https://github.com/blazncloud/blazn.git", destination: "/workspace/src/blazn", writable: true }],
    artifacts: options.withArtifact ? [
      { name: "patch", path: "/workspace/artifacts/change.patch", mediaType: "text/plain", required: true },
      { name: "logs", path: "/workspace/artifacts/run.log", mediaType: "text/plain", required: false },
    ] : [] };
  const createdAt = options.expired ? new Date(Date.now() - 120_000) : new Date();
  const expiresAt = options.expired ? new Date(Date.now() - 60_000) : new Date(Date.now() + 600_000);
  await admin.query("BEGIN");
  try {
    await admin.query("INSERT INTO sandbox_templates(id,workspace_id,name,draft_spec,draft_digest,created_by) VALUES($1,$2,$3,$4,$5,$6)", [templateId, workspaceId, `template-${suffix}`, spec, "a".repeat(64), userId]);
    await admin.query("INSERT INTO sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by) VALUES($1,$2,$3,'1',$4,$5,$6,$7)", [versionId, workspaceId, templateId, Buffer.from(JSON.stringify(spec)), spec, "a".repeat(64), userId]);
    await admin.query("INSERT INTO sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile,command,resources) VALUES($1,$2,$3,'linux-amd64','amd64',$4,$5,'poc-linux-amd64-v1',$6::jsonb,$7::jsonb)", [versionId, workspaceId, templateId, `registry.invalid/poc@sha256:${"b".repeat(64)}`, `registry.invalid/poc@sha256:${"c".repeat(64)}`, JSON.stringify(["/bin/true"]), JSON.stringify(spec.variants[0]!.resources)]);
    await admin.query("INSERT INTO sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable) VALUES($1,$2,$3,'source','https://github.com/blazncloud/blazn.git','/workspace/src/blazn',true)", [versionId, workspaceId, templateId]);
    if (options.withArtifact) await admin.query(`INSERT INTO sandbox_template_version_artifacts(version_id,workspace_id,template_id,name,path,media_type,required) VALUES
      ($1,$2,$3,'patch','/workspace/artifacts/change.patch','text/plain',true),
      ($1,$2,$3,'logs','/workspace/artifacts/run.log','text/plain',false)`, [versionId, workspaceId, templateId]);
    await admin.query("INSERT INTO sandbox_template_version_status(version_id,workspace_id,template_id,status,changed_by) VALUES($1,$2,$3,'published',$4)", [versionId, workspaceId, templateId, userId]);
    await admin.query("UPDATE sandbox_templates SET current_published_version_id=$1 WHERE id=$2", [versionId, templateId]);
    await admin.query(`INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,
      variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,backend_uid,backend_resource_version,
      queue_name,admission_id,artifact_contract_digest,isolation,approved_non_sensitive,expires_at,created_at,updated_at,version)
      VALUES($1,$2,$3,$4,$5,$6,'1',$7,'linux-amd64',$8,$9,'amd64','direct',$10,$11,$12,$13,'blazn-poc-sandboxes',$14,$15,
      'approved-non-sensitive-poc',true,$16,$17,$17,$18)`, [sandboxId, workspaceId, userId, templateId, versionId, `template-${suffix}`, "a".repeat(64),
      `registry.invalid/poc@sha256:${"b".repeat(64)}`, `registry.invalid/poc@sha256:${"c".repeat(64)}`, options.state,
      options.desiredState ?? "ready", options.backend?.[0] ?? null, options.backend?.[1] ?? null, options.backend?.[2] ?? null,
      "d".repeat(64), expiresAt, createdAt, options.version ?? 1]);
    await admin.query("INSERT INTO sandbox_sources(sandbox_id,workspace_id,template_version_id,repository_name,commit) VALUES($1,$2,$3,'source',$4)", [sandboxId, workspaceId, versionId, "1".repeat(40)]);
    if (options.withArtifact) await admin.query(`INSERT INTO sandbox_artifact_contract_entries(sandbox_id,workspace_id,template_version_id,name,path,media_type,required) VALUES
      ($1,$2,$3,'patch','/workspace/artifacts/change.patch','text/plain',true),
      ($1,$2,$3,'logs','/workspace/artifacts/run.log','text/plain',false)`, [sandboxId, workspaceId, versionId]);
    if (options.backend) {
      const historicalOperationId = randomUUID(), historicalReceiptId = randomUUID();
      const observation = admissionObservation(workspaceId, sandboxId, options.backend[0], options.backend[1], options.backend[2]);
      const admission = observation.workload;
      await admin.query(`INSERT INTO sandbox_operations(id,workspace_id,sandbox_id,type,status,expected_sandbox_version,
        requested_by,idempotency_key,request_digest,terminal_receipt_id,completed_at)
        VALUES($1,$2,$3,'create','succeeded',$4,$5,$6,$7,$8,clock_timestamp())`,
      [historicalOperationId, workspaceId, sandboxId, options.version ?? 1, userId,
        `seed-create-${sandboxId}`, "f".repeat(64), historicalReceiptId]);
      await admin.query(`INSERT INTO sandbox_workload_admissions(sandbox_id,workspace_id,backend_uid,backend_resource_version,
        operation_id,api_version,namespace,workload_name,workload_uid,workload_resource_version,admitted_cluster_queue,
        owner_api_version,owner_kind,owner_name,owner_uid,owner_controller,workspace_label,sandbox_label,
        admitted,condition_type,condition_status,admission_digest,pod_api_version,pod_kind,pod_namespace,
        pod_name,pod_uid,pod_resource_version,observation_digest)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
          $23,$24,$25,$26,$27,$28,$29)`,
      [sandboxId, workspaceId, options.backend[0], options.backend[1], historicalOperationId, admission.apiVersion, admission.namespace,
        admission.name, admission.uid, admission.resourceVersion, admission.clusterQueue, admission.owner.apiVersion,
        admission.owner.kind, admission.owner.name, admission.owner.uid, admission.owner.controller,
        admission.workspaceId, admission.sandboxId, admission.admitted, admission.condition.type,
        admission.condition.status, admission.digest.slice(7), observation.pod.apiVersion,
        observation.pod.kind, observation.pod.namespace, observation.pod.name, observation.pod.uid,
        observation.pod.resourceVersion, observation.digest.slice(7)]);
      await admin.query(`INSERT INTO sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,
        operation_type,status,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,
        backend_present,backend_uid,backend_resource_version,admission_digest)
        VALUES($1,$2,$3,$4,'create','succeeded',false,false,false,false,true,$5,$6,$7)`,
      [historicalReceiptId, historicalOperationId, workspaceId, sandboxId, options.backend[0], options.backend[1], admission.digest.slice(7)]);
    }
    await admin.query("COMMIT"); return sandboxId;
  } catch (error) { await admin.query("ROLLBACK"); throw error; }
}

async function insertOperation(admin: Pool, workspaceId: string, sandboxId: string, userId: string, type: "create" | "stop" | "delete", expectedVersion?: number): Promise<string> {
  const id = randomUUID();
  await admin.query("INSERT INTO sandbox_operations(id,workspace_id,sandbox_id,type,status,expected_sandbox_version,requested_by,idempotency_key,request_digest) SELECT $1,$2,$3,$4,'pending',coalesce($8,version),$5,$6,$7 FROM sandboxes WHERE id=$3", [id, workspaceId, sandboxId, type, userId, `${type}-${id}`, "e".repeat(64), expectedVersion ?? null]);
  return id;
}

function successCreate(uid: string, observation: SandboxControllerAdmissionObservation | null) {
  return { status: "succeeded" as const, expectedBackendUid: uid, expectedBackendResourceVersion: "resource-create",
    expectedWorkloadDigest: observation?.workload.digest ?? null,
    expectedObservationDigest: observation?.digest ?? null,
    cleanupComplete: false, artifactExportComplete: false, grantsRevoked: false,
    backendDestroyed: false, artifactIds: [], warningCodes: [], error: null };
}

function sourceReceipt(sources: SandboxControllerSource[]): SandboxControllerSourceReceipt {
  const materialized = sources.map((source) => ({ ...source, tree: source.commit,
    contentDigest: `sha256:${"e".repeat(64)}`, fileCount: 0, totalBytes: 0 }));
  const manifestDigest = sourceFieldDigest(["blazn.dev/sandbox-source-manifest/v1",
    ...sources.flatMap((source) => [source.name, source.url, source.destination, source.commit, String(source.writable)])]);
  const receipt: SandboxControllerSourceReceipt = {
    schemaVersion: "blazn.dev/sandbox-source-materialization/v1", manifestDigest,
    sources: materialized, digest: "",
  };
  receipt.digest = sourceFieldDigest([receipt.schemaVersion, receipt.manifestDigest,
    ...materialized.flatMap((source) => [source.name, source.url, source.destination, source.commit, source.tree,
      source.contentDigest, String(source.fileCount), String(source.totalBytes), String(source.writable)])]);
  return receipt;
}

function sourceFieldDigest(fields: string[]): string {
  const hash = createHash("sha256");
  for (const field of fields) {
    const encoded = Buffer.from(field, "utf8"), size = Buffer.alloc(8);
    size.writeBigUInt64BE(BigInt(encoded.length));
    hash.update(size); hash.update(encoded);
  }
  return `sha256:${hash.digest("hex")}`;
}

function admissionObservation(workspaceId: string, sandboxId: string, backendUid: string,
  backendResourceVersion: string, workloadUid: string): SandboxControllerAdmissionObservation {
  const workload = admissionIdentity(workspaceId, sandboxId, backendUid, workloadUid);
  const observation = {
    sandbox: { apiVersion: "agents.x-k8s.io/v1beta1", kind: "Sandbox", namespace: "blazn-poc-sandboxes",
      name: sandboxId, uid: backendUid, resourceVersion: backendResourceVersion },
    pod: { apiVersion: "v1", kind: "Pod", namespace: "blazn-poc-sandboxes",
      name: `pod-${sandboxId}`, uid: `pod-${workloadUid}`, resourceVersion: "pod-resource-1" },
    workload,
  };
  const canonical = ["sandbox-admission-observation-v1", observation.sandbox.apiVersion,
    observation.sandbox.kind, observation.sandbox.namespace, observation.sandbox.name,
    observation.sandbox.uid, observation.sandbox.resourceVersion, observation.pod.apiVersion,
    observation.pod.kind, observation.pod.namespace, observation.pod.name, observation.pod.uid,
    observation.pod.resourceVersion, workload.apiVersion, workload.namespace, workload.name,
    workload.uid, workload.resourceVersion, workload.clusterQueue, workload.owner.apiVersion,
    workload.owner.kind, workload.owner.name, workload.owner.uid, String(workload.owner.controller),
    workload.workspaceId, workload.sandboxId, String(workload.admitted), workload.condition.type,
    workload.condition.status, workload.digest].join("\n");
  return { ...observation, digest: `sha256:${createHash("sha256").update(canonical).digest("hex")}` };
}

function admissionIdentity(workspaceId: string, sandboxId: string, backendUid: string, workloadUid: string): SandboxControllerAdmissionIdentity {
  const identity = {
    apiVersion: "kueue.x-k8s.io/v1beta1" as const, namespace: "blazn-poc-sandboxes" as const,
    name: `workload-${sandboxId}`, uid: workloadUid, resourceVersion: "workload-resource-1", clusterQueue: "poc-cluster",
    owner: { apiVersion: "agents.x-k8s.io/v1beta1" as const, kind: "Sandbox" as const, name: sandboxId, uid: backendUid, controller: true as const },
    workspaceId, sandboxId, admitted: true as const, condition: { type: "Admitted" as const, status: "True" as const },
  };
  const canonical = ["sandbox-workload-admission-v1", identity.apiVersion, identity.namespace, identity.name,
    identity.uid, identity.resourceVersion, identity.clusterQueue, identity.owner.apiVersion, identity.owner.kind,
    identity.owner.name, identity.owner.uid, String(identity.owner.controller), identity.workspaceId, identity.sandboxId,
    String(identity.admitted), identity.condition.type, identity.condition.status].join("\n");
  return { ...identity, digest: `sha256:${createHash("sha256").update(canonical).digest("hex")}` };
}

function hasCode(code: string): (error: unknown) => boolean {
  return (error) => !!error && typeof error === "object" && "code" in error && error.code === code;
}

async function sandboxSnapshot(admin: Pool, sandboxId: string): Promise<Record<string, unknown>> {
  const result = await admin.query("SELECT state,desired_state,version,backend_uid,backend_resource_version,admission_id FROM sandboxes WHERE id=$1", [sandboxId]);
  return result.rows[0]!;
}

async function assertStaleQuarantine(admin: Pool, operationId: string, sandboxId: string, snapshot: Record<string, unknown>): Promise<void> {
  const operation = await admin.query("SELECT o.status,r.error FROM sandbox_operations o JOIN sandbox_operation_terminal_receipts r ON r.id=o.terminal_receipt_id WHERE o.id=$1", [operationId]);
  assert.equal(operation.rows[0]?.status, "recovery_required");
  assert.equal(operation.rows[0]?.error.code, "stale_sandbox_operation");
  assert.deepEqual(await sandboxSnapshot(admin, sandboxId), snapshot, "stale post-claim authority changed the newer Sandbox");
}
