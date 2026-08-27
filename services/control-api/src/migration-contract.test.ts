import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { readMigrationInventory } from "./migration-inventory.js";

test("auth migration grants only the reviewed bootstrap and runtime operations", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const directory = path.resolve(here, "../migrations");
  const migration = (await Promise.all((await readdir(directory)).filter((name) => name.endsWith(".sql")).sort().map((name) => readFile(path.join(directory, name), "utf8")))).join("\n");
  const grants = [
    "REVOKE ALL ON ALL TABLES IN SCHEMA public FROM blazn_runtime, blazn_bootstrap;",
    "REVOKE ALL ON SEQUENCES FROM blazn_runtime, blazn_bootstrap;",
    "GRANT SELECT, INSERT ON TABLE users TO blazn_bootstrap;",
    "REVOKE INSERT, UPDATE, DELETE ON TABLE users FROM blazn_runtime;",
    "GRANT SELECT ON TABLE users TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE ON TABLE devices TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE device_authorizations TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE ON TABLE sessions TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE auth_rate_limits TO blazn_runtime;",
    "GRANT SELECT, INSERT, UPDATE ON TABLE user_identities TO blazn_runtime;",
    "GRANT INSERT (id, email, display_name, email_verified_at) ON TABLE users TO blazn_runtime;",
  ];
  for (const grant of grants) assert.ok(migration.includes(grant), `missing least-privilege SQL: ${grant}`);
  assert.doesNotMatch(migration, /GRANT\s+(?:ALL|SELECT, INSERT, UPDATE, DELETE)\s+ON\s+(?:ALL TABLES|TABLE users)\s+TO blazn_runtime/i);
});

test("OIDC signup serializes by email without requiring users update privilege", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const server = await readFile(path.resolve(here, "../src/server.ts"), "utf8");
  const signup = server.slice(server.indexOf("async function approveOidcIdentity"), server.indexOf("async function oidcCallback"));
  const advisoryLock = 'SELECT pg_advisory_xact_lock(hashtext($1))';
  const existingUserLookup = 'SELECT id FROM users WHERE email=$1';
  const advisoryLockIndex = signup.indexOf(advisoryLock);
  const existingUserLookupIndex = signup.indexOf(existingUserLookup);
  assert.ok(advisoryLockIndex >= 0, "OIDC signup must hold an email advisory lock");
  assert.ok(existingUserLookupIndex >= 0, "OIDC signup must check for an existing email");
  assert.ok(advisoryLockIndex < existingUserLookupIndex, "email advisory lock must precede the existing-user lookup");
  assert.doesNotMatch(signup, /SELECT id FROM users WHERE email=\$1 FOR UPDATE/);
});

test("node enrollment signing trust is immutable and relationally bound to plans", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/006_node_plan_signing_trust.sql"), "utf8");
  assert.match(sql, /requires zero pre-contract node enrollments/);
  assert.match(sql, /plan_signing_public_key char\(43\) NOT NULL/);
  assert.match(sql, /plan_signing_key_fingerprint char\(64\) NOT NULL/);
  assert.match(sql, /FOREIGN KEY \(enrollment_id, signing_key_id\)[\s\S]*REFERENCES node_enrollments\(id, plan_signing_key_id\)/);
  assert.match(sql, /REVOKE UPDATE ON TABLE node_enrollments FROM blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT UPDATE \([^)]*plan_signing_(?:key_id|public_key|key_fingerprint)/);
});

test("Node broker can connect without receiving schema creation rights", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/007_node_broker_connect.sql"), "utf8");
  assert.match(sql, /GRANT CONNECT ON DATABASE %I TO blazn_node_broker/);
  assert.match(sql, /REVOKE CREATE ON SCHEMA public FROM blazn_node_broker/);
  assert.match(sql, /REVOKE INSERT, UPDATE ON TABLE node_join_issuances FROM blazn_node_broker/);
  assert.match(sql, /GRANT INSERT \([\s\S]*credential_ciphertext[\s\S]*expires_at[\s\S]*\) ON TABLE node_join_issuances TO blazn_node_broker/);
  assert.doesNotMatch(sql, /GRANT INSERT \([^)]*(?:consumed_at|revoked_at|joined_node_uid)/);
});

