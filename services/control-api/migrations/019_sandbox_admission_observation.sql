-- Persist the complete API-stable Sandbox -> Pod -> Workload observation.  Rows
-- written before this migration intentionally remain Workload-only: they are
-- not evidence for a backend mutation after a controller restart.

ALTER TABLE sandbox_workload_admissions
  ADD COLUMN pod_api_version text,
  ADD COLUMN pod_kind text,
  ADD COLUMN pod_namespace text,
  ADD COLUMN pod_name text,
  ADD COLUMN pod_uid text,
  ADD COLUMN pod_resource_version text,
  ADD COLUMN observation_digest char(64);

CREATE FUNCTION sandbox_admission_observation_digest(
  p_backend_uid text, p_backend_resource_version text,
  p_pod_api_version text, p_pod_kind text, p_pod_namespace text,
  p_pod_name text, p_pod_uid text, p_pod_resource_version text,
  p_workload_api_version text, p_workload_namespace text, p_workload_name text,
  p_workload_uid text, p_workload_resource_version text, p_admitted_cluster_queue text,
  p_owner_api_version text, p_owner_kind text, p_owner_name text, p_owner_uid text,
  p_owner_controller boolean, p_workspace_label text, p_sandbox_label text,
  p_admitted boolean, p_condition_type text, p_condition_status text,
  p_workload_digest text)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT
SET search_path = pg_catalog, public
RETURN encode(public.digest(convert_to(
  'sandbox-admission-observation-v1'||E'\n'||
  'agents.x-k8s.io/v1beta1'||E'\n'||'Sandbox'||E'\n'||'blazn-poc-sandboxes'||E'\n'||
  p_sandbox_label||E'\n'||p_backend_uid||E'\n'||p_backend_resource_version||E'\n'||
  p_pod_api_version||E'\n'||p_pod_kind||E'\n'||p_pod_namespace||E'\n'||
  p_pod_name||E'\n'||p_pod_uid||E'\n'||p_pod_resource_version||E'\n'||
  p_workload_api_version||E'\n'||p_workload_namespace||E'\n'||p_workload_name||E'\n'||
  p_workload_uid||E'\n'||p_workload_resource_version||E'\n'||p_admitted_cluster_queue||E'\n'||
  p_owner_api_version||E'\n'||p_owner_kind||E'\n'||p_owner_name||E'\n'||p_owner_uid||E'\n'||
  p_owner_controller::text||E'\n'||p_workspace_label||E'\n'||p_sandbox_label||E'\n'||
  p_admitted::text||E'\n'||p_condition_type||E'\n'||p_condition_status||E'\n'||
  'sha256:'||p_workload_digest,'UTF8'),'sha256'),'hex');

ALTER TABLE sandbox_workload_admissions
  ADD CONSTRAINT sandbox_admission_observation_all_or_none CHECK (
    (pod_api_version IS NULL AND pod_kind IS NULL AND pod_namespace IS NULL AND
     pod_name IS NULL AND pod_uid IS NULL AND pod_resource_version IS NULL AND
     observation_digest IS NULL) OR
    (pod_api_version IS NOT NULL AND pod_kind IS NOT NULL AND pod_namespace IS NOT NULL AND
     pod_name IS NOT NULL AND pod_uid IS NOT NULL AND pod_resource_version IS NOT NULL AND
     observation_digest IS NOT NULL)),
  ADD CONSTRAINT sandbox_admission_observation_identity CHECK (
    observation_digest IS NULL OR
    (pod_api_version='v1' AND pod_kind='Pod' AND pod_namespace='blazn-poc-sandboxes' AND
     char_length(pod_name)<=253 AND
     pod_name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$' AND
     pod_uid ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' AND
     pod_resource_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' AND
     observation_digest=public.sandbox_admission_observation_digest(
       backend_uid,backend_resource_version,pod_api_version,pod_kind,pod_namespace,
       pod_name,pod_uid,pod_resource_version,api_version,namespace,workload_name,
       workload_uid,workload_resource_version,admitted_cluster_queue,owner_api_version,
       owner_kind,owner_name,owner_uid,owner_controller,workspace_label,sandbox_label,
       admitted,condition_type,condition_status,admission_digest::text)));

