-- Complete the immutable controller work-item contract without widening the
-- controller role. The v1 claim remains an internal implementation detail;
-- only this typed projection is callable by the service role.

CREATE FUNCTION sandbox_controller_claim_v2(p_worker_id text, p_lease_seconds integer)
RETURNS TABLE(
  operation_id uuid, workspace_id uuid, sandbox_id uuid, requested_by uuid,
  operation_type text, expected_sandbox_version bigint, lease_token uuid,
  lease_expires_at timestamptz, attempt integer, allocation_mode text,
  desired_state text, architecture text, template_version_id uuid,
  template_digest text, variant_name text, image_index_digest text,
  image_child_digest text, placement_profile text, command text[],
  request_cpu text, request_memory text, request_ephemeral_storage text,
  limit_cpu text, limit_memory text, limit_ephemeral_storage text,
  queue_name text, admission_id text, backend_uid text,
  backend_resource_version text, expires_at timestamptz,
  source_names text[], source_urls text[], source_destinations text[],
  source_writable boolean[], source_commits text[],
  artifact_names text[], artifact_paths text[], artifact_media_types text[],
  artifact_required boolean[])
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
  SELECT claimed.operation_id,claimed.workspace_id,claimed.sandbox_id,s.requested_by,
    claimed.operation_type,claimed.expected_sandbox_version,claimed.lease_token,
    claimed.lease_expires_at,claimed.attempt,claimed.allocation_mode,
    claimed.desired_state,claimed.architecture,claimed.template_version_id,
    claimed.template_digest,claimed.variant_name,claimed.image_index_digest,
    claimed.image_child_digest,claimed.placement_profile,claimed.command,
    claimed.request_cpu,claimed.request_memory,claimed.request_ephemeral_storage,
    claimed.limit_cpu,claimed.limit_memory,claimed.limit_ephemeral_storage,
    claimed.queue_name,claimed.admission_id,claimed.backend_uid,
    claimed.backend_resource_version,claimed.expires_at,
    claimed.source_names,claimed.source_urls,claimed.source_destinations,
    claimed.source_writable,claimed.source_commits,
    coalesce(artifacts.names,'{}'::text[]),coalesce(artifacts.paths,'{}'::text[]),
    coalesce(artifacts.media_types,'{}'::text[]),coalesce(artifacts.required,'{}'::boolean[])
  FROM public.sandbox_controller_claim(p_worker_id,p_lease_seconds) claimed
  JOIN public.sandboxes s ON s.id=claimed.sandbox_id AND s.workspace_id=claimed.workspace_id
  LEFT JOIN LATERAL (
    SELECT array_agg(entry.name ORDER BY entry.name) names,
      array_agg(entry.path ORDER BY entry.name) paths,
      array_agg(entry.media_type ORDER BY entry.name) media_types,
      array_agg(entry.required ORDER BY entry.name) required
    FROM public.sandbox_artifact_contract_entries entry
    WHERE entry.sandbox_id=claimed.sandbox_id AND entry.workspace_id=claimed.workspace_id
  ) artifacts ON true
$$;

REVOKE ALL ON FUNCTION sandbox_controller_claim_v2(text,integer)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION sandbox_controller_claim(text,integer) FROM blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION sandbox_controller_claim_v2(text,integer) TO blazn_sandbox_controller;