test("Node broker durable intents and row-lock function are narrowly granted", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/008_node_broker_intents.sql"), "utf8");
  assert.match(sql, /provider_handle text NOT NULL CHECK \(provider_handle = id::text\)/);
  assert.match(sql, /status IN \('pending', 'issuing', 'revoke_required', 'completed', 'revoked'\)/);
  assert.match(sql, /CHECK \(\(status = 'issuing'\) = \(lease_expires_at IS NOT NULL\)\)/);
  assert.match(sql, /FOR UPDATE OF e,p,n/);
  assert.match(sql, /REVOKE ALL ON FUNCTION node_broker_lock_join_binding/);
  assert.match(sql, /GRANT EXECUTE ON FUNCTION node_broker_lock_join_binding/);
  const intentTable = sql.match(/CREATE TABLE node_join_issuance_intents \([\s\S]*?\n\);/)?.[0] ?? "";
  assert.doesNotMatch(intentTable, /credential_(?:hash|ciphertext)|\b(?:token|secret)\b/i);
});

test("sandbox persistence freezes immutable versions and workspace-scoped bindings", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/009_sandboxes.sql"), "utf8");
  assert.match(sql, /sandbox_template_versions_immutable/);
  assert.match(sql, /canonical_spec bytea NOT NULL/);
  assert.match(sql, /UNIQUE \(template_id, version\)/);
  assert.match(sql, /UNIQUE \(template_id, content_digest\)/);
  assert.match(sql, /FOREIGN KEY \(template_version_id, workspace_id, template_id, template_version, template_digest\)[\s\S]*REFERENCES sandbox_template_versions\(id, workspace_id, template_id, version, content_digest\)/);
  assert.match(sql, /token_hash char\(64\) NOT NULL UNIQUE/);
  assert.match(sql, /FOREIGN KEY \(session_id, user_id\) REFERENCES sessions\(id, user_id\)/);
  assert.match(sql, /CHECK \(expires_at > created_at AND expires_at <= created_at \+ interval '60 seconds'\)/);
  assert.match(sql, /workspace_json_contains_secret_key\(conditions\)/);
  for (const key of ["apikey", "privatekey", "clientsecret", "sessiontoken", "bearertoken", "signingkey"]) assert.match(sql, new RegExp(`'${key}'`));
  assert.match(sql, /REVOKE ALL ON TABLE[\s\S]*FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker/);
  assert.match(sql, /GRANT SELECT ON TABLE sandbox_template_versions TO blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT SELECT, INSERT ON TABLE sandbox_template_versions TO blazn_runtime/);
  assert.match(sql, /CREATE TABLE sandbox_template_version_variants/);
  assert.match(sql, /PRIMARY KEY \(version_id, architecture\)/);
  for (const comparison of ["image_index_digest=item->>'imageIndex'", "image_child_digest=item->>'imageDigest'", "placement_profile=item->>'placementProfile'", "command=item->'command'", "resources=item->'resources'", "url=item->>'url'", "writable=(item->>'writable')::boolean", "media_type=item->>'mediaType'", "required=(item->>'required')::boolean"]) assert.ok(sql.includes(comparison), `missing normalized comparison: ${comparison}`);
  assert.match(sql, /CREATE CONSTRAINT TRIGGER sandbox_create_children_complete/);
  assert.match(sql, /CREATE FUNCTION sandbox_publish_template_version[\s\S]*SECURITY DEFINER[\s\S]*digest\(p_canonical_spec,'sha256'\)/);
  assert.match(sql, /CREATE FUNCTION sandbox_create_bound_sandbox[\s\S]*SECURITY DEFINER[\s\S]*artifact contract digest mismatch/);
  assert.match(sql, /FOREIGN KEY \(sandbox_id, workspace_id, template_version_id\) REFERENCES sandboxes\(id, workspace_id, template_version_id\)/);
  assert.match(sql, /EXCEPT SELECT repository_name FROM public\.sandbox_sources/);
  assert.match(sql, /sandbox sources must cover every selected template repository exactly once/);
  assert.match(sql, /CREATE FUNCTION sandbox_consume_access_grant[\s\S]*SECURITY DEFINER/);
  assert.match(sql, /session_row\.id=grant_row\.session_id AND session_row\.user_id=grant_row\.user_id/);
  assert.match(sql, /session_row\.revoked_at IS NULL AND session_row\.access_expires_at > effective_now/);
  assert.match(sql, /session_row\.refresh_expires_at > effective_now/);
  assert.match(sql, /CREATE FUNCTION sandbox_revoke_access_grants[\s\S]*SECURITY DEFINER/);
  assert.doesNotMatch(sql, /sandbox_(?:consume_access_grant|revoke_access_grants)\([^)]*p_now/);
  assert.match(sql, /CREATE TRIGGER sandbox_access_grants_monotonic/);
  assert.match(sql, /UNIQUE \(sandbox_id, sequence\)/);
  assert.match(sql, /operation_type = 'create' AND backend_present AND NOT backend_destroyed/);
  assert.match(sql, /sandbox create receipt backend identity mismatch/);
  assert.match(sql, /CREATE TRIGGER sandbox_operation_terminal_receipts_enforced/);
  assert.match(sql, /operation_type IN \('stop','delete'\) AND NOT backend_present AND cleanup_complete/);
  assert.match(sql, /FOREIGN KEY \(sandbox_id, workspace_id\) REFERENCES sandboxes\(id, workspace_id\) ON DELETE CASCADE/);
  assert.doesNotMatch(sql, /GRANT UPDATE[^;]*sandbox_access_grants TO blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT SELECT, INSERT, UPDATE(?:, DELETE)? ON TABLE sandbox_template_versions/);
});

