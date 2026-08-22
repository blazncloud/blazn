CREATE TABLE node_join_issuance_intents (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  enrollment_id uuid NOT NULL UNIQUE,
  plan_id uuid NOT NULL UNIQUE,
  node_id uuid NOT NULL,
  provider_handle text NOT NULL CHECK (provider_handle = id::text),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  status text NOT NULL CHECK (status IN ('pending', 'revoke_required', 'completed', 'revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (node_id, idempotency_key),
  FOREIGN KEY (enrollment_id, workspace_id) REFERENCES node_enrollments(id, workspace_id),
  FOREIGN KEY (plan_id, workspace_id, enrollment_id, node_id)
    REFERENCES node_install_plans(id, workspace_id, enrollment_id, node_id),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id) ON DELETE CASCADE
);

REVOKE ALL ON TABLE node_join_issuance_intents FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker;
GRANT SELECT ON TABLE node_join_issuance_intents TO blazn_node_broker;
GRANT INSERT (id, workspace_id, enrollment_id, plan_id, node_id, provider_handle,
  idempotency_key, request_digest, status) ON TABLE node_join_issuance_intents TO blazn_node_broker;
GRANT UPDATE (status, updated_at) ON TABLE node_join_issuance_intents TO blazn_node_broker;

CREATE FUNCTION node_broker_lock_join_binding(p_enrollment_id uuid, p_plan_id uuid, p_node_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE found boolean := false;
BEGIN
  SELECT true INTO found
    FROM node_enrollments e
    JOIN node_install_plans p ON p.enrollment_id=e.id AND p.workspace_id=e.workspace_id
    JOIN nodes n ON n.id=p.node_id AND n.workspace_id=p.workspace_id
    WHERE e.id=p_enrollment_id AND p.id=p_plan_id AND n.id=p_node_id
    FOR UPDATE OF e,p,n;
  RETURN coalesce(found, false);
END
$$;

REVOKE ALL ON FUNCTION node_broker_lock_join_binding(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION node_broker_lock_join_binding(uuid, uuid, uuid) TO blazn_node_broker;
