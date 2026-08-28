BEGIN;

-- The controller and PostgreSQL drivers may represent an empty warning set as
-- either NULL or an empty text array. There is no semantic distinction: a
-- missing warning is not a warning. Canonicalize at the authority boundary so
-- a warning-free export cannot strand an otherwise complete cleanup.
CREATE OR REPLACE FUNCTION sandbox_controller_complete_artifact_export_v1(
  p_operation_id uuid,p_worker_id text,p_lease_token uuid,p_expected_observation_digest text,p_warning_codes text[])
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; warning text; previous text := NULL;
  canonical_warnings text[] := coalesce(p_warning_codes,'{}'::text[]);
BEGIN
  SELECT o.workspace_id,o.sandbox_id,o.type,a.observation_digest::text INTO target
  FROM public.sandbox_operations o JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  JOIN public.sandbox_workload_admissions a ON a.sandbox_id=o.sandbox_id AND a.workspace_id=o.workspace_id
  WHERE o.id=p_operation_id AND o.status='running' AND o.type IN ('stop','delete') AND j.completed_at IS NULL
    AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j,a;
  IF NOT FOUND OR p_expected_observation_digest IS NULL OR target.observation_digest<>p_expected_observation_digest OR
     cardinality(canonical_warnings)>32 THEN RETURN false; END IF;
  FOREACH warning IN ARRAY canonical_warnings LOOP
    IF warning !~ '^optional_artifact_missing_[a-z0-9]([a-z0-9_]{0,61}[a-z0-9])?$' OR
       previous IS NOT NULL AND previous>=warning OR NOT EXISTS(
         SELECT 1 FROM public.sandbox_artifact_contract_entries contract
         WHERE contract.sandbox_id=target.sandbox_id AND NOT contract.required AND
           warning='optional_artifact_missing_'||replace(contract.name,'-','_') AND
           NOT EXISTS(SELECT 1 FROM public.sandbox_artifacts artifact
             WHERE artifact.sandbox_id=contract.sandbox_id AND artifact.name=contract.name))
    THEN RETURN false; END IF;
    previous := warning;
  END LOOP;
  IF EXISTS(
    SELECT 1 FROM public.sandbox_artifact_contract_entries contract
    WHERE contract.sandbox_id=target.sandbox_id AND
      NOT EXISTS(SELECT 1 FROM public.sandbox_artifacts artifact
        WHERE artifact.sandbox_id=contract.sandbox_id AND artifact.name=contract.name) AND
      (contract.required OR NOT ('optional_artifact_missing_'||replace(contract.name,'-','_')=ANY(canonical_warnings)))) THEN RETURN false; END IF;
  INSERT INTO public.sandbox_artifact_export_receipts(
    sandbox_id,workspace_id,operation_id,operation_type,observation_digest,warning_codes)
  VALUES(target.sandbox_id,target.workspace_id,p_operation_id,target.type,p_expected_observation_digest,canonical_warnings)
  ON CONFLICT (operation_id) DO NOTHING;
  RETURN EXISTS(SELECT 1 FROM public.sandbox_artifact_export_receipts receipt
    WHERE receipt.sandbox_id=target.sandbox_id AND receipt.workspace_id=target.workspace_id AND
      receipt.operation_id=p_operation_id AND receipt.operation_type=target.type AND
      receipt.observation_digest=p_expected_observation_digest AND receipt.warning_codes=canonical_warnings);
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
  canonical_warnings text[] := coalesce(p_warning_codes,'{}'::text[]);
BEGIN
  SELECT o.type,o.sandbox_id,o.workspace_id,phase.warning_codes INTO target
  FROM public.sandbox_operations o JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  LEFT JOIN public.sandbox_artifact_export_receipts phase ON phase.operation_id=o.id AND phase.sandbox_id=o.sandbox_id
  WHERE o.id=p_operation_id AND o.status='running' AND j.completed_at IS NULL AND
    j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j;
  IF NOT FOUND OR (p_status='succeeded' AND target.type IN ('stop','delete') AND
    (target.warning_codes IS NULL OR p_artifact_export_complete IS NOT TRUE OR target.warning_codes<>canonical_warnings)) THEN RETURN false; END IF;
  completed := public.sandbox_controller_complete_v4(
    p_operation_id,p_worker_id,p_lease_token,p_status,p_expected_backend_uid,p_expected_backend_resource_version,
    p_expected_workload_digest,p_expected_observation_digest,p_cleanup_complete,p_artifact_export_complete,
    p_grants_revoked,p_backend_destroyed,p_artifact_ids,canonical_warnings,p_error_code,p_safe_message,p_request_id);
  IF completed AND p_status='succeeded' THEN
    PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
      CASE target.type WHEN 'create' THEN 'sandbox.ready' WHEN 'stop' THEN 'sandbox.stopped' ELSE 'sandbox.deleted' END,
      NULL,NULL);
  END IF;
  RETURN completed;
END
$$;

COMMIT;