test("OIDC migration records complete reviewed assurance without removing legacy password identity", async () => {
	const here = path.dirname(fileURLToPath(import.meta.url));
	const sql = await readFile(path.resolve(here, "../migrations/014_oidc_identities.sql"), "utf8");
	assert.match(sql, /approved_identity_provider/);
	assert.match(sql, /approved_identity_policy_digest/);
	assert.match(sql, /approved_identity_acr/);
	assert.match(sql, /approved_identity_amr/);
	assert.match(sql, /cardinality\(approved_identity_amr\) >= 2/);
	assert.match(sql, /ALTER TABLE users ALTER COLUMN password_salt DROP NOT NULL/);
	assert.doesNotMatch(sql, /DROP (?:TABLE|COLUMN).*password/i);
});

test("sandbox duration migration derives expiry from one database clock and retires timestamp authority", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/011_sandbox_expiry_duration.sql"), "utf8");
  assert.match(sql, /CREATE FUNCTION sandbox_create_bound_sandbox_for_duration/);
  assert.match(sql, /p_expires_in_seconds < 60 OR p_expires_in_seconds > 7200/);
  assert.match(sql, /effective_expires_at := effective_now \+ make_interval\(secs => p_expires_in_seconds\)/);
  assert.match(sql, /REVOKE ALL ON FUNCTION sandbox_create_bound_sandbox\([\s\S]*timestamptz[\s\S]*FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker/);
  assert.match(sql, /GRANT EXECUTE ON FUNCTION sandbox_create_bound_sandbox_for_duration\([\s\S]*integer[\s\S]*TO blazn_runtime/);
});

test("sandbox controller migration exposes only fenced database authority", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/013_sandbox_controller_queue.sql"), "utf8");
  assert.match(sql, /sandbox_operations_one_nonterminal_per_sandbox_idx[\s\S]*status IN \('pending', 'running'\)/);
  assert.match(sql, /legacy_operation_incompatible/);
  assert.match(sql, /sandbox_controller_operation_is_current/);
  assert.match(sql, /FOR UPDATE OF j,o,s SKIP LOCKED LIMIT 1/);
  assert.match(sql, /stale_sandbox_operation/);
  assert.match(sql, /lease_token=p_lease_token AND j\.lease_expires_at>effective_now/);
  assert.match(sql, /sandbox\.operation\.lease_recovered/);
  assert.match(sql, /sandbox_controller_enqueue_expired[\s\S]*FOR UPDATE OF s SKIP LOCKED/);
  assert.match(sql, /successful create requires exact live backend and admission identity/);
  assert.match(sql, /sandbox completion artifact identity mismatch/);
  assert.match(sql, /sandbox_repository_url_has_no_inline_capability/);
  assert.match(sql, /REVOKE ALL ON TABLE sandbox_reconcile_jobs[\s\S]*blazn_sandbox_controller/);
  assert.match(sql, /GRANT EXECUTE ON FUNCTION sandbox_controller_claim[\s\S]*TO blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*TO blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /p_(?:payload|result|error|conditions) jsonb/);
});

