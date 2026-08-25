CREATE TABLE development_sandbox_test_runs (
  build_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  attempt_generation bigint NOT NULL CHECK (attempt_generation > 0),
  platform text NOT NULL CHECK (platform IN ('linux/amd64','linux/arm64')),
  test_name text NOT NULL CHECK (test_name ~ '^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$' OR test_name ~ '^[a-z0-9]$'),
  sandbox_id uuid NOT NULL,
  create_operation_id uuid NOT NULL,
  cleanup_operation_id uuid,
  argv jsonb NOT NULL CHECK (jsonb_typeof(argv)='array' AND jsonb_array_length(argv) BETWEEN 1 AND 64),
  argv_digest text NOT NULL CHECK (argv_digest ~ '^sha256:[0-9a-f]{64}$'),
  timeout_seconds integer NOT NULL CHECK (timeout_seconds BETWEEN 1 AND 600),
  candidate_image_index text NOT NULL CHECK (candidate_image_index ~ '^.+@sha256:[0-9a-f]{64}$'),
  candidate_image_bound boolean NOT NULL DEFAULT false,
  status text NOT NULL DEFAULT 'preparing' CHECK (status IN ('preparing','ready','running','succeeded','failed','cleanup_pending','clean')),
  node_id uuid,
  receipt jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (build_id,attempt_generation,platform,test_name),
  UNIQUE (sandbox_id),
  UNIQUE (create_operation_id),
  UNIQUE (cleanup_operation_id),
  FOREIGN KEY (build_id,workspace_id,project_id) REFERENCES development_builds(id,workspace_id,project_id) ON DELETE CASCADE,
  FOREIGN KEY (sandbox_id,workspace_id) REFERENCES sandboxes(id,workspace_id),
  FOREIGN KEY (create_operation_id,workspace_id,sandbox_id) REFERENCES sandbox_operations(id,workspace_id,sandbox_id),
  FOREIGN KEY (cleanup_operation_id,workspace_id,sandbox_id) REFERENCES sandbox_operations(id,workspace_id,sandbox_id),
  FOREIGN KEY (node_id,workspace_id) REFERENCES nodes(id,workspace_id),
  CHECK (candidate_image_bound OR status IN ('preparing','ready','cleanup_pending','clean')),
  CHECK ((receipt IS NULL)=(status NOT IN ('succeeded','failed','cleanup_pending','clean'))),
  CHECK (receipt IS NULL OR (jsonb_typeof(receipt)='object' AND development_evidence_is_redacted(receipt) AND NOT workspace_json_contains_secret_key(receipt)))
);

CREATE FUNCTION development_collector_prepare_sandbox_v1(
  p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text,p_candidate_image_index text
) RETURNS SETOF development_sandbox_test_runs
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target public.development_builds%ROWTYPE; existing public.development_sandbox_test_runs%ROWTYPE;
  job public.development_build_jobs%ROWTYPE;
  repository record; sandbox_id uuid:=gen_random_uuid(); operation_id uuid:=gen_random_uuid(); architecture text;
  test_definition jsonb; expected_contract jsonb; canonical_contract bytea; contract_digest text; sources jsonb;
  argv_digest text; operation_digest text;
