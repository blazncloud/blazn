DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blazn_node_broker') THEN
    RAISE EXCEPTION 'required role blazn_node_broker is absent';
  END IF;
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO blazn_node_broker', current_database());
END
$$;

REVOKE CREATE ON SCHEMA public FROM blazn_node_broker;
REVOKE INSERT, UPDATE ON TABLE node_join_issuances FROM blazn_node_broker;
GRANT INSERT (
  id,
  workspace_id,
  enrollment_id,
  plan_id,
  node_id,
  node_public_key_fingerprint,
  machine_fingerprint,
  credential_hash,
  credential_ciphertext,
  credential_key_id,
  idempotency_key,
  request_digest,
  issued_at,
  expires_at
) ON TABLE node_join_issuances TO blazn_node_broker;