test("sandbox controller v2 claim returns immutable owner and ordered artifact bindings", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/015_sandbox_controller_claim_contract.sql"), "utf8");
  assert.match(sql, /sandbox_controller_claim_v2/);
  assert.match(sql, /s\.requested_by/);
  assert.match(sql, /array_agg\(entry\.name ORDER BY entry\.name\)/);
  assert.match(sql, /array_agg\(entry\.path ORDER BY entry\.name\)/);
  assert.match(sql, /array_agg\(entry\.media_type ORDER BY entry\.name\)/);
  assert.match(sql, /array_agg\(entry\.required ORDER BY entry\.name\)/);
  assert.match(sql, /REVOKE ALL ON FUNCTION sandbox_controller_claim\(text,integer\) FROM blazn_sandbox_controller/);
  assert.match(sql, /GRANT EXECUTE ON FUNCTION sandbox_controller_claim_v2\(text,integer\) TO blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*TO blazn_sandbox_controller/);
});

test("sandbox controller admission migration persists only digest-bound admitted Workload ownership", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/016_sandbox_workload_admission_identity.sql"), "utf8");
  assert.match(sql, /CREATE TABLE sandbox_workload_admissions/);
  assert.match(sql, /owner_controller boolean NOT NULL CHECK \(owner_controller\)/);
  assert.match(sql, /condition_type text NOT NULL CHECK \(condition_type='Admitted'\)/);
  assert.match(sql, /condition_status text NOT NULL CHECK \(condition_status='True'\)/);
  assert.match(sql, /sandbox_workload_admission_digest/);
  assert.match(sql, /successful create requires digest-bound Workload admission identity/);
  assert.match(sql, /sandbox_controller_bind_backend_v2/);
  assert.match(sql, /sandbox_controller_complete_v2/);
  assert.match(sql, /REVOKE ALL ON TABLE sandbox_workload_admissions[\s\S]*blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*sandbox_workload_admissions/);
});

test("migration sequence derives one ordered collision-free inventory", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const directory = path.resolve(here, "../migrations");
  const migrations = await readMigrationInventory(directory);
  assert.deepEqual(migrations.slice(-9), [
    "023_node_activation.sql",
    "024_development_controller.sql",
    "025_development_executor.sql",
    "026_development_sandbox_evidence.sql",
    "027_controller_role_public_grants.sql",
    "028_development_candidate_image_binding.sql",
    "029_public_function_hardening_boundary.sql",
    "030_run_messages.sql",
    "031_run_message_digest_authority.sql",
  ]);
});