CREATE FUNCTION sandbox_controller_bind_backend_v3(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_backend_uid text, p_backend_resource_version text,
  p_sandbox_api_version text, p_sandbox_kind text, p_sandbox_namespace text,
  p_sandbox_name text, p_sandbox_uid text, p_sandbox_resource_version text,
  p_pod_api_version text, p_pod_kind text, p_pod_namespace text,
  p_pod_name text, p_pod_uid text, p_pod_resource_version text,
  p_workload_api_version text, p_workload_namespace text, p_workload_name text,
  p_workload_uid text, p_workload_resource_version text, p_admitted_cluster_queue text,
  p_owner_api_version text, p_owner_kind text, p_owner_name text, p_owner_uid text,
  p_owner_controller boolean, p_workspace_label text, p_sandbox_label text,
  p_admitted boolean, p_condition_type text, p_condition_status text,
  p_workload_digest text, p_observation_digest text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; expected_workload_digest text; expected_observation_digest text; bound boolean;
BEGIN
  SELECT o.workspace_id,o.sandbox_id,o.type INTO target
    FROM public.sandbox_operations o
    JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
    JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
    WHERE o.id=p_operation_id AND o.status='running' AND j.completed_at IS NULL
      AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token
      AND j.lease_expires_at>clock_timestamp()
    FOR UPDATE OF o,j,s;
  IF NOT FOUND OR target.type<>'create' THEN RETURN false; END IF;

  IF p_sandbox_api_version<>'agents.x-k8s.io/v1beta1' OR p_sandbox_kind<>'Sandbox' OR
     p_sandbox_namespace<>'blazn-poc-sandboxes' OR p_sandbox_name<>target.sandbox_id::text OR
     p_sandbox_uid<>p_backend_uid OR p_sandbox_resource_version<>p_backend_resource_version OR
     p_pod_api_version<>'v1' OR p_pod_kind<>'Pod' OR p_pod_namespace<>'blazn-poc-sandboxes' OR
     p_workload_api_version<>'kueue.x-k8s.io/v1beta1' OR p_workload_namespace<>'blazn-poc-sandboxes' OR
     p_owner_api_version<>'agents.x-k8s.io/v1beta1' OR p_owner_kind<>'Sandbox' OR
     p_owner_name<>target.sandbox_id::text OR p_owner_uid<>p_backend_uid OR p_owner_controller IS NOT TRUE OR
     p_workspace_label<>target.workspace_id::text OR p_sandbox_label<>target.sandbox_id::text OR
     p_admitted IS NOT TRUE OR p_condition_type<>'Admitted' OR p_condition_status<>'True' THEN
    RETURN false;
  END IF;
  expected_workload_digest := public.sandbox_workload_admission_digest(
    p_workload_api_version,p_workload_namespace,p_workload_name,p_workload_uid,
    p_workload_resource_version,p_admitted_cluster_queue,p_owner_api_version,p_owner_kind,
    p_owner_name,p_owner_uid,p_owner_controller,p_workspace_label,p_sandbox_label,p_admitted,
    p_condition_type,p_condition_status);
  IF p_workload_digest IS NULL OR p_workload_digest<>expected_workload_digest THEN RETURN false; END IF;
  expected_observation_digest := public.sandbox_admission_observation_digest(
    p_backend_uid,p_backend_resource_version,p_pod_api_version,p_pod_kind,p_pod_namespace,
    p_pod_name,p_pod_uid,p_pod_resource_version,p_workload_api_version,p_workload_namespace,
    p_workload_name,p_workload_uid,p_workload_resource_version,p_admitted_cluster_queue,
    p_owner_api_version,p_owner_kind,p_owner_name,p_owner_uid,p_owner_controller,
    p_workspace_label,p_sandbox_label,p_admitted,p_condition_type,p_condition_status,p_workload_digest);
  IF p_observation_digest IS NULL OR p_observation_digest<>expected_observation_digest THEN RETURN false; END IF;

  -- v1 changes the Sandbox tuple under the same row locks, but does not write
  -- the legacy Workload-only evidence table.  A conflicting legacy row is
  -- deliberately not upgraded by the INSERT below.
  bound := public.sandbox_controller_bind_backend(p_operation_id,p_worker_id,p_lease_token,
    p_backend_uid,p_backend_resource_version,p_workload_uid);
  IF NOT bound THEN RETURN false; END IF;
  INSERT INTO public.sandbox_workload_admissions(
    sandbox_id,workspace_id,operation_id,backend_uid,backend_resource_version,
    api_version,namespace,workload_name,workload_uid,workload_resource_version,
    admitted_cluster_queue,owner_api_version,owner_kind,owner_name,owner_uid,
    owner_controller,workspace_label,sandbox_label,admitted,condition_type,condition_status,
    admission_digest,pod_api_version,pod_kind,pod_namespace,pod_name,pod_uid,
    pod_resource_version,observation_digest)
  VALUES(target.sandbox_id,target.workspace_id,p_operation_id,p_backend_uid,p_backend_resource_version,
    p_workload_api_version,p_workload_namespace,p_workload_name,p_workload_uid,
    p_workload_resource_version,p_admitted_cluster_queue,p_owner_api_version,p_owner_kind,
    p_owner_name,p_owner_uid,p_owner_controller,p_workspace_label,p_sandbox_label,p_admitted,
    p_condition_type,p_condition_status,p_workload_digest,p_pod_api_version,p_pod_kind,
    p_pod_namespace,p_pod_name,p_pod_uid,p_pod_resource_version,p_observation_digest)
  ON CONFLICT (sandbox_id) DO NOTHING;
  RETURN EXISTS(SELECT 1 FROM public.sandbox_workload_admissions a
    WHERE a.sandbox_id=target.sandbox_id AND a.workspace_id=target.workspace_id AND
      a.operation_id=p_operation_id AND a.backend_uid=p_backend_uid AND
      a.backend_resource_version=p_backend_resource_version AND
      a.api_version=p_workload_api_version AND a.namespace=p_workload_namespace AND
      a.workload_name=p_workload_name AND a.workload_uid=p_workload_uid AND
      a.workload_resource_version=p_workload_resource_version AND
      a.admitted_cluster_queue=p_admitted_cluster_queue AND a.owner_api_version=p_owner_api_version AND
      a.owner_kind=p_owner_kind AND a.owner_name=p_owner_name AND a.owner_uid=p_owner_uid AND
      a.owner_controller=p_owner_controller AND a.workspace_label=p_workspace_label AND
      a.sandbox_label=p_sandbox_label AND a.admitted=p_admitted AND
      a.condition_type=p_condition_type AND a.condition_status=p_condition_status AND
      a.admission_digest=p_workload_digest AND a.pod_api_version=p_pod_api_version AND
      a.pod_kind=p_pod_kind AND a.pod_namespace=p_pod_namespace AND a.pod_name=p_pod_name AND
      a.pod_uid=p_pod_uid AND a.pod_resource_version=p_pod_resource_version AND
      a.observation_digest=p_observation_digest);
END
$$;

-- v3 exposes the full tuple.  A pre-019 row is returned with null Pod and
-- observation fields so typed clients can quarantine it without touching the
-- backend; no migration-time or claim-time evidence is synthesized.
CREATE FUNCTION sandbox_controller_claim_v3(p_worker_id text, p_lease_seconds integer)
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
  pod_resource_version text, observation_digest text)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
  SELECT claimed.operation_id,claimed.workspace_id,claimed.sandbox_id,claimed.requested_by,
    claimed.operation_type,claimed.expected_sandbox_version,claimed.lease_token,claimed.lease_expires_at,
    claimed.attempt,claimed.allocation_mode,claimed.desired_state,claimed.architecture,claimed.template_version_id,
    claimed.template_digest,claimed.variant_name,claimed.image_index_digest,claimed.image_child_digest,
    claimed.placement_profile,claimed.command,claimed.request_cpu,claimed.request_memory,claimed.request_ephemeral_storage,
    claimed.limit_cpu,claimed.limit_memory,claimed.limit_ephemeral_storage,claimed.queue_name,claimed.admission_id,
    claimed.backend_uid,claimed.backend_resource_version,claimed.expires_at,
    claimed.source_names,claimed.source_urls,claimed.source_destinations,claimed.source_writable,claimed.source_commits,
    claimed.artifact_names,claimed.artifact_paths,claimed.artifact_media_types,claimed.artifact_required,
    claimed.admission_digest,claimed.workload_api_version,claimed.workload_namespace,claimed.workload_name,
    claimed.workload_uid,claimed.workload_resource_version,claimed.admitted_cluster_queue,
    claimed.owner_api_version,claimed.owner_kind,claimed.owner_name,claimed.owner_uid,claimed.owner_controller,
    claimed.workspace_label,claimed.sandbox_label,claimed.admitted,claimed.condition_type,claimed.condition_status,
    a.pod_api_version,a.pod_kind,a.pod_namespace,a.pod_name,a.pod_uid,a.pod_resource_version,a.observation_digest::text
  FROM public.sandbox_controller_claim_v2(p_worker_id,p_lease_seconds) claimed
  LEFT JOIN public.sandbox_workload_admissions a
    ON a.sandbox_id=claimed.sandbox_id AND a.workspace_id=claimed.workspace_id
