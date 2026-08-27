BEGIN;

CREATE OR REPLACE FUNCTION sandbox_controller_consume_access_grant_v1(
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
  FROM public.sandbox_access_grants AS candidate_grant
  WHERE candidate_grant.id = p_grant_id
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
    SELECT 1 FROM public.sessions AS candidate_session
    WHERE candidate_session.id=grant_row.session_id AND candidate_session.user_id=grant_row.user_id
      AND candidate_session.revoked_at IS NULL AND candidate_session.access_expires_at > effective_now
      AND candidate_session.refresh_expires_at > effective_now
  ) INTO session_valid;
  IF NOT session_valid THEN RETURN; END IF;

  SELECT * INTO sandbox_row
  FROM public.sandboxes AS candidate_sandbox
  WHERE candidate_sandbox.id=grant_row.sandbox_id AND candidate_sandbox.workspace_id=grant_row.workspace_id
  FOR SHARE;
  IF NOT FOUND OR sandbox_row.state NOT IN ('ready','running') OR
     sandbox_row.backend_uid IS NULL OR sandbox_row.backend_resource_version IS NULL THEN
    RETURN;
  END IF;

  UPDATE public.sandbox_access_grants SET state='consumed', consumed_at=effective_now WHERE id=p_grant_id;

  workspace_id := sandbox_row.workspace_id;
  sandbox_id := sandbox_row.id;
  requested_by := sandbox_row.requested_by;
  backend_uid := sandbox_row.backend_uid;
  backend_resource_version := sandbox_row.backend_resource_version;
  RETURN NEXT;
END
$$;

CREATE OR REPLACE FUNCTION sandbox_controller_complete_v5(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid, p_status text,
  p_expected_backend_uid text, p_expected_backend_resource_version text,
  p_expected_workload_digest text, p_expected_observation_digest text,
  p_cleanup_complete boolean, p_artifact_export_complete boolean, p_grants_revoked boolean,
  p_backend_destroyed boolean, p_artifact_ids uuid[], p_warning_codes text[],
  p_error_code text, p_safe_message text, p_request_id uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; completed boolean;
BEGIN
  SELECT o.type,o.sandbox_id,o.workspace_id,phase.warning_codes INTO target
  FROM public.sandbox_operations o JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  LEFT JOIN public.sandbox_artifact_export_receipts phase ON phase.operation_id=o.id AND phase.sandbox_id=o.sandbox_id
  WHERE o.id=p_operation_id AND o.status='running' AND j.completed_at IS NULL AND
    j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j;
  IF NOT FOUND OR (p_status='succeeded' AND target.type IN ('stop','delete') AND
    (target.warning_codes IS NULL OR p_artifact_export_complete IS NOT TRUE OR target.warning_codes<>p_warning_codes)) THEN RETURN false; END IF;
  completed := public.sandbox_controller_complete_v4(
    p_operation_id,p_worker_id,p_lease_token,p_status,p_expected_backend_uid,p_expected_backend_resource_version,
    p_expected_workload_digest,p_expected_observation_digest,p_cleanup_complete,p_artifact_export_complete,
    p_grants_revoked,p_backend_destroyed,p_artifact_ids,p_warning_codes,p_error_code,p_safe_message,p_request_id);
  IF completed AND p_status='succeeded' THEN
    PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
      CASE target.type WHEN 'create' THEN 'sandbox.ready' WHEN 'stop' THEN 'sandbox.stopped' ELSE 'sandbox.deleted' END,
      NULL,NULL);
  END IF;
  RETURN completed;
END
$$;

COMMIT;
