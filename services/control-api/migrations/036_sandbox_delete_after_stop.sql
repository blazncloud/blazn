BEGIN;

-- A successful stop destroys the backend and clears its live identifiers.
-- Its immutable admission and cleanup receipts remain historical evidence.
-- Finalize a subsequent delete inside the fenced database authority instead
-- of returning a contradictory live-backend work item to the controller.
CREATE FUNCTION sandbox_controller_finalize_stopped_delete_v1(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; prior record; completed boolean; artifact_ids uuid[];
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
    -- Missing proof must not turn a stopped/failed row into successful cleanup,
    -- or crash the shared controller repeatedly on an undecodable claim.
    RETURN public.sandbox_controller_complete(p_operation_id,p_worker_id,p_lease_token,
      'recovery_required',NULL,NULL,NULL,false,false,false,false,'{}'::uuid[],'{}'::text[],
      'prior_cleanup_unverified','delete without a live backend requires a verified stop receipt',gen_random_uuid());
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

CREATE OR REPLACE FUNCTION sandbox_controller_claim_v5(p_worker_id text, p_lease_seconds integer)
RETURNS TABLE(
  operation_id uuid, workspace_id uuid, sandbox_id uuid, requested_by uuid,
  operation_type text, expected_sandbox_version bigint, lease_token uuid, lease_expires_at timestamptz,
  attempt integer, allocation_mode text, desired_state text, architecture text, template_version_id uuid,
  template_digest text, variant_name text, image_index_digest text, image_child_digest text,
  placement_profile text, command text[], request_cpu text, request_memory text, request_ephemeral_storage text,
  limit_cpu text, limit_memory text, limit_ephemeral_storage text, queue_name text, admission_id text,
  backend_uid text, backend_resource_version text, expires_at timestamptz,
  source_names text[], source_urls text[], source_destinations text[], source_writable boolean[], source_commits text[],
  artifact_names text[], artifact_paths text[], artifact_media_types text[], artifact_required boolean[],
  admission_digest text, workload_api_version text, workload_namespace text, workload_name text,
  workload_uid text, workload_resource_version text, admitted_cluster_queue text,
  owner_api_version text, owner_kind text, owner_name text, owner_uid text, owner_controller boolean,
  workspace_label text, sandbox_label text, admitted boolean, condition_type text, condition_status text,
  pod_api_version text, pod_kind text, pod_namespace text, pod_name text, pod_uid text,
  pod_resource_version text, observation_digest text, source_materialization_receipt jsonb,
  source_bootstrap_observation jsonb, exported_artifact_ids uuid[], exported_artifact_names text[],
  exported_artifact_paths text[], exported_artifact_media_types text[], exported_artifact_digests text[],
  exported_artifact_sizes bigint[], exported_artifact_keys text[], exported_artifact_times timestamptz[],
  artifact_export_complete boolean, artifact_export_warning_codes text[])
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE claimed record;
BEGIN
  FOR claimed IN SELECT * FROM public.sandbox_controller_claim_v4(p_worker_id,p_lease_seconds)
  LOOP
    IF claimed.operation_type='delete' AND claimed.backend_uid IS NULL
       AND claimed.backend_resource_version IS NULL AND claimed.admission_id IS NULL THEN
      PERFORM public.sandbox_controller_finalize_stopped_delete_v1(
        claimed.operation_id,p_worker_id,claimed.lease_token);
      CONTINUE;
    END IF;
    RETURN QUERY SELECT
      claimed.operation_id,claimed.workspace_id,claimed.sandbox_id,claimed.requested_by,
      claimed.operation_type,claimed.expected_sandbox_version,claimed.lease_token,claimed.lease_expires_at,
      claimed.attempt,claimed.allocation_mode,claimed.desired_state,claimed.architecture,
      claimed.template_version_id,claimed.template_digest,claimed.variant_name,claimed.image_index_digest,
      claimed.image_child_digest,claimed.placement_profile,claimed.command,claimed.request_cpu,
      claimed.request_memory,claimed.request_ephemeral_storage,claimed.limit_cpu,claimed.limit_memory,
      claimed.limit_ephemeral_storage,claimed.queue_name,claimed.admission_id,claimed.backend_uid,
      claimed.backend_resource_version,claimed.expires_at,claimed.source_names,claimed.source_urls,
      claimed.source_destinations,claimed.source_writable,claimed.source_commits,claimed.artifact_names,
      claimed.artifact_paths,claimed.artifact_media_types,claimed.artifact_required,claimed.admission_digest,
      claimed.workload_api_version,claimed.workload_namespace,claimed.workload_name,claimed.workload_uid,
      claimed.workload_resource_version,claimed.admitted_cluster_queue,claimed.owner_api_version,claimed.owner_kind,
      claimed.owner_name,claimed.owner_uid,claimed.owner_controller,claimed.workspace_label,
      claimed.sandbox_label,claimed.admitted,claimed.condition_type,claimed.condition_status,
      claimed.pod_api_version,claimed.pod_kind,claimed.pod_namespace,claimed.pod_name,
      claimed.pod_uid,claimed.pod_resource_version,claimed.observation_digest,claimed.source_materialization_receipt,
      claimed.source_bootstrap_observation,
    coalesce(exported.ids,'{}'::uuid[]),coalesce(exported.names,'{}'::text[]),
    coalesce(exported.paths,'{}'::text[]),coalesce(exported.media_types,'{}'::text[]),
    coalesce(exported.digests,'{}'::text[]),coalesce(exported.sizes,'{}'::bigint[]),
    coalesce(exported.keys,'{}'::text[]),coalesce(exported.times,'{}'::timestamptz[]),
    (phase.operation_id IS NOT NULL),coalesce(phase.warning_codes,'{}'::text[])
  FROM LATERAL (
    SELECT array_agg(a.id ORDER BY a.name) ids,array_agg(a.name ORDER BY a.name) names,
      array_agg(a.path ORDER BY a.name) paths,array_agg(a.media_type ORDER BY a.name) media_types,
      array_agg(a.content_digest::text ORDER BY a.name) digests,array_agg(a.size_bytes ORDER BY a.name) sizes,
      array_agg(a.object_key ORDER BY a.name) keys,array_agg(a.exported_at ORDER BY a.name) times
    FROM public.sandbox_artifacts a WHERE a.sandbox_id=claimed.sandbox_id AND a.workspace_id=claimed.workspace_id
  ) exported
  LEFT JOIN public.sandbox_artifact_export_receipts phase
    ON phase.sandbox_id=claimed.sandbox_id AND phase.workspace_id=claimed.workspace_id AND phase.operation_id=claimed.operation_id;
  END LOOP;
END
$$;

COMMIT;