test("controller privilege hardening closes PUBLIC functions and preserves exact pgcrypto authority", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/029_public_function_hardening_boundary.sql"), "utf8");
  assert.match(sql, /owner_name=current_user[\s\S]*REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC/);
  assert.match(sql, /reviewed_pgcrypto[\s\S]*extension_catalog\.extname='pgcrypto'/);
  assert.match(sql, /unreviewed external function % retains PUBLIC EXECUTE/);
  assert.match(sql, /REVOKE EXECUTE ON FUNCTION %s FROM PUBLIC/);
  assert.match(sql, /public\.digest\(bytea,text\)[\s\S]*public\.digest\(text,text\)/);
  assert.match(sql, /reviewed pgcrypto digest authority is unavailable/);
  assert.doesNotMatch(sql, /GRANT EXECUTE ON (?:ALL FUNCTIONS|FUNCTION .*?) TO PUBLIC/);
});

test("artifact export migration fences immutable object evidence and UUID replay", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/021_sandbox_artifact_export.sql"), "utf8");
  assert.match(sql, /sandbox_controller_record_artifact_v1/);
  assert.match(sql, /j\.lease_expires_at>clock_timestamp\(\)/);
  assert.match(sql, /p_size_bytes>8388608/);
  assert.match(sql, /ON CONFLICT \(sandbox_id,name\) DO NOTHING/);
  assert.match(sql, /sandbox_controller_claim_v5/);
  assert.match(sql, /CREATE TABLE sandbox_artifact_export_receipts/);
  assert.match(sql, /sandbox_controller_complete_artifact_export_v1/);
  assert.match(sql, /sandbox_controller_complete_v5/);
  assert.match(sql, /target\.warning_codes<>p_warning_codes/);
  assert.match(sql, /array_agg\(a\.id ORDER BY a\.name\)/);
  assert.match(sql, /REVOKE ALL ON TABLE sandbox_artifacts,sandbox_artifact_export_receipts[\s\S]*blazn_sandbox_controller/);
  assert.match(sql, /GRANT SELECT ON TABLE sandbox_artifacts TO blazn_runtime/);
  assert.match(sql, /REVOKE ALL ON FUNCTION[\s\S]*sandbox_controller_complete_v4[\s\S]*FROM blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /GRANT (?:INSERT|UPDATE|DELETE|ALL)[^;]*sandbox_(?:artifacts|artifact_export_receipts)/);
});

test("source materialization migration fences exact receipts before create completion", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/020_sandbox_source_materialization.sql"), "utf8");
  assert.match(sql, /CREATE TABLE sandbox_source_materialization_receipts/);
  assert.match(sql, /sandbox_repository_destinations_nonoverlapping/);
  assert.match(sql, /sandbox_source_manifest_digest/);
  assert.match(sql, /sandbox_source_receipt_digest/);
  assert.match(sql, /sandbox_controller_record_source_materialization_v1/);
  assert.match(sql, /sandbox_controller_bind_backend_v4/);
  assert.match(sql, /j\.lease_expires_at>clock_timestamp\(\)/);
  assert.match(sql, /p_bootstrap_observation->>'digest'<>'sha256:'\|\|p_expected_observation_digest/);
  assert.match(sql, /ON CONFLICT \(sandbox_id\) DO NOTHING/);
  assert.match(sql, /sandbox_controller_claim_v4/);
  assert.match(sql, /sandbox_controller_complete_v4/);
  assert.match(sql, /target\.source_count>0 AND NOT target\.source_receipt/);
  assert.match(sql, /bootstrap#>>'\{pod,uid\}'<>p_pod_uid/);
  assert.match(sql, /REVOKE ALL ON TABLE sandbox_source_materialization_receipts[\s\S]*blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*sandbox_source_materialization_receipts/);
});

