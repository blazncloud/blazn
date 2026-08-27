BEGIN;

CREATE FUNCTION sandbox_controller_consume_access_grant_v1(
  p_grant_id uuid,
  p_token_hash char(64),
  p_kind text
)
RETURNS TABLE(
  workspace_id uuid,
  sandbox_id uuid,
  requested_by uuid,
  backend_uid text,
  backend_resource_version text
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
  grant_row public.sandbox_access_grants%ROWTYPE;
  sandbox_row public.sandboxes%ROWTYPE;
  session_valid boolean;
  effective_now timestamptz := clock_timestamp();
BEGIN
  SELECT * INTO grant_row
  FROM public.sandbox_access_grants
  WHERE id = p_grant_id
  FOR UPDATE;

  IF NOT FOUND THEN RETURN; END IF;
  IF grant_row.state = 'active' AND grant_row.expires_at <= effective_now THEN
    UPDATE public.sandbox_access_grants SET state='expired' WHERE id=p_grant_id;
    RETURN;
  END IF;
  IF grant_row.state <> 'active' OR grant_row.expires_at <= effective_now OR
     grant_row.token_hash <> p_token_hash OR grant_row.kind <> p_kind THEN
    RETURN;
  END IF;

  SELECT EXISTS(
    SELECT 1 FROM public.sessions
    WHERE id=grant_row.session_id AND user_id=grant_row.user_id
      AND revoked_at IS NULL AND access_expires_at > effective_now
      AND refresh_expires_at > effective_now
  ) INTO session_valid;
  IF NOT session_valid THEN RETURN; END IF;

  SELECT * INTO sandbox_row
  FROM public.sandboxes
  WHERE id=grant_row.sandbox_id AND workspace_id=grant_row.workspace_id
  FOR SHARE;
  IF NOT FOUND OR sandbox_row.state NOT IN ('ready','running') OR
     sandbox_row.backend_uid IS NULL OR sandbox_row.backend_resource_version IS NULL THEN
    RETURN;
  END IF;

  UPDATE public.sandbox_access_grants
  SET state='consumed', consumed_at=effective_now
  WHERE id=p_grant_id;

  workspace_id := sandbox_row.workspace_id;
  sandbox_id := sandbox_row.id;
  requested_by := sandbox_row.requested_by;
  backend_uid := sandbox_row.backend_uid;
  backend_resource_version := sandbox_row.backend_resource_version;
  RETURN NEXT;
END
$$;

REVOKE ALL ON FUNCTION sandbox_controller_consume_access_grant_v1(uuid,char(64),text)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker;
GRANT EXECUTE ON FUNCTION sandbox_controller_consume_access_grant_v1(uuid,char(64),text)
  TO blazn_sandbox_controller;

COMMIT;
