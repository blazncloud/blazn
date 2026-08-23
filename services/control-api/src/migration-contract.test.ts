import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

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
  const sql = await readFile(path.resolve(here, "../migrations/014_sandbox_controller_claim_contract.sql"), "utf8");
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
  const sql = await readFile(path.resolve(here, "../migrations/015_sandbox_workload_admission_identity.sql"), "utf8");
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