test("sandbox controller observation migration requires complete restart-safe Pod evidence", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/019_sandbox_admission_observation.sql"), "utf8");
  assert.match(sql, /ADD COLUMN pod_api_version text/);
  assert.match(sql, /ADD COLUMN observation_digest char\(64\)/);
  assert.match(sql, /sandbox_admission_observation_all_or_none/);
  assert.match(sql, /sandbox-admission-observation-v1/);
  assert.match(sql, /'sha256:'\|\|p_workload_digest/);
  assert.match(sql, /sandbox_controller_claim_v3/);
  assert.match(sql, /sandbox_controller_bind_backend_v3/);
  assert.match(sql, /sandbox_controller_complete_v3/);
  assert.match(sql, /A Workload-only legacy row cannot authorize success/);
  assert.match(sql, /p_status<>'recovery_required'/);
  assert.match(sql, /ON CONFLICT \(sandbox_id\) DO NOTHING/);
  assert.doesNotMatch(sql, /UPDATE public\.sandbox_workload_admissions[\s\S]*pod_/);
  assert.match(sql, /REVOKE ALL ON FUNCTION[\s\S]*sandbox_controller_claim_v2/);
  assert.match(sql, /REVOKE ALL ON TABLE sandbox_workload_admissions[\s\S]*blazn_sandbox_controller/);
  assert.doesNotMatch(sql, /GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*sandbox_workload_admissions/);
});

