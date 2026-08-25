DO $$
DECLARE constraint_name text;
BEGIN
  SELECT c.conname INTO constraint_name FROM pg_constraint c WHERE c.conrelid='nodes'::regclass AND c.contype='c' AND pg_get_constraintdef(c.oid) LIKE '%NOT agent_eligible%current_capability_version%';
  IF constraint_name IS NULL THEN RAISE EXCEPTION 'node eligibility constraint was not found'; END IF;
  EXECUTE format('ALTER TABLE nodes DROP CONSTRAINT %I',constraint_name);
END $$;

ALTER TABLE nodes ADD CONSTRAINT nodes_agent_eligibility_check CHECK (NOT agent_eligible OR (lifecycle_state='active' AND trust_state='verified' AND current_identity_status='active' AND kubernetes_node_uid IS NOT NULL AND current_identity_generation IS NOT NULL));
ALTER TABLE node_install_receipts
  ADD COLUMN activation_idempotency_key text,
  ADD COLUMN request_digest char(64),
  ADD COLUMN expected_node_version bigint,
  ADD COLUMN activation_grant jsonb;

ALTER TABLE node_heartbeat_state
  ADD COLUMN request_digest char(64),
  ADD CHECK (request_digest IS NULL OR request_digest ~ '^[0-9a-f]{64}$');

UPDATE node_install_receipts SET
  activation_idempotency_key='legacy-'||id::text,
  request_digest=receipt_digest,
  expected_node_version=1
WHERE activation_idempotency_key IS NULL;

ALTER TABLE node_install_receipts
  ALTER COLUMN activation_idempotency_key SET NOT NULL,
  ALTER COLUMN request_digest SET NOT NULL,
  ALTER COLUMN expected_node_version SET NOT NULL,
  ADD CHECK (char_length(activation_idempotency_key) BETWEEN 8 AND 128),
  ADD CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  ADD CHECK (expected_node_version > 0),
  ADD UNIQUE(node_id,activation_idempotency_key);
