BEGIN;

-- M4.8: a create that fails before the controller records its backend (for
-- example an unschedulable Pod) used to leave a Sandbox whose delete always
-- ended in recovery_required/prior_cleanup_unverified, because the only
-- accepted proof was a verified stop receipt. A failed create whose terminal
-- receipt reports cleanup complete, grants revoked, and the backend destroyed
-- and absent is now accepted as that proof. The stop-proof branch is
-- unchanged, and the function stays callable only through claim_v5.
CREATE OR REPLACE FUNCTION sandbox_controller_finalize_stopped_delete_v1(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; prior record; completed boolean; artifact_ids uuid[]; warnings text[];
BEGIN
  SELECT o.id,o.workspace_id,o.sandbox_id,o.expected_sandbox_version,s.stopped_at
  INTO target
  FROM public.sandbox_operations o JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
  WHERE o.id=p_operation_id AND o.type='delete' AND o.status='running'
    AND s.state='deleting' AND s.desired_state='deleted'
    AND s.backend_uid IS NULL AND s.backend_resource_version IS NULL AND s.admission_id IS NULL
    AND j.completed_at IS NULL AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token
    AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j,s;
  IF NOT FOUND THEN RETURN false; END IF;

  SELECT r.result,a.admission_digest::text,a.observation_digest::text,phase.warning_codes
  INTO prior
  FROM public.sandbox_operations previous
  JOIN public.sandbox_operation_terminal_receipts r ON r.id=previous.terminal_receipt_id
  JOIN public.sandbox_artifact_export_receipts phase ON phase.operation_id=previous.id
    AND phase.sandbox_id=previous.sandbox_id AND phase.workspace_id=previous.workspace_id
  JOIN public.sandbox_workload_admissions a ON a.sandbox_id=previous.sandbox_id
    AND a.workspace_id=previous.workspace_id
  WHERE previous.sandbox_id=target.sandbox_id AND previous.workspace_id=target.workspace_id
    AND previous.type='stop' AND previous.status='succeeded'
    AND previous.completed_at=target.stopped_at
    AND previous.expected_sandbox_version+2<=target.expected_sandbox_version
    AND r.cleanup_complete AND r.artifact_export_complete AND r.grants_revoked
    AND r.backend_destroyed AND NOT r.backend_present
    AND r.admission_digest=a.admission_digest AND a.observation_digest IS NOT NULL
    AND phase.observation_digest=a.observation_digest
    AND r.result->'warnings'=to_jsonb(phase.warning_codes)
    AND NOT EXISTS(SELECT 1 FROM public.sandbox_operations later
      WHERE later.sandbox_id=target.sandbox_id AND later.id<>p_operation_id
        AND later.created_at>previous.completed_at
        AND (later.type<>'delete' OR later.status NOT IN ('failed','recovery_required')))
  FOR SHARE OF previous,r,phase,a;
  IF NOT FOUND THEN
    -- A create that failed before its backend was recorded has no stop
    -- receipt. Its own terminal receipt is the proof instead, but only when
    -- the controller reported the backend destroyed and absent: never an
    -- ambiguous recovery_required create, and never one that was admitted.
    PERFORM 1
    FROM public.sandbox_operations previous
    JOIN public.sandbox_operation_terminal_receipts r ON r.id=previous.terminal_receipt_id
    WHERE previous.sandbox_id=target.sandbox_id AND previous.workspace_id=target.workspace_id
      AND previous.type='create' AND previous.status='failed'
      AND r.status='failed' AND r.cleanup_complete AND r.grants_revoked AND r.backend_destroyed AND NOT r.backend_present
      AND r.backend_uid IS NULL AND r.admission_digest IS NULL
      AND previous.expected_sandbox_version<target.expected_sandbox_version
      AND target.stopped_at IS NULL
      AND NOT EXISTS(SELECT 1 FROM public.sandbox_workload_admissions a WHERE a.sandbox_id=target.sandbox_id)
      AND NOT EXISTS(SELECT 1 FROM public.sandbox_artifacts x WHERE x.sandbox_id=target.sandbox_id)
      AND NOT EXISTS(SELECT 1 FROM public.sandbox_artifact_contract_entries c WHERE c.sandbox_id=target.sandbox_id AND c.required)
      AND NOT EXISTS(SELECT 1 FROM public.sandbox_operations later
        WHERE later.sandbox_id=target.sandbox_id AND later.id<>p_operation_id
          AND later.created_at>previous.completed_at
          AND (later.type<>'delete' OR later.status NOT IN ('failed','recovery_required')))
    FOR SHARE OF previous,r;
    IF NOT FOUND THEN
      -- Missing proof must not turn a stopped/failed row into successful cleanup,
      -- or crash the shared controller repeatedly on an undecodable claim.
      RETURN public.sandbox_controller_complete(p_operation_id,p_worker_id,p_lease_token,
        'recovery_required',NULL,NULL,NULL,false,false,false,false,'{}'::uuid[],'{}'::text[],
        'prior_cleanup_unverified','delete without a live backend requires a verified stop or failed-create cleanup receipt',gen_random_uuid());
    END IF;
    -- Nothing ran, so every optional artifact is reported missing.
    SELECT coalesce(array_agg(code ORDER BY code),'{}'::text[]) INTO warnings
    FROM (SELECT 'optional_artifact_missing_'||replace(c.name,'-','_') AS code
          FROM public.sandbox_artifact_contract_entries c WHERE c.sandbox_id=target.sandbox_id) missing;
    completed := public.sandbox_controller_complete(p_operation_id,p_worker_id,p_lease_token,
      'succeeded',NULL,NULL,NULL,true,true,true,true,'{}'::uuid[],warnings,NULL,NULL,NULL);
    IF completed THEN
      PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
        'sandbox.deleted',NULL,NULL);
    END IF;
    RETURN completed;
  END IF;

  SELECT coalesce(array_agg(value::uuid),'{}'::uuid[]) INTO artifact_ids
  FROM jsonb_array_elements_text(prior.result->'artifactIds');
  IF NOT public.sandbox_controller_complete_artifact_export_v1(
      p_operation_id,p_worker_id,p_lease_token,prior.observation_digest,prior.warning_codes) THEN
    RETURN false;
  END IF;
  -- The base completion function still enforces the current operation/version,
  -- lease fencing, required artifacts, grant revocation, and terminal receipt.
  -- Live-identity wrappers are deliberately inapplicable after a proven stop.
  completed := public.sandbox_controller_complete(p_operation_id,p_worker_id,p_lease_token,
    'succeeded',NULL,NULL,NULL,true,true,true,true,artifact_ids,prior.warning_codes,NULL,NULL,NULL);
  IF completed THEN
    UPDATE public.sandbox_operation_terminal_receipts SET admission_digest=prior.admission_digest
      WHERE operation_id=p_operation_id;
    PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
      'sandbox.deleted',NULL,NULL);
  END IF;
  RETURN completed;
END
$$;
REVOKE ALL ON FUNCTION sandbox_controller_finalize_stopped_delete_v1(uuid,text,uuid)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller;

COMMIT;