test("Development runtime migration freezes tenant, version, bearer proof, and closed finalization authority", async () => {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const sql = await readFile(path.resolve(here, "../migrations/022_development_runtime.sql"), "utf8");
  assert.match(sql, /FOREIGN KEY \(project_id, workspace_id\) REFERENCES projects\(id, workspace_id\)/);
  assert.match(sql, /FOREIGN KEY \(run_id, workspace_id, project_id\) REFERENCES runs\(id, workspace_id, project_id\)/);
  assert.match(sql, /FOREIGN KEY \(template_version_id, workspace_id, publication_template_id, template_version, template_digest\)/);
  assert.match(sql, /manifest#>>'\{template,digest\}' = 'sha256:'\|\|trim\(template_digest\)/);
  assert.match(sql, /CREATE FUNCTION development_controller_finalize/);
  assert.match(sql, /Reserved fail-closed stub[\s\S]*RETURN false/);
  assert.match(sql, /s\.token_hash=encode\(public\.digest\(p_access_token,'sha256'\),'hex'\)/);
  assert.doesNotMatch(sql, /current_setting\('blazn\.development_user_id'/);
  assert.doesNotMatch(sql, /p_(?:created_by|requested_by)/);
  assert.match(sql, /d\.version=p_expected_project_version AND d\.manifest_digest=p_expected_manifest_digest/);
  assert.match(sql, /FOR SHARE OF builder,network,resource,publication/);
  assert.match(sql, /s\.status='published' FOR SHARE OF v,s/);
  assert.match(sql, /v\.id=project\.template_version_id[\s\S]*s\.status='published' FOR SHARE OF v,s/);
  assert.match(sql, /REVOKE ALL ON TABLE development_policy_profiles,development_registry_repositories,development_projects,[\s\S]*development_reproducibility_baselines[\s\S]*blazn_runtime/);
  assert.match(sql, /GRANT EXECUTE ON FUNCTION development_runtime_access[\s\S]*development_runtime_get_project[\s\S]*development_runtime_list_builds[\s\S]*TO blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*development_(?:projects|builds|registry_repositories|reproducibility_baselines)/);
  assert.match(sql, /REVOKE ALL ON FUNCTION development_controller_finalize[\s\S]*blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT EXECUTE ON FUNCTION development_controller_finalize/);
});

test("Development controller migration normalizes evidence and grants only fenced functions",async()=>{
  const here=path.dirname(fileURLToPath(import.meta.url));
  const sql=await readFile(path.resolve(here,"../migrations/024_development_controller.sql"),"utf8");
  assert.match(sql,/CREATE TABLE development_build_jobs/);
  assert.match(sql,/FOR UPDATE OF job SKIP LOCKED/);
  assert.match(sql,/job\.lease_token=p_lease_token[\s\S]*job\.lease_expires_at>effective_now/);
  assert.match(sql,/CREATE TABLE development_build_evidence \(/);
  assert.match(sql,/CREATE TABLE development_build_evidence_artifacts \(/);
  assert.match(sql,/UNIQUE \(build_id,artifact_id\)/);
  assert.match(sql,/UNIQUE \(build_id,content_digest\)/);
  assert.match(sql,/stored\.source_run_id=target\.run_id/);
  assert.match(sql,/INSERT INTO public\.run_receipts[\s\S]*UPDATE public\.runs[\s\S]*UPDATE public\.development_builds/);
  assert.match(sql,/DROP FUNCTION development_controller_finalize\(uuid,bigint,jsonb\)/);
  assert.match(sql,/REVOKE ALL ON TABLE development_build_jobs,development_build_evidence,development_build_evidence_artifacts[\s\S]*blazn_development_controller/);
  assert.match(sql,/GRANT EXECUTE ON FUNCTION development_controller_claim[\s\S]*development_controller_finalize_v1[\s\S]*TO blazn_development_controller/);
  assert.doesNotMatch(sql,/GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*TO blazn_development_controller/);
  for(const forbidden of ["objectkey","signedurl","buildkitendpoint","buildkitclientcertificate","registrycredential"])
    assert.match(sql,new RegExp(`'${forbidden}'`));
});

test("Development executor migration persists bounded evidence through lease-fenced authority",async()=>{
  const here=path.dirname(fileURLToPath(import.meta.url));
  const sql=await readFile(path.resolve(here,"../migrations/025_development_executor.sql"),"utf8");
  assert.match(sql,/CREATE TABLE development_artifact_blobs/);
  assert.match(sql,/octet_length\(content\) BETWEEN 1 AND 16777216/);
  assert.match(sql,/p_content_digest <> 'sha256:' \|\| encode\(digest\(p_content,'sha256'\),'hex'\)/);
  assert.match(sql,/job\.worker_id<>p_worker_id[\s\S]*job\.lease_token<>p_lease_token[\s\S]*job\.lease_expires_at<=effective_now/);
  assert.match(sql,/stored_content=p_content/);
  assert.match(sql,/development_build_jobs WHERE build_id=p_build_id FOR UPDATE/);
  assert.match(sql,/development_controller_release_v1/);
  assert.match(sql,/attempt_count=CASE WHEN attempt_count>=5 THEN 0 ELSE attempt_count END/);
  assert.match(sql,/failure_count=failure_count\+1/);
  assert.match(sql,/execution_generation bigint NOT NULL DEFAULT 0/);
  assert.match(sql,/execution_generation=job\.execution_generation\+1/);
  assert.match(sql,/attempt_count=CASE WHEN job\.attempt_count>=5 THEN 1 ELSE job\.attempt_count\+1 END/);
  assert.doesNotMatch(sql,/available_at<=effective_now AND job\.attempt_count<5/);
  assert.match(sql,/CREATE FUNCTION development_controller_commit_execution_v1/);
  assert.match(sql,/Development Artifact commit was fenced[\s\S]*development_controller_finalize_v1[\s\S]*Development finalization was fenced/);
  assert.match(sql,/development_evidence_is_redacted\(content_document\)/);
  assert.match(sql,/github_pat_/);
  assert.match(sql,/REVOKE ALL ON TABLE development_artifact_blobs[\s\S]*blazn_development_controller/);
  assert.match(sql,/GRANT EXECUTE ON FUNCTION development_controller_store_artifact_v1[\s\S]*TO blazn_development_controller/);
  assert.doesNotMatch(sql,/GRANT (?:SELECT|INSERT|UPDATE|DELETE|ALL)[^;]*TO blazn_development_controller/);
});

test("Development Sandbox evidence migration reuses lifecycle authority and remains candidate-image fail closed",async()=>{
  const here=path.dirname(fileURLToPath(import.meta.url));
  const sql=await readFile(path.resolve(here,"../migrations/026_development_sandbox_evidence.sql"),"utf8");
  assert.match(sql,/CREATE TABLE development_sandbox_test_runs/);
  assert.match(sql,/candidate_image_bound boolean NOT NULL DEFAULT false/);
  assert.match(sql,/CHECK \(candidate_image_bound OR status IN \('preparing','ready','cleanup_pending','clean'\)\)/);
  assert.match(sql,/sandbox_create_bound_sandbox_for_duration/);
  assert.match(sql,/INSERT INTO public\.sandbox_operations[\s\S]*'create','pending'/);
  assert.match(sql,/development_collector_prepare_sandbox_v1/);
  assert.match(sql,/development_collector_resolve_sandbox_v1/);
  assert.match(sql,/attempt_generation bigint NOT NULL/);
  assert.match(sql,/job\.execution_generation=p_attempt_generation AND job\.lease_expires_at>clock_timestamp\(\)/);
  assert.match(sql,/development_build_jobs WHERE build_id=p_build_id FOR UPDATE/);
  assert.match(sql,/development_collector_authorize_execution_v1/);
  assert.match(sql,/PRIMARY KEY \(build_id,attempt_generation,platform,test_name\)/);
  assert.match(sql,/timeout_seconds integer NOT NULL CHECK \(timeout_seconds BETWEEN 1 AND 600\)/);
  assert.match(sql,/GRANT EXECUTE ON FUNCTION[\s\S]*TO blazn_development_controller/);
  assert.doesNotMatch(sql,/GRANT (?:SELECT|INSERT|UPDATE|DELETE).*development_sandbox_test_runs/);
});

test("Development candidate images replace only controller claims and fence terminal evidence",async()=>{
  const here=path.dirname(fileURLToPath(import.meta.url));
  const sql=await readFile(path.resolve(here,"../migrations/028_development_candidate_image_binding.sql"),"utf8");
  assert.match(sql,/CREATE TABLE development_candidate_image_bindings/);
  assert.match(sql,/FOREIGN KEY \(candidate_binding_id,build_id,workspace_id,project_id,attempt_generation,platform,candidate_image_index,candidate_image_child\)/);
  assert.match(sql,/CREATE OR REPLACE FUNCTION sandbox_controller_claim_v2[\s\S]*CASE WHEN authority\.mode='candidate' THEN binding\.image_child_digest ELSE claimed\.image_child_digest END/);
  assert.match(sql,/development_candidate_claim_mode_v1[\s\S]*development_build_jobs WHERE build_id=run\.build_id FOR UPDATE[\s\S]*job\.lease_expires_at>clock_timestamp\(\)[\s\S]*job\.execution_generation=run\.attempt_generation/);
  assert.match(sql,/run\.create_operation_id=p_operation_id AND p_operation_type='create'/);
  assert.match(sql,/run\.cleanup_operation_id=p_operation_id AND p_operation_type='delete'[\s\S]*RETURN 'ordinary'/);
  assert.match(sql,/development_collector_mark_sandbox_ready_v1[\s\S]*run\.status IN \('preparing','ready'\)[\s\S]*admission\.operation_id=run\.create_operation_id/);
  assert.match(sql,/development_collector_authorize_execution_v1[\s\S]*run\.status IN \('ready','running'\)/);
  assert.match(sql,/sandbox\.state IN \('ready','running'\)[\s\S]*admission\.observation_digest IS NOT NULL/);
  assert.match(sql,/REVOKE ALL ON FUNCTION development_reject_candidate_binding_mutation\(\)[\s\S]*FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller/);
  assert.match(sql,/REVOKE ALL ON FUNCTION development_reject_test_run_candidate_rebinding\(\)[\s\S]*FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller/);
  assert.match(sql,/CREATE OR REPLACE FUNCTION development_controller_commit_execution_v1[\s\S]*Development terminal images do not match the resolved candidate binding/);
  assert.match(sql,/REVOKE EXECUTE ON FUNCTION development_controller_finalize_v1[\s\S]*FROM blazn_development_controller/);
  assert.doesNotMatch(sql,/UPDATE public\.sandboxes SET image_/);
});