$$;

CREATE FUNCTION sandbox_controller_complete_v3(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid, p_status text,
  p_expected_backend_uid text, p_expected_backend_resource_version text,
  p_expected_workload_digest text, p_expected_observation_digest text,
  p_cleanup_complete boolean, p_artifact_export_complete boolean, p_grants_revoked boolean,
  p_backend_destroyed boolean, p_artifact_ids uuid[], p_warning_codes text[],
  p_error_code text, p_safe_message text, p_request_id uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record;
BEGIN
  SELECT o.type,a.admission_digest::text,a.observation_digest::text INTO target
    FROM public.sandbox_operations o
    JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
    LEFT JOIN public.sandbox_workload_admissions a
      ON a.sandbox_id=o.sandbox_id AND a.workspace_id=o.workspace_id
    WHERE o.id=p_operation_id AND o.status='running' AND j.completed_at IS NULL
      AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token
      AND j.lease_expires_at>clock_timestamp()
    FOR UPDATE OF o,j;
  IF NOT FOUND THEN RETURN false; END IF;

  IF target.admission_digest IS NULL THEN
    IF p_expected_workload_digest IS NOT NULL OR p_expected_observation_digest IS NOT NULL THEN RETURN false; END IF;
  ELSIF target.observation_digest IS NULL THEN
    -- A Workload-only legacy row cannot authorize success, failure/retry
    -- cleanup, or an in-place observation upgrade.
    IF p_status<>'recovery_required' OR p_expected_workload_digest IS NULL OR
       p_expected_workload_digest<>target.admission_digest OR p_expected_observation_digest IS NOT NULL THEN
      RETURN false;
    END IF;
  ELSIF p_expected_workload_digest IS NULL OR p_expected_observation_digest IS NULL OR
        p_expected_workload_digest<>target.admission_digest OR
        p_expected_observation_digest<>target.observation_digest THEN
    RETURN false;
  END IF;

  IF p_status='succeeded' AND (target.admission_digest IS NULL OR target.observation_digest IS NULL) THEN
    RETURN false;
  END IF;
  RETURN public.sandbox_controller_complete_v2(
    p_operation_id,p_worker_id,p_lease_token,p_status,p_expected_backend_uid,
    p_expected_backend_resource_version,p_expected_workload_digest,p_cleanup_complete,
    p_artifact_export_complete,p_grants_revoked,p_backend_destroyed,p_artifact_ids,
    p_warning_codes,p_error_code,p_safe_message,p_request_id);
END
$$;

REVOKE ALL ON FUNCTION
  sandbox_controller_claim_v2(text,integer),
  sandbox_controller_bind_backend_v2(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text),
  sandbox_controller_complete_v2(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  FROM blazn_sandbox_controller;

REVOKE ALL ON TABLE sandbox_workload_admissions
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION
  sandbox_admission_observation_digest(text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text),
  sandbox_controller_bind_backend_v3(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text),
  sandbox_controller_claim_v3(text,integer),
  sandbox_controller_complete_v3(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION
  sandbox_controller_claim_v3(text,integer),
  sandbox_controller_bind_backend_v3(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text),
  sandbox_controller_complete_v3(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  TO blazn_sandbox_controller;
