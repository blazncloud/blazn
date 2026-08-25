CREATE TABLE development_candidate_image_bindings (
  id uuid PRIMARY KEY,
  build_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  attempt_generation bigint NOT NULL CHECK (attempt_generation > 0),
  platform text NOT NULL CHECK (platform IN ('linux/amd64','linux/arm64')),
  architecture text NOT NULL CHECK (architecture IN ('amd64','arm64')),
  image_index_digest text NOT NULL CHECK (image_index_digest ~ '^.+@sha256:[0-9a-f]{64}$'),
  image_child_digest text NOT NULL CHECK (image_child_digest ~ '^.+@sha256:[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (build_id,attempt_generation,platform),
  UNIQUE (id,build_id,workspace_id,project_id,attempt_generation,platform,image_index_digest,image_child_digest),
  FOREIGN KEY (build_id,workspace_id,project_id) REFERENCES development_builds(id,workspace_id,project_id) ON DELETE CASCADE,
  CHECK ((platform='linux/amd64' AND architecture='amd64') OR (platform='linux/arm64' AND architecture='arm64'))
);

CREATE FUNCTION development_reject_candidate_binding_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $$
BEGIN
  RAISE EXCEPTION 'Development candidate image bindings are immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER development_candidate_image_bindings_immutable
BEFORE UPDATE ON development_candidate_image_bindings
FOR EACH ROW EXECUTE FUNCTION development_reject_candidate_binding_mutation();
REVOKE ALL ON FUNCTION development_reject_candidate_binding_mutation()
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;

ALTER TABLE development_sandbox_test_runs ADD COLUMN candidate_binding_id uuid;
ALTER TABLE development_sandbox_test_runs ADD COLUMN candidate_image_child text CHECK (candidate_image_child IS NULL OR candidate_image_child ~ '^.+@sha256:[0-9a-f]{64}$');
ALTER TABLE development_sandbox_test_runs ADD CONSTRAINT development_sandbox_test_run_candidate_binding_shape
  CHECK ((candidate_image_bound AND candidate_binding_id IS NOT NULL AND candidate_image_child IS NOT NULL) OR
         (NOT candidate_image_bound AND candidate_binding_id IS NULL AND candidate_image_child IS NULL));
ALTER TABLE development_sandbox_test_runs ADD CONSTRAINT development_sandbox_test_run_candidate_binding_fk
  FOREIGN KEY (candidate_binding_id,build_id,workspace_id,project_id,attempt_generation,platform,candidate_image_index,candidate_image_child)
  REFERENCES development_candidate_image_bindings(id,build_id,workspace_id,project_id,attempt_generation,platform,image_index_digest,image_child_digest);

CREATE FUNCTION development_collector_bind_candidate_images_v1(
  p_build_id uuid,p_workspace_id uuid,p_attempt_generation bigint,p_image_index_digest text,
  p_amd64_child_digest text,p_arm64_child_digest text
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target public.development_builds%ROWTYPE; job public.development_build_jobs%ROWTYPE; existing_count bigint;
BEGIN
  IF p_attempt_generation<1 OR p_image_index_digest !~ '^.+@sha256:[0-9a-f]{64}$' OR
     p_amd64_child_digest !~ '^.+@sha256:[0-9a-f]{64}$' OR p_arm64_child_digest !~ '^.+@sha256:[0-9a-f]{64}$' OR
     p_amd64_child_digest=p_arm64_child_digest THEN
    RAISE EXCEPTION 'invalid Development candidate image binding' USING ERRCODE='22023';
  END IF;
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF NOT FOUND OR job.completed_at IS NOT NULL OR job.lease_expires_at IS NULL OR job.lease_expires_at<=clock_timestamp() OR
     job.execution_generation<>p_attempt_generation THEN RETURN false; END IF;
  SELECT * INTO target FROM public.development_builds WHERE id=p_build_id AND workspace_id=p_workspace_id FOR SHARE;
  IF NOT FOUND OR target.status NOT IN ('building','testing') THEN RETURN false; END IF;
  IF p_image_index_digest<>target.registry_repository||'@'||substring(p_image_index_digest from '@(sha256:[0-9a-f]{64})$') OR
     p_amd64_child_digest<>target.registry_repository||'@'||substring(p_amd64_child_digest from '@(sha256:[0-9a-f]{64})$') OR
     p_arm64_child_digest<>target.registry_repository||'@'||substring(p_arm64_child_digest from '@(sha256:[0-9a-f]{64})$') THEN
    RAISE EXCEPTION 'Development candidate image repository is invalid' USING ERRCODE='22023';
  END IF;
  SELECT count(*) INTO existing_count FROM public.development_candidate_image_bindings
    WHERE build_id=p_build_id AND attempt_generation=p_attempt_generation;
  IF existing_count>0 THEN
    IF existing_count=2 AND EXISTS(SELECT 1 FROM public.development_candidate_image_bindings WHERE build_id=p_build_id AND
      attempt_generation=p_attempt_generation AND platform='linux/amd64' AND workspace_id=p_workspace_id AND
      image_index_digest=p_image_index_digest AND image_child_digest=p_amd64_child_digest) AND
      EXISTS(SELECT 1 FROM public.development_candidate_image_bindings WHERE build_id=p_build_id AND
      attempt_generation=p_attempt_generation AND platform='linux/arm64' AND workspace_id=p_workspace_id AND
      image_index_digest=p_image_index_digest AND image_child_digest=p_arm64_child_digest) THEN RETURN true; END IF;
    RAISE EXCEPTION 'Development candidate image binding changed' USING ERRCODE='40001';
  END IF;
  INSERT INTO public.development_candidate_image_bindings(id,build_id,workspace_id,project_id,attempt_generation,platform,architecture,image_index_digest,image_child_digest)
  VALUES(gen_random_uuid(),target.id,target.workspace_id,target.project_id,p_attempt_generation,'linux/amd64','amd64',p_image_index_digest,p_amd64_child_digest),
    (gen_random_uuid(),target.id,target.workspace_id,target.project_id,p_attempt_generation,'linux/arm64','arm64',p_image_index_digest,p_arm64_child_digest);
  RETURN true;
END $$;

REVOKE ALL ON TABLE development_candidate_image_bindings FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,
  blazn_sandbox_controller,blazn_development_controller;
REVOKE ALL ON FUNCTION development_collector_bind_candidate_images_v1(uuid,uuid,bigint,text,text,text)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
GRANT EXECUTE ON FUNCTION development_collector_bind_candidate_images_v1(uuid,uuid,bigint,text,text,text)
  TO blazn_development_controller;

CREATE FUNCTION development_reject_test_run_candidate_rebinding() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $$
BEGIN
  IF OLD.candidate_image_bound AND (NEW.candidate_binding_id,NEW.candidate_image_index,NEW.candidate_image_child)
    IS DISTINCT FROM (OLD.candidate_binding_id,OLD.candidate_image_index,OLD.candidate_image_child) THEN
    RAISE EXCEPTION 'Development Sandbox candidate image binding is immutable' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER development_sandbox_test_run_candidate_binding_immutable
BEFORE UPDATE ON development_sandbox_test_runs
FOR EACH ROW EXECUTE FUNCTION development_reject_test_run_candidate_rebinding();
REVOKE ALL ON FUNCTION development_reject_test_run_candidate_rebinding()
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;

CREATE FUNCTION development_collector_prepare_bound_sandbox_v1(
  p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text,p_candidate_image_index text
) RETURNS SETOF development_sandbox_test_runs
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE binding public.development_candidate_image_bindings%ROWTYPE; prepared public.development_sandbox_test_runs%ROWTYPE;
BEGIN
  SELECT * INTO binding FROM public.development_candidate_image_bindings
    WHERE build_id=p_build_id AND attempt_generation=p_attempt_generation AND platform=p_platform AND image_index_digest=p_candidate_image_index;
  IF NOT FOUND THEN RETURN; END IF;
  SELECT * INTO prepared FROM public.development_collector_prepare_sandbox_v1(
    p_build_id,p_attempt_generation,p_platform,p_test_name,p_candidate_image_index);
  IF NOT FOUND THEN RETURN; END IF;
  UPDATE public.development_sandbox_test_runs SET candidate_binding_id=binding.id,candidate_image_child=binding.image_child_digest,
    candidate_image_bound=true,updated_at=clock_timestamp()
    WHERE build_id=p_build_id AND attempt_generation=p_attempt_generation AND platform=p_platform AND test_name=p_test_name AND
      (NOT candidate_image_bound OR (candidate_binding_id=binding.id AND candidate_image_child=binding.image_child_digest));
  IF NOT FOUND THEN RAISE EXCEPTION 'Development Sandbox candidate image binding changed' USING ERRCODE='40001'; END IF;
  SELECT * INTO prepared FROM public.development_sandbox_test_runs
    WHERE build_id=p_build_id AND attempt_generation=p_attempt_generation AND platform=p_platform AND test_name=p_test_name;
  RETURN NEXT prepared;
END $$;

CREATE FUNCTION development_collector_resolve_bound_sandbox_v1(p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text)
RETURNS TABLE(build_id uuid,workspace_id uuid,project_id uuid,platform text,test_name text,sandbox_id uuid,
  status text,sandbox_state text,candidate_image_index text,candidate_image_child text,candidate_image_bound boolean,argv jsonb,argv_digest text,
  timeout_seconds integer,backend_uid text,backend_resource_version text,pod_namespace text,pod_name text,pod_uid text,
  pod_resource_version text,observation_digest text,node_id uuid,receipt jsonb)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
  SELECT run.build_id,run.workspace_id,run.project_id,run.platform,run.test_name,run.sandbox_id,run.status,sandbox.state,
    run.candidate_image_index,run.candidate_image_child,run.candidate_image_bound,run.argv,run.argv_digest,run.timeout_seconds,sandbox.backend_uid,
    sandbox.backend_resource_version,admission.pod_namespace,admission.pod_name,admission.pod_uid,
    admission.pod_resource_version,admission.observation_digest::text,run.node_id,run.receipt
  FROM public.development_sandbox_test_runs run JOIN public.sandboxes sandbox ON sandbox.id=run.sandbox_id
  JOIN public.development_builds build ON build.id=run.build_id
  JOIN public.development_build_jobs job ON job.build_id=run.build_id
  JOIN public.development_candidate_image_bindings binding ON binding.id=run.candidate_binding_id AND
    binding.build_id=run.build_id AND binding.workspace_id=run.workspace_id AND binding.project_id=run.project_id AND
    binding.attempt_generation=run.attempt_generation AND binding.platform=run.platform AND
    binding.image_index_digest=run.candidate_image_index AND binding.image_child_digest=run.candidate_image_child
  LEFT JOIN public.sandbox_workload_admissions admission ON admission.sandbox_id=run.sandbox_id AND admission.workspace_id=run.workspace_id
  WHERE run.build_id=p_build_id AND run.attempt_generation=p_attempt_generation AND run.platform=p_platform AND run.test_name=p_test_name
    AND run.candidate_image_bound AND build.status IN ('building','testing') AND job.completed_at IS NULL
    AND job.execution_generation=p_attempt_generation AND job.lease_expires_at>clock_timestamp()
$$;

CREATE FUNCTION development_collector_mark_sandbox_ready_v1(
  p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text,p_sandbox_id uuid
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE job public.development_build_jobs%ROWTYPE; changed bigint;
BEGIN
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF NOT FOUND OR job.completed_at IS NOT NULL OR job.worker_id IS NULL OR job.lease_token IS NULL OR
     job.lease_expires_at IS NULL OR job.lease_expires_at<=clock_timestamp() OR
     job.execution_generation<>p_attempt_generation THEN RETURN false; END IF;
  UPDATE public.development_sandbox_test_runs run SET status='ready',updated_at=clock_timestamp()
    FROM public.sandboxes sandbox,public.sandbox_workload_admissions admission,public.development_builds build
    WHERE run.build_id=p_build_id AND run.attempt_generation=p_attempt_generation AND run.platform=p_platform AND
      run.test_name=p_test_name AND run.sandbox_id=p_sandbox_id AND run.candidate_image_bound AND run.status IN ('preparing','ready') AND
      build.id=run.build_id AND build.workspace_id=run.workspace_id AND build.project_id=run.project_id AND build.status IN ('building','testing') AND
      job.workspace_id=run.workspace_id AND job.project_id=run.project_id AND
      sandbox.id=run.sandbox_id AND sandbox.workspace_id=run.workspace_id AND sandbox.state IN ('ready','running') AND
      sandbox.backend_uid IS NOT NULL AND sandbox.backend_resource_version IS NOT NULL AND
      admission.sandbox_id=sandbox.id AND admission.workspace_id=sandbox.workspace_id AND
      admission.operation_id=run.create_operation_id AND admission.backend_uid=sandbox.backend_uid AND
      admission.backend_resource_version=sandbox.backend_resource_version AND admission.admitted AND
      admission.condition_type='Admitted' AND admission.condition_status='True' AND admission.owner_controller AND
      admission.owner_name=sandbox.id::text AND admission.owner_uid=sandbox.backend_uid AND
      admission.workspace_label=sandbox.workspace_id::text AND admission.sandbox_label=sandbox.id::text AND
      admission.pod_api_version='v1' AND admission.pod_kind='Pod' AND admission.pod_namespace='blazn-poc-sandboxes' AND
      admission.pod_name IS NOT NULL AND admission.pod_uid IS NOT NULL AND admission.pod_resource_version IS NOT NULL AND
      admission.observation_digest IS NOT NULL AND
      EXISTS(SELECT 1 FROM public.development_candidate_image_bindings binding WHERE binding.id=run.candidate_binding_id AND
        binding.build_id=run.build_id AND binding.workspace_id=run.workspace_id AND binding.project_id=run.project_id AND
        binding.attempt_generation=run.attempt_generation AND binding.platform=run.platform AND
        binding.image_index_digest=run.candidate_image_index AND binding.image_child_digest=run.candidate_image_child);
  GET DIAGNOSTICS changed=ROW_COUNT;
  RETURN changed=1;
END $$;

CREATE OR REPLACE FUNCTION development_collector_authorize_execution_v1(
  p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text,p_sandbox_id uuid
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE job public.development_build_jobs%ROWTYPE; changed bigint;
BEGIN
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF NOT FOUND OR job.completed_at IS NOT NULL OR job.worker_id IS NULL OR job.lease_token IS NULL OR
     job.lease_expires_at IS NULL OR job.lease_expires_at<=clock_timestamp() OR
     job.execution_generation<>p_attempt_generation THEN RETURN false; END IF;
  UPDATE public.development_sandbox_test_runs run SET status='running',updated_at=clock_timestamp()
    FROM public.sandboxes sandbox,public.sandbox_workload_admissions admission
    WHERE run.build_id=p_build_id AND run.attempt_generation=p_attempt_generation AND run.platform=p_platform AND
      run.test_name=p_test_name AND run.sandbox_id=p_sandbox_id AND run.candidate_image_bound AND run.status IN ('ready','running') AND
      sandbox.id=run.sandbox_id AND sandbox.workspace_id=run.workspace_id AND sandbox.state IN ('ready','running') AND
      sandbox.backend_uid IS NOT NULL AND sandbox.backend_resource_version IS NOT NULL AND
      admission.sandbox_id=sandbox.id AND admission.workspace_id=sandbox.workspace_id AND admission.operation_id=run.create_operation_id AND
      admission.backend_uid=sandbox.backend_uid AND admission.backend_resource_version=sandbox.backend_resource_version AND
      admission.admitted AND admission.condition_type='Admitted' AND admission.condition_status='True' AND
      admission.owner_controller AND admission.owner_name=sandbox.id::text AND admission.owner_uid=sandbox.backend_uid AND
      admission.workspace_label=sandbox.workspace_id::text AND admission.sandbox_label=sandbox.id::text AND
      admission.pod_api_version='v1' AND admission.pod_kind='Pod' AND admission.pod_namespace='blazn-poc-sandboxes' AND
      admission.pod_name IS NOT NULL AND admission.pod_uid IS NOT NULL AND admission.pod_resource_version IS NOT NULL AND
      admission.observation_digest IS NOT NULL AND
      EXISTS(SELECT 1 FROM public.development_candidate_image_bindings binding WHERE binding.id=run.candidate_binding_id AND
        binding.build_id=run.build_id AND binding.workspace_id=run.workspace_id AND binding.project_id=run.project_id AND
        binding.attempt_generation=run.attempt_generation AND binding.platform=run.platform AND
        binding.image_index_digest=run.candidate_image_index AND binding.image_child_digest=run.candidate_image_child) AND
      EXISTS(SELECT 1 FROM public.development_builds build WHERE build.id=run.build_id AND build.status IN ('building','testing'));
  GET DIAGNOSTICS changed=ROW_COUNT;
  RETURN changed=1;
END $$;

REVOKE EXECUTE ON FUNCTION development_collector_prepare_sandbox_v1(uuid,bigint,text,text,text),
  development_collector_resolve_sandbox_v1(uuid,bigint,text,text) FROM blazn_development_controller;
REVOKE ALL ON FUNCTION development_collector_prepare_bound_sandbox_v1(uuid,bigint,text,text,text),
  development_collector_resolve_bound_sandbox_v1(uuid,bigint,text,text),
  development_collector_mark_sandbox_ready_v1(uuid,bigint,text,text,uuid)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
GRANT EXECUTE ON FUNCTION development_collector_prepare_bound_sandbox_v1(uuid,bigint,text,text,text),
  development_collector_resolve_bound_sandbox_v1(uuid,bigint,text,text),
  development_collector_mark_sandbox_ready_v1(uuid,bigint,text,text,uuid) TO blazn_development_controller;

-- Preserve the ordinary Sandbox tuple and its published-template foreign key,
-- but project the immutable Development candidate into the controller claim.
-- v3/v4/v5 compose this v2 function, so every production controller path sees
-- the candidate child that is linked to the Development run rather than the
-- published template child used to bootstrap the ordinary lifecycle row.
CREATE FUNCTION development_candidate_claim_mode_v1(
  p_operation_id uuid,p_workspace_id uuid,p_sandbox_id uuid,p_operation_type text,
  p_backend_uid text,p_backend_resource_version text
) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE run public.development_sandbox_test_runs%ROWTYPE; job public.development_build_jobs%ROWTYPE; job_found boolean;
BEGIN
  SELECT * INTO run FROM public.development_sandbox_test_runs
    WHERE sandbox_id=p_sandbox_id AND workspace_id=p_workspace_id;
  IF NOT FOUND THEN RETURN 'ordinary'; END IF;
  -- Follow the job -> run lock order used by prepare/readiness authority so a
  -- claim cannot deadlock with a generation reclaim or candidate binding.
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=run.build_id FOR UPDATE;
  job_found:=FOUND;
  SELECT * INTO run FROM public.development_sandbox_test_runs
    WHERE sandbox_id=p_sandbox_id AND workspace_id=p_workspace_id FOR UPDATE;
  IF NOT FOUND THEN RETURN 'ordinary'; END IF;
  IF run.cleanup_operation_id=p_operation_id AND p_operation_type='delete' THEN RETURN 'ordinary'; END IF;
  IF run.create_operation_id=p_operation_id AND p_operation_type='create' AND
     run.status IN ('preparing','ready','running') AND run.candidate_image_bound THEN
    -- Serialize candidate projection with Development lease reclaim. A plain
    -- outer-join snapshot can remain stale while another controller advances
    -- execution_generation, so authority has to lock and re-read the job.
    IF job_found AND job.workspace_id=run.workspace_id AND job.project_id=run.project_id AND
       job.completed_at IS NULL AND job.worker_id IS NOT NULL AND job.lease_token IS NOT NULL AND
       job.lease_expires_at IS NOT NULL AND job.lease_expires_at>clock_timestamp() AND
       job.execution_generation=run.attempt_generation AND
       EXISTS(SELECT 1 FROM public.development_builds build WHERE build.id=run.build_id AND
         build.workspace_id=run.workspace_id AND build.project_id=run.project_id AND build.status IN ('building','testing')) AND
       EXISTS(SELECT 1 FROM public.development_candidate_image_bindings binding WHERE binding.id=run.candidate_binding_id AND
         binding.build_id=run.build_id AND binding.workspace_id=run.workspace_id AND binding.project_id=run.project_id AND
         binding.attempt_generation=run.attempt_generation AND binding.platform=run.platform AND
         binding.image_index_digest=run.candidate_image_index AND binding.image_child_digest=run.candidate_image_child) THEN
      RETURN 'candidate';
    END IF;
  END IF;
  PERFORM public.sandbox_controller_quarantine_stale(p_operation_id,p_workspace_id,p_sandbox_id,p_operation_type,
    p_backend_uid,p_backend_resource_version);
  RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION development_candidate_claim_mode_v1(uuid,uuid,uuid,text,text,text)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;

CREATE OR REPLACE FUNCTION sandbox_controller_claim_v2(p_worker_id text,p_lease_seconds integer)
RETURNS TABLE(
  operation_id uuid,workspace_id uuid,sandbox_id uuid,requested_by uuid,
  operation_type text,expected_sandbox_version bigint,lease_token uuid,lease_expires_at timestamptz,
  attempt integer,allocation_mode text,desired_state text,architecture text,template_version_id uuid,
  template_digest text,variant_name text,image_index_digest text,image_child_digest text,
  placement_profile text,command text[],request_cpu text,request_memory text,request_ephemeral_storage text,
  limit_cpu text,limit_memory text,limit_ephemeral_storage text,queue_name text,admission_id text,
  backend_uid text,backend_resource_version text,expires_at timestamptz,
  source_names text[],source_urls text[],source_destinations text[],source_writable boolean[],source_commits text[],
  artifact_names text[],artifact_paths text[],artifact_media_types text[],artifact_required boolean[],
  admission_digest text,workload_api_version text,workload_namespace text,workload_name text,
  workload_uid text,workload_resource_version text,admitted_cluster_queue text,
  owner_api_version text,owner_kind text,owner_name text,owner_uid text,owner_controller boolean,
  workspace_label text,sandbox_label text,admitted boolean,condition_type text,condition_status text)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
  SELECT claimed.operation_id,claimed.workspace_id,claimed.sandbox_id,s.requested_by,
    claimed.operation_type,claimed.expected_sandbox_version,claimed.lease_token,claimed.lease_expires_at,
    claimed.attempt,claimed.allocation_mode,claimed.desired_state,claimed.architecture,claimed.template_version_id,
    claimed.template_digest,claimed.variant_name,
    CASE WHEN authority.mode='candidate' THEN binding.image_index_digest ELSE claimed.image_index_digest END,
    CASE WHEN authority.mode='candidate' THEN binding.image_child_digest ELSE claimed.image_child_digest END,
    claimed.placement_profile,claimed.command,claimed.request_cpu,claimed.request_memory,claimed.request_ephemeral_storage,
    claimed.limit_cpu,claimed.limit_memory,claimed.limit_ephemeral_storage,claimed.queue_name,claimed.admission_id,
    claimed.backend_uid,claimed.backend_resource_version,claimed.expires_at,
    claimed.source_names,claimed.source_urls,claimed.source_destinations,claimed.source_writable,claimed.source_commits,
    coalesce(artifacts.names,'{}'::text[]),coalesce(artifacts.paths,'{}'::text[]),
    coalesce(artifacts.media_types,'{}'::text[]),coalesce(artifacts.required,'{}'::boolean[]),
    admission.admission_digest::text,admission.api_version,admission.namespace,admission.workload_name,
    admission.workload_uid,admission.workload_resource_version,admission.admitted_cluster_queue,
    admission.owner_api_version,admission.owner_kind,admission.owner_name,admission.owner_uid,admission.owner_controller,
    admission.workspace_label,admission.sandbox_label,admission.admitted,admission.condition_type,admission.condition_status
  FROM public.sandbox_controller_claim(p_worker_id,p_lease_seconds) claimed
  JOIN public.sandboxes s ON s.id=claimed.sandbox_id AND s.workspace_id=claimed.workspace_id
  JOIN LATERAL (SELECT public.development_candidate_claim_mode_v1(claimed.operation_id,claimed.workspace_id,
    claimed.sandbox_id,claimed.operation_type,claimed.backend_uid,claimed.backend_resource_version) mode) authority
    ON authority.mode IS NOT NULL
  LEFT JOIN public.development_sandbox_test_runs run ON run.sandbox_id=claimed.sandbox_id AND
    run.workspace_id=claimed.workspace_id
  LEFT JOIN public.development_candidate_image_bindings binding ON binding.id=run.candidate_binding_id AND
    binding.build_id=run.build_id AND binding.workspace_id=run.workspace_id AND binding.project_id=run.project_id AND
    binding.attempt_generation=run.attempt_generation AND binding.platform=run.platform AND
    binding.image_index_digest=run.candidate_image_index AND binding.image_child_digest=run.candidate_image_child
  LEFT JOIN public.sandbox_workload_admissions admission ON admission.sandbox_id=s.id AND admission.workspace_id=s.workspace_id
  LEFT JOIN LATERAL (
    SELECT array_agg(entry.name ORDER BY entry.name) names,array_agg(entry.path ORDER BY entry.name) paths,
      array_agg(entry.media_type ORDER BY entry.name) media_types,array_agg(entry.required ORDER BY entry.name) required
    FROM public.sandbox_artifact_contract_entries entry
    WHERE entry.sandbox_id=claimed.sandbox_id AND entry.workspace_id=claimed.workspace_id
  ) artifacts ON true
$$;

CREATE OR REPLACE FUNCTION development_controller_commit_execution_v1(
  p_build_id uuid,p_worker_id text,p_lease_token uuid,p_expected_version bigint,
  p_node_id uuid,p_sandbox_id uuid,p_document jsonb,p_artifacts jsonb
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE artifact jsonb; stored boolean; completed boolean; content bytea; artifact_count bigint; evidence_count bigint;
  generation bigint; binding_count bigint;
BEGIN
  IF jsonb_typeof(p_artifacts)<>'array' OR jsonb_array_length(p_artifacts) NOT BETWEEN 1 AND 100 THEN
    RAISE EXCEPTION 'invalid Development Artifact set' USING ERRCODE='22023';
  END IF;
  SELECT execution_generation INTO generation FROM public.development_build_jobs
    WHERE build_id=p_build_id AND worker_id=p_worker_id AND lease_token=p_lease_token AND
      (completed_at IS NOT NULL OR lease_expires_at>clock_timestamp());
  IF generation IS NULL OR jsonb_typeof(p_document#>'{outputs,images}')<>'array' OR jsonb_array_length(p_document#>'{outputs,images}')<>2 OR
     jsonb_typeof(p_document#>'{outputs,refreshArtifacts}')<>'object' THEN
    RAISE EXCEPTION 'Development candidate image finalization was fenced' USING ERRCODE='40001';
  END IF;
  SELECT count(*) INTO binding_count FROM public.development_candidate_image_bindings binding
    WHERE binding.build_id=p_build_id AND binding.attempt_generation=generation AND
      binding.image_index_digest=p_document#>>'{outputs,imageIndexDigest}' AND
      EXISTS(SELECT 1 FROM jsonb_array_elements(p_document#>'{outputs,images}') image
        WHERE image->>'platform'=binding.platform AND image->>'digest'=binding.image_child_digest) AND
      p_document#>>ARRAY['outputs','refreshArtifacts',binding.platform,'imageDigest']=binding.image_child_digest;
  IF binding_count<>2 OR (SELECT count(DISTINCT image->>'platform') FROM jsonb_array_elements(p_document#>'{outputs,images}') image)<>2 THEN
    RAISE EXCEPTION 'Development terminal images do not match the resolved candidate binding' USING ERRCODE='40001';
  END IF;
  FOR artifact IN SELECT value FROM jsonb_array_elements(p_artifacts) LOOP
    BEGIN content:=decode(artifact->>'contentBase64','base64');
    EXCEPTION WHEN others THEN RAISE EXCEPTION 'invalid Development Artifact encoding' USING ERRCODE='22023'; END;
    SELECT public.development_controller_store_artifact_v1(p_build_id,p_worker_id,p_lease_token,
      (artifact->>'id')::uuid,artifact->>'role',artifact->>'kind',artifact->>'contentDigest',content) INTO stored;
    IF stored IS NOT TRUE THEN RAISE EXCEPTION 'Development Artifact commit was fenced' USING ERRCODE='40001'; END IF;
  END LOOP;
  SELECT public.development_controller_finalize_v1(p_build_id,p_worker_id,p_lease_token,p_expected_version,
    p_node_id,p_sandbox_id,p_document) INTO completed;
  IF completed IS NOT TRUE THEN RAISE EXCEPTION 'Development finalization was fenced' USING ERRCODE='40001'; END IF;
  SELECT count(*) INTO artifact_count FROM jsonb_array_elements(p_artifacts);
  SELECT count(*) INTO evidence_count FROM public.development_build_evidence_artifacts WHERE build_id=p_build_id;
  IF artifact_count<>evidence_count OR
     (SELECT count(DISTINCT value->>'role') FROM jsonb_array_elements(p_artifacts))<>artifact_count OR
     (SELECT count(DISTINCT value->>'id') FROM jsonb_array_elements(p_artifacts))<>artifact_count OR
     EXISTS(SELECT 1 FROM jsonb_array_elements(p_artifacts) supplied WHERE NOT EXISTS(
       SELECT 1 FROM public.development_build_evidence_artifacts evidence
       WHERE evidence.build_id=p_build_id AND evidence.role=supplied.value->>'role' AND
         evidence.artifact_id::text=supplied.value->>'id' AND evidence.kind=supplied.value->>'kind' AND
         evidence.content_digest=supplied.value->>'contentDigest')) THEN
    RAISE EXCEPTION 'Development Artifact replay does not match terminal evidence' USING ERRCODE='40001';
  END IF;
  RETURN true;
END $$;

REVOKE EXECUTE ON FUNCTION development_controller_finalize_v1(uuid,text,uuid,bigint,uuid,uuid,jsonb)
  FROM blazn_development_controller;
