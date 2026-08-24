-- Persist immutable artifact UUIDs only after the controller has verified the
-- exact object under an active cleanup lease and frozen admission observation.

CREATE FUNCTION sandbox_controller_record_artifact_v1(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_expected_backend_uid text, p_expected_backend_resource_version text,
  p_expected_workload_digest text, p_expected_observation_digest text,
  p_name text, p_path text, p_media_type text, p_content_digest text,
  p_size_bytes bigint, p_object_key text)
RETURNS TABLE(artifact_id uuid, exported_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record;
BEGIN
  SELECT o.workspace_id,o.sandbox_id,o.type,a.backend_uid,a.backend_resource_version,
    a.admission_digest::text workload_digest,a.observation_digest::text observation_digest
  INTO target
  FROM public.sandbox_operations o
  JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
  JOIN public.sandbox_workload_admissions a ON a.sandbox_id=o.sandbox_id AND a.workspace_id=o.workspace_id
  WHERE o.id=p_operation_id AND o.status='running' AND o.type IN ('stop','delete') AND j.completed_at IS NULL
    AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j,s,a;
  IF NOT FOUND OR p_expected_backend_uid IS NULL OR p_expected_backend_resource_version IS NULL OR
     p_expected_workload_digest IS NULL OR p_expected_observation_digest IS NULL OR p_name IS NULL OR p_path IS NULL OR
     p_media_type IS NULL OR p_content_digest IS NULL OR p_size_bytes IS NULL OR p_object_key IS NULL OR
     target.backend_uid<>p_expected_backend_uid OR
     target.backend_resource_version<>p_expected_backend_resource_version OR
     target.workload_digest<>p_expected_workload_digest OR target.observation_digest<>p_expected_observation_digest OR
     p_name !~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' OR
     p_path !~ '^/workspace/artifacts(/[A-Za-z0-9._-]+)+$' OR p_path ~ '/\.\.?(/|$)' OR
     p_media_type !~ '^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$' OR
     p_content_digest !~ '^[0-9a-f]{64}$' OR p_size_bytes<0 OR p_size_bytes>8388608 OR
     p_object_key<>'workspaces/'||target.workspace_id::text||'/sandboxes/'||target.sandbox_id::text||'/artifacts/'||p_name OR
     NOT EXISTS(SELECT 1 FROM public.sandbox_artifact_contract_entries contract
       WHERE contract.sandbox_id=target.sandbox_id AND contract.workspace_id=target.workspace_id AND
         contract.name=p_name AND contract.path=p_path AND contract.media_type=p_media_type)
  THEN RETURN; END IF;

  INSERT INTO public.sandbox_artifacts(id,workspace_id,sandbox_id,name,path,object_key,media_type,content_digest,size_bytes,exported_at)
  VALUES(gen_random_uuid(),target.workspace_id,target.sandbox_id,p_name,p_path,p_object_key,p_media_type,
    p_content_digest,p_size_bytes,clock_timestamp())
  ON CONFLICT (sandbox_id,name) DO NOTHING;
  RETURN QUERY SELECT artifact.id,artifact.exported_at FROM public.sandbox_artifacts artifact
    WHERE artifact.sandbox_id=target.sandbox_id AND artifact.workspace_id=target.workspace_id AND
      artifact.name=p_name AND artifact.path=p_path AND artifact.object_key=p_object_key AND
      artifact.media_type=p_media_type AND artifact.content_digest=p_content_digest AND artifact.size_bytes=p_size_bytes;
END
$$;

CREATE FUNCTION sandbox_controller_claim_v5(p_worker_id text, p_lease_seconds integer)
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
  exported_artifact_sizes bigint[], exported_artifact_keys text[], exported_artifact_times timestamptz[])
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
  SELECT claimed.*,
    coalesce(exported.ids,'{}'::uuid[]),coalesce(exported.names,'{}'::text[]),
    coalesce(exported.paths,'{}'::text[]),coalesce(exported.media_types,'{}'::text[]),
    coalesce(exported.digests,'{}'::text[]),coalesce(exported.sizes,'{}'::bigint[]),
    coalesce(exported.keys,'{}'::text[]),coalesce(exported.times,'{}'::timestamptz[])
  FROM public.sandbox_controller_claim_v4(p_worker_id,p_lease_seconds) claimed
  LEFT JOIN LATERAL (
    SELECT array_agg(a.id ORDER BY a.name) ids,array_agg(a.name ORDER BY a.name) names,
      array_agg(a.path ORDER BY a.name) paths,array_agg(a.media_type ORDER BY a.name) media_types,
      array_agg(a.content_digest::text ORDER BY a.name) digests,array_agg(a.size_bytes ORDER BY a.name) sizes,
      array_agg(a.object_key ORDER BY a.name) keys,array_agg(a.exported_at ORDER BY a.name) times
    FROM public.sandbox_artifacts a WHERE a.sandbox_id=claimed.sandbox_id AND a.workspace_id=claimed.workspace_id
  ) exported ON true
$$;

REVOKE ALL ON TABLE sandbox_artifacts
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION
  sandbox_controller_record_artifact_v1(uuid,text,uuid,text,text,text,text,text,text,text,text,bigint,text),
  sandbox_controller_claim_v5(text,integer)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION sandbox_controller_claim_v4(text,integer) FROM blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION
  sandbox_controller_record_artifact_v1(uuid,text,uuid,text,text,text,text,text,text,text,text,bigint,text),
  sandbox_controller_claim_v5(text,integer)
  TO blazn_sandbox_controller;