BEGIN
  IF p_attempt_generation<1 OR p_platform NOT IN ('linux/amd64','linux/arm64') OR p_test_name !~ '^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$' AND p_test_name !~ '^[a-z0-9]$' OR
     p_candidate_image_index !~ '^.+@sha256:[0-9a-f]{64}$' THEN
    RAISE EXCEPTION 'invalid Development Sandbox test identity' USING ERRCODE='22023';
  END IF;
  -- This row lock makes the lease-generation check and all Sandbox lifecycle
  -- inserts one atomic operation with respect to release, renewal, and reclaim.
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF NOT FOUND OR job.completed_at IS NOT NULL OR job.lease_expires_at IS NULL OR job.lease_expires_at<=clock_timestamp() OR job.execution_generation<>p_attempt_generation THEN
    RETURN;
  END IF;
  SELECT * INTO existing FROM public.development_sandbox_test_runs
    WHERE build_id=p_build_id AND attempt_generation=p_attempt_generation AND platform=p_platform AND test_name=p_test_name FOR UPDATE;
  IF FOUND THEN
    IF NOT EXISTS(SELECT 1 FROM public.development_builds build WHERE build.id=p_build_id AND build.status IN ('building','testing')) THEN
      RETURN;
    END IF;
    IF existing.candidate_image_index<>p_candidate_image_index THEN
      RAISE EXCEPTION 'Development Sandbox candidate image changed' USING ERRCODE='40001';
    END IF;
    RETURN NEXT existing; RETURN;
  END IF;
  SELECT * INTO target FROM public.development_builds WHERE id=p_build_id FOR SHARE;
  IF NOT FOUND OR target.status NOT IN ('building','testing') OR
     job.build_id IS DISTINCT FROM target.id THEN
    RETURN;
  END IF;
  test_definition:=target.project_snapshot->'tests'->p_test_name;
  IF test_definition IS NULL OR jsonb_typeof(test_definition)<>'object' OR jsonb_typeof(test_definition->'argv')<>'array' OR
     jsonb_array_length(test_definition->'argv') NOT BETWEEN 1 AND 64 OR (test_definition->>'timeoutSeconds')::integer NOT BETWEEN 1 AND 600 OR
     EXISTS(SELECT 1 FROM jsonb_array_elements(test_definition->'argv') value WHERE jsonb_typeof(value)<>'string' OR length(value#>>'{}') NOT BETWEEN 1 AND 1024) THEN
    RAISE EXCEPTION 'Development Sandbox test definition is invalid' USING ERRCODE='22023';
  END IF;
  IF split_part(p_candidate_image_index,'@',1)<>target.registry_repository OR
     p_candidate_image_index<>target.registry_repository||'@'||substring(p_candidate_image_index from '@(sha256:[0-9a-f]{64})$') THEN
    RAISE EXCEPTION 'Development Sandbox candidate repository is invalid' USING ERRCODE='22023';
  END IF;
  architecture:=split_part(p_platform,'/',2);
  SELECT r.name,r.url INTO repository FROM public.sandbox_template_version_repositories r
    WHERE r.version_id=target.template_version_id;
  IF NOT FOUND OR repository.url<>target.source_repository OR
     (SELECT count(*) FROM public.sandbox_template_version_repositories WHERE version_id=target.template_version_id)<>1 THEN
    RAISE EXCEPTION 'Development Sandbox requires one exact source binding' USING ERRCODE='22023';
  END IF;
  SELECT jsonb_build_object('items',coalesce(jsonb_agg(jsonb_build_object('name',a.name,'path',a.path,'mediaType',a.media_type,'required',a.required) ORDER BY a.name),'[]'::jsonb))
    INTO expected_contract FROM public.sandbox_template_version_artifacts a WHERE a.version_id=target.template_version_id;
  canonical_contract:=convert_to(expected_contract::text,'UTF8');
  contract_digest:=encode(digest(canonical_contract,'sha256'),'hex');
  sources:=jsonb_build_array(jsonb_build_object('repository',repository.name,'commit',target.source_commit));
  PERFORM public.sandbox_create_bound_sandbox_for_duration(sandbox_id,target.workspace_id,target.template_version_id,architecture,
    'direct',(test_definition->>'timeoutSeconds')::integer+300,'blazn-poc',sources,
    canonical_contract,contract_digest,target.requested_by);
  argv_digest:='sha256:'||encode(digest(convert_to((test_definition->'argv')::text,'UTF8'),'sha256'),'hex');
  operation_digest:=encode(digest(convert_to(jsonb_build_object('buildId',target.id,'platform',p_platform,'testName',p_test_name,
    'sandboxId',sandbox_id,'sourceCommit',target.source_commit)::text,'UTF8'),'sha256'),'hex');
  INSERT INTO public.sandbox_operations(id,workspace_id,sandbox_id,type,status,expected_sandbox_version,requested_by,idempotency_key,request_digest)
    VALUES(operation_id,target.workspace_id,sandbox_id,'create','pending',1,target.requested_by,
      'development-create-'||operation_digest,operation_digest);
  INSERT INTO public.sandbox_events(id,operation_id,workspace_id,sandbox_id,sequence,type,payload)
    VALUES(gen_random_uuid(),operation_id,target.workspace_id,sandbox_id,0,'sandbox.requested',jsonb_build_object('developmentBuildId',target.id));
  INSERT INTO public.development_sandbox_test_runs(build_id,workspace_id,project_id,attempt_generation,platform,test_name,sandbox_id,
    create_operation_id,argv,argv_digest,timeout_seconds,candidate_image_index)
  VALUES(target.id,target.workspace_id,target.project_id,p_attempt_generation,p_platform,p_test_name,sandbox_id,operation_id,test_definition->'argv',argv_digest,
    (test_definition->>'timeoutSeconds')::integer,p_candidate_image_index)
  RETURNING * INTO existing;
  RETURN NEXT existing;
END $$;

CREATE FUNCTION development_collector_resolve_sandbox_v1(p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text)
RETURNS TABLE(build_id uuid,workspace_id uuid,project_id uuid,platform text,test_name text,sandbox_id uuid,
  status text,sandbox_state text,candidate_image_index text,candidate_image_bound boolean,argv jsonb,argv_digest text,
  timeout_seconds integer,backend_uid text,backend_resource_version text,pod_namespace text,pod_name text,pod_uid text,
  pod_resource_version text,observation_digest text,node_id uuid,receipt jsonb)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
  SELECT run.build_id,run.workspace_id,run.project_id,run.platform,run.test_name,run.sandbox_id,run.status,sandbox.state,
    run.candidate_image_index,run.candidate_image_bound,run.argv,run.argv_digest,run.timeout_seconds,sandbox.backend_uid,
    sandbox.backend_resource_version,admission.pod_namespace,admission.pod_name,admission.pod_uid,
    admission.pod_resource_version,admission.observation_digest::text,run.node_id,run.receipt
  FROM public.development_sandbox_test_runs run JOIN public.sandboxes sandbox ON sandbox.id=run.sandbox_id
  JOIN public.development_builds build ON build.id=run.build_id
  JOIN public.development_build_jobs job ON job.build_id=run.build_id
  LEFT JOIN public.sandbox_workload_admissions admission ON admission.sandbox_id=run.sandbox_id AND admission.workspace_id=run.workspace_id
  WHERE run.build_id=p_build_id AND run.attempt_generation=p_attempt_generation AND run.platform=p_platform AND run.test_name=p_test_name
    AND build.status IN ('building','testing') AND job.completed_at IS NULL
    AND job.execution_generation=p_attempt_generation AND job.lease_expires_at>clock_timestamp()
$$;

CREATE FUNCTION development_collector_authorize_execution_v1(
  p_build_id uuid,p_attempt_generation bigint,p_platform text,p_test_name text,p_sandbox_id uuid
) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE job public.development_build_jobs%ROWTYPE; changed bigint;
BEGIN
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF NOT FOUND OR job.completed_at IS NOT NULL OR job.lease_expires_at IS NULL OR job.lease_expires_at<=clock_timestamp() OR
     job.execution_generation<>p_attempt_generation THEN RETURN false; END IF;
  UPDATE public.development_sandbox_test_runs run SET status='running',updated_at=clock_timestamp()
    WHERE run.build_id=p_build_id AND run.attempt_generation=p_attempt_generation AND run.platform=p_platform AND
      run.test_name=p_test_name AND run.sandbox_id=p_sandbox_id AND run.candidate_image_bound AND run.status IN ('ready','running') AND
      EXISTS(SELECT 1 FROM public.development_builds build WHERE build.id=run.build_id AND build.status IN ('building','testing'));
  GET DIAGNOSTICS changed=ROW_COUNT;
  RETURN changed=1;
END $$;

REVOKE ALL ON TABLE development_sandbox_test_runs FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,
  blazn_sandbox_controller,blazn_development_controller;
REVOKE ALL ON FUNCTION development_collector_prepare_sandbox_v1(uuid,bigint,text,text,text),
  development_collector_resolve_sandbox_v1(uuid,bigint,text,text),development_collector_authorize_execution_v1(uuid,bigint,text,text,uuid)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
GRANT EXECUTE ON FUNCTION development_collector_prepare_sandbox_v1(uuid,bigint,text,text,text),
  development_collector_resolve_sandbox_v1(uuid,bigint,text,text),development_collector_authorize_execution_v1(uuid,bigint,text,text,uuid)
  TO blazn_development_controller;
