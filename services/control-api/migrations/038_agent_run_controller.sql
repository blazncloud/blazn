-- Freeze an executable Agent/Harness selection before a controller is allowed
-- to allocate a Sandbox.  These rows are immutable provenance; mutable lease
-- state lives separately in agent_run_jobs.
CREATE TABLE agent_run_bindings (
  run_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  agent_id uuid NOT NULL,
  agent_version_id uuid NOT NULL,
  agent_version_digest text NOT NULL CHECK (agent_version_digest ~ '^sha256:[0-9a-f]{64}$'),
  harness_definition_id uuid NOT NULL,
  harness_version_id uuid NOT NULL,
  harness_version_digest text NOT NULL CHECK (harness_version_digest ~ '^sha256:[0-9a-f]{64}$'),
  harness_profile_id uuid NOT NULL,
  harness_profile_digest text NOT NULL CHECK (harness_profile_digest ~ '^sha256:[0-9a-f]{64}$'),
  harness_profile_resource_version bigint NOT NULL CHECK (harness_profile_resource_version > 0),
  template_version_id uuid NOT NULL,
  template_digest text NOT NULL CHECK (template_digest ~ '^sha256:[0-9a-f]{64}$'),
  required_capabilities text[] NOT NULL,
  selected_capabilities text[] NOT NULL,
  model_route_id uuid NOT NULL,
  model_route_version bigint NOT NULL CHECK (model_route_version > 0),
  model_protocol text NOT NULL CHECK (model_protocol IN ('openai-responses','openai-chat','anthropic-messages')),
  bound_sandbox_id uuid,
  bound_node_id uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  bound_at timestamptz,
  UNIQUE (run_id,workspace_id,project_id),
  FOREIGN KEY (run_id,workspace_id,project_id) REFERENCES runs(id,workspace_id,project_id) ON DELETE CASCADE,
  FOREIGN KEY (agent_version_id,workspace_id) REFERENCES agent_versions(id,workspace_id),
  FOREIGN KEY (agent_id,workspace_id) REFERENCES agents(id,workspace_id),
  FOREIGN KEY (harness_definition_id,workspace_id) REFERENCES harness_definitions(id,workspace_id),
  FOREIGN KEY (harness_version_id,workspace_id) REFERENCES harness_versions(id,workspace_id),
  FOREIGN KEY (harness_profile_id,workspace_id) REFERENCES harness_profiles(id,workspace_id),
  FOREIGN KEY (template_version_id,workspace_id) REFERENCES sandbox_template_versions(id,workspace_id),
  FOREIGN KEY (bound_sandbox_id,workspace_id) REFERENCES sandboxes(id,workspace_id),
  FOREIGN KEY (bound_node_id,workspace_id) REFERENCES nodes(id,workspace_id),
  CHECK ((bound_sandbox_id IS NULL)=(bound_node_id IS NULL) AND (bound_sandbox_id IS NULL)=(bound_at IS NULL)),
  CHECK (cardinality(required_capabilities) BETWEEN 1 AND 32),
  CHECK (cardinality(selected_capabilities) BETWEEN 1 AND 32)
);

CREATE TABLE agent_run_jobs (
  run_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  worker_id text CHECK (worker_id IS NULL OR worker_id ~ '^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$' OR worker_id ~ '^[a-z0-9]$'),
  lease_token uuid,
  lease_expires_at timestamptz,
  attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 5),
  available_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,62}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (run_id,workspace_id,project_id) REFERENCES agent_run_bindings(run_id,workspace_id,project_id) ON DELETE CASCADE,
  CHECK ((worker_id IS NULL)=(lease_token IS NULL) AND (worker_id IS NULL)=(lease_expires_at IS NULL)),
  CHECK (completed_at IS NULL OR worker_id IS NULL)
);
CREATE INDEX agent_run_jobs_claim_idx ON agent_run_jobs(available_at,created_at,run_id) WHERE completed_at IS NULL;

-- The original Run invariant correctly requires placement for every active or
-- terminal sandbox Run.  Admission can nevertheless fail before placement.
-- Record that narrow exception in a controller-only row, and use deferred
-- cross-table validation so a failed Run, its receipt, and marker commit as one
-- atomic authority change.  No caller may manufacture a node or Sandbox ID.
CREATE TABLE agent_run_preallocation_failures (
  run_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  error_code text NOT NULL CHECK (error_code IN ('controller_attempts_exhausted','compatibility_revoked','sandbox_admission_failed')),
  agent_version_id uuid NOT NULL,
  agent_version_digest text NOT NULL CHECK (agent_version_digest ~ '^sha256:[0-9a-f]{64}$'),
  harness_profile_id uuid NOT NULL,
  harness_profile_digest text NOT NULL CHECK (harness_profile_digest ~ '^sha256:[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (run_id,workspace_id,project_id) REFERENCES agent_run_bindings(run_id,workspace_id,project_id) ON DELETE CASCADE
);

ALTER TABLE runs DROP CONSTRAINT runs_check4;
ALTER TABLE runs DROP CONSTRAINT runs_check6;
ALTER TABLE runs ADD CONSTRAINT runs_node_placement_or_preallocation_failure_check CHECK (
  status='queued' OR proof_class NOT IN ('local','sandbox') OR node_id IS NOT NULL OR
  (proof_class='sandbox' AND status IN ('failed','cancelled') AND node_id IS NULL));
ALTER TABLE runs ADD CONSTRAINT runs_sandbox_placement_or_preallocation_failure_check CHECK (
  status='queued' OR proof_class<>'sandbox' OR sandbox_id IS NOT NULL OR status IN ('failed','cancelled'));

CREATE FUNCTION validate_agent_run_preallocation_failure() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target_id uuid; run_row public.runs%ROWTYPE; marker public.agent_run_preallocation_failures%ROWTYPE; receipt public.run_receipts%ROWTYPE;
BEGIN
  IF TG_TABLE_NAME='runs' THEN
    target_id:=coalesce(NEW.id,OLD.id);
  ELSE
    target_id:=coalesce(NEW.run_id,OLD.run_id);
  END IF;
  SELECT * INTO run_row FROM public.runs WHERE id=target_id;
  IF NOT FOUND THEN RETURN CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END; END IF;
  SELECT * INTO marker FROM public.agent_run_preallocation_failures WHERE run_id=target_id;
  SELECT * INTO receipt FROM public.run_receipts WHERE run_id=target_id;
  IF run_row.proof_class='sandbox' AND run_row.status='failed' AND run_row.node_id IS NULL AND run_row.sandbox_id IS NULL THEN
    IF marker.run_id IS NULL OR receipt.run_id IS NULL OR marker.workspace_id<>run_row.workspace_id OR marker.project_id<>run_row.project_id OR
       marker.error_code<>run_row.error_code OR receipt.outcome<>'failed' OR receipt.proof_class<>'sandbox' OR
       receipt.receipt->>'schemaVersion'<>'blazn.run/receipt/v1alpha1' THEN
      RAISE EXCEPTION 'placementless failed Agent Run requires exact controller admission failure authority' USING ERRCODE='23514';
    END IF;
  ELSIF marker.run_id IS NOT NULL THEN
    RAISE EXCEPTION 'Agent Run preallocation failure marker is inconsistent with Run placement' USING ERRCODE='23514';
  END IF;
  RETURN CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
END $$;

CREATE CONSTRAINT TRIGGER agent_run_preallocation_failure_from_run
AFTER INSERT OR UPDATE ON runs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_agent_run_preallocation_failure();
CREATE CONSTRAINT TRIGGER agent_run_preallocation_failure_from_marker
AFTER INSERT OR UPDATE OR DELETE ON agent_run_preallocation_failures DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_agent_run_preallocation_failure();
CREATE CONSTRAINT TRIGGER agent_run_preallocation_failure_from_receipt
AFTER INSERT OR UPDATE OR DELETE ON run_receipts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_agent_run_preallocation_failure();

CREATE FUNCTION agent_run_append_event(p_run_id uuid,p_workspace_id uuid,p_project_id uuid,p_type text,p_payload jsonb)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE next_sequence bigint;
BEGIN
  IF p_type !~ '^agent\.run\.[a-z0-9_.-]{1,76}$' OR p_payload IS NULL OR jsonb_typeof(p_payload)<>'object' OR
     public.workspace_json_contains_secret_key(p_payload) THEN
    RAISE EXCEPTION 'invalid Agent Run event' USING ERRCODE='22023';
  END IF;
  SELECT coalesce(max(sequence)+1,0) INTO next_sequence FROM public.run_events WHERE run_id=p_run_id;
  INSERT INTO public.run_events(run_id,workspace_id,project_id,sequence,type,payload)
    VALUES(p_run_id,p_workspace_id,p_project_id,next_sequence,p_type,p_payload);
END $$;

-- API-side admission.  It freezes exact immutable identities and rejects an
-- incompatible selection before a worker can observe (and allocate for) it.
CREATE FUNCTION agent_run_enqueue(p_run_id uuid,p_workspace_id uuid,p_agent_version_id uuid,p_harness_profile_id uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target public.runs%ROWTYPE; av public.agent_versions%ROWTYPE; agent public.agents%ROWTYPE;
  profile public.harness_profiles%ROWTYPE; hv public.harness_versions%ROWTYPE; definition public.harness_definitions%ROWTYPE;
  selected_template_id uuid; selected_template_hash text; required text[]; available text[]; agent_tools text[]; profile_tools text[]; expected_outputs text[];
  route_id uuid; route_version bigint; protocol text;
BEGIN
  SELECT * INTO target FROM public.runs WHERE id=p_run_id AND workspace_id=p_workspace_id FOR UPDATE;
  IF NOT FOUND OR target.proof_class<>'sandbox' OR target.status<>'queued' OR target.node_id IS NOT NULL OR target.sandbox_id IS NOT NULL OR
     EXISTS(SELECT 1 FROM public.development_builds WHERE run_id=target.id) THEN RETURN false; END IF;
  SELECT * INTO av FROM public.agent_versions WHERE id=p_agent_version_id AND workspace_id=p_workspace_id FOR SHARE;
  IF NOT FOUND THEN RETURN false; END IF;
  SELECT * INTO agent FROM public.agents WHERE id=av.agent_id AND workspace_id=p_workspace_id AND status='active' FOR SHARE;
  IF NOT FOUND THEN RETURN false; END IF;
  SELECT * INTO profile FROM public.harness_profiles WHERE id=p_harness_profile_id AND workspace_id=p_workspace_id AND status='approved' FOR SHARE;
  IF NOT FOUND THEN RETURN false; END IF;
  SELECT * INTO hv FROM public.harness_versions WHERE id=profile.harness_version_id AND workspace_id=p_workspace_id FOR SHARE;
  IF NOT FOUND THEN RETURN false; END IF;
  SELECT * INTO definition FROM public.harness_definitions WHERE id=hv.definition_id AND workspace_id=p_workspace_id AND status='approved' FOR SHARE;
  IF NOT FOUND THEN RETURN false; END IF;

  IF jsonb_typeof(av.document->'allowedHarnessProfiles') IS DISTINCT FROM 'array' OR
     jsonb_typeof(av.document->'requiredCapabilities') IS DISTINCT FROM 'array' OR
     jsonb_typeof(av.document->'tools') IS DISTINCT FROM 'array' OR
     jsonb_typeof(hv.document->'capabilities') IS DISTINCT FROM 'array' OR
     jsonb_typeof(profile.document->'tools') IS DISTINCT FROM 'array' OR
     NOT EXISTS(SELECT 1 FROM jsonb_array_elements(av.document->'allowedHarnessProfiles') pin
      WHERE pin->>'id'=profile.id::text AND pin->>'digest'=profile.digest) OR
     profile.document->>'digest' IS DISTINCT FROM profile.digest OR hv.document->>'digest' IS DISTINCT FROM hv.digest OR
     av.document->>'digest' IS DISTINCT FROM av.digest OR profile.document->>'harnessVersionId' IS DISTINCT FROM hv.id::text OR
     hv.document->>'definitionId' IS DISTINCT FROM definition.id::text THEN RETURN false; END IF;
  SELECT coalesce(array_agg(value ORDER BY value),'{}'::text[]) INTO required FROM jsonb_array_elements_text(av.document->'requiredCapabilities') value;
  SELECT coalesce(array_agg(value ORDER BY value),'{}'::text[]) INTO available FROM jsonb_array_elements_text(hv.document->'capabilities') value;
  IF cardinality(required)=0 OR NOT required <@ available THEN RETURN false; END IF;
  SELECT coalesce(array_agg(value ORDER BY value),'{}'::text[]) INTO agent_tools FROM jsonb_array_elements_text(av.document->'tools') value;
  SELECT coalesce(array_agg(value ORDER BY value),'{}'::text[]) INTO profile_tools FROM jsonb_array_elements_text(profile.document->'tools') value;
  IF agent_tools IS DISTINCT FROM profile_tools THEN RETURN false; END IF;
  SELECT coalesce(array_agg(DISTINCT value ORDER BY value),'{}'::text[]) INTO expected_outputs
    FROM jsonb_array_elements_text(coalesce(av.document#>'{outputContract,artifacts}','[]'::jsonb)) value;
  IF av.document#>>'{outputContract,summary}'='true' AND NOT 'summary'=ANY(expected_outputs) THEN expected_outputs:=array_append(expected_outputs,'summary'); END IF;
  IF av.document#>>'{outputContract,patch}'='true' AND NOT 'patch'=ANY(expected_outputs) THEN expected_outputs:=array_append(expected_outputs,'patch'); END IF;
  SELECT array_agg(value ORDER BY value) INTO expected_outputs FROM unnest(expected_outputs) value;
  IF expected_outputs IS NULL OR ARRAY(SELECT unnest(target.output_names) ORDER BY 1) IS DISTINCT FROM expected_outputs THEN RETURN false; END IF;
  BEGIN
    selected_template_id:=(av.document#>>'{template,versionId}')::uuid;
    route_id:=(profile.document#>>'{model,routeId}')::uuid;
    route_version:=(profile.document#>>'{model,routeVersion}')::bigint;
  EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN RETURN false; END;
  selected_template_hash:=av.document#>>'{template,digest}'; protocol:=profile.document#>>'{model,protocol}';
  IF selected_template_hash !~ '^sha256:[0-9a-f]{64}$' OR route_version<1 OR protocol NOT IN ('openai-responses','openai-chat','anthropic-messages') OR
     profile.document#>>'{model,routeId}' IS DISTINCT FROM av.document#>>'{modelPolicy,routeId}' OR
     profile.document#>>'{model,routeVersion}' IS DISTINCT FROM av.document#>>'{modelPolicy,routeVersion}' OR
     NOT coalesce(av.document#>'{modelPolicy,requiredProtocols}','[]'::jsonb) @> to_jsonb(ARRAY[protocol]) OR
     NOT coalesce(hv.document#>'{compatibility,proxyProtocols}','[]'::jsonb) @> to_jsonb(ARRAY[protocol]) OR
     NOT EXISTS(SELECT 1 FROM public.sandbox_template_versions v WHERE v.id=selected_template_id AND v.workspace_id=p_workspace_id
       AND 'sha256:'||trim(v.content_digest)=selected_template_hash) THEN RETURN false; END IF;

  INSERT INTO public.agent_run_bindings(run_id,workspace_id,project_id,agent_id,agent_version_id,agent_version_digest,
    harness_definition_id,harness_version_id,harness_version_digest,harness_profile_id,harness_profile_digest,
    harness_profile_resource_version,template_version_id,template_digest,required_capabilities,selected_capabilities,
    model_route_id,model_route_version,model_protocol)
  VALUES(target.id,target.workspace_id,target.project_id,agent.id,av.id,av.digest,definition.id,hv.id,hv.digest,profile.id,
    profile.digest,profile.resource_version,selected_template_id,selected_template_hash,required,available,route_id,route_version,protocol)
  ON CONFLICT(run_id) DO NOTHING;
  IF NOT FOUND THEN
    RETURN EXISTS(SELECT 1 FROM public.agent_run_bindings b WHERE b.run_id=target.id AND b.agent_version_id=av.id
      AND b.harness_profile_id=profile.id AND b.agent_version_digest=av.digest AND b.harness_profile_digest=profile.digest);
  END IF;
  INSERT INTO public.agent_run_jobs(run_id,workspace_id,project_id) VALUES(target.id,target.workspace_id,target.project_id);
  PERFORM public.agent_run_append_event(target.id,target.workspace_id,target.project_id,'agent.run.compatibility-accepted',
    jsonb_build_object('agentVersionId',av.id,'agentVersionDigest',av.digest,'harnessVersionId',hv.id,
      'harnessVersionDigest',hv.digest,'harnessProfileId',profile.id,'harnessProfileDigest',profile.digest,
      'templateVersionId',selected_template_id,'templateDigest',selected_template_hash));
  RETURN true;
END $$;

CREATE FUNCTION agent_run_controller_claim(p_worker_id text,p_lease_seconds integer)
RETURNS TABLE(run_id uuid,workspace_id uuid,project_id uuid,run_version bigint,lease_token uuid,lease_expires_at timestamptz,
  attempt integer,requested_by uuid,plan_digest text,agent_version_id uuid,agent_version_digest text,agent_version jsonb,
  harness_definition_id uuid,harness_version_id uuid,harness_version_digest text,harness_version jsonb,
  harness_profile_id uuid,harness_profile_digest text,harness_profile jsonb,template_version_id uuid,template_digest text,
  model_route_id uuid,model_route_version bigint,model_protocol text,bound_sandbox_id uuid,bound_node_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE selected public.agent_run_jobs%ROWTYPE; effective_now timestamptz:=clock_timestamp();
  exhausted record; exhausted_receipt jsonb;
BEGIN
  IF (p_worker_id !~ '^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$' AND p_worker_id !~ '^[a-z0-9]$') OR
     p_lease_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid Agent Run controller claim' USING ERRCODE='22023'; END IF;
  -- Cancellation is authoritative; retire its queued controller work without rewriting the Run.
  UPDATE public.agent_run_jobs job SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,updated_at=effective_now
    FROM public.runs run WHERE job.run_id=run.id AND job.completed_at IS NULL AND run.status IN ('succeeded','failed','cancelled');
  -- An expired fifth lease is failed by a new transaction, never by the stale
  -- worker.  This closes the queue without granting a sixth execution attempt.
  SELECT run.*,job.attempt_count INTO exhausted FROM public.agent_run_jobs job JOIN public.runs run ON run.id=job.run_id
    WHERE job.completed_at IS NULL AND job.attempt_count>=5 AND job.lease_expires_at<=effective_now
      AND run.status IN ('queued','running') ORDER BY job.available_at,job.created_at,job.run_id
    FOR UPDATE OF job,run SKIP LOCKED LIMIT 1;
  IF FOUND THEN
    exhausted_receipt:=jsonb_build_object('schemaVersion','blazn.run/receipt/v1alpha1','proofClass','sandbox','outcome','failed',
      'planDigest',exhausted.plan_digest,'artifactIds','[]'::jsonb,'summary',jsonb_build_object('steps',0,'warnings',jsonb_build_array('controller attempts exhausted')));
    INSERT INTO public.run_receipts(run_id,workspace_id,project_id,proof_class,outcome,plan_digest,receipt)
      VALUES(exhausted.id,exhausted.workspace_id,exhausted.project_id,'sandbox','failed',exhausted.plan_digest,exhausted_receipt);
    INSERT INTO public.agent_run_preallocation_failures(run_id,workspace_id,project_id,error_code,agent_version_id,
      agent_version_digest,harness_profile_id,harness_profile_digest)
      SELECT binding.run_id,binding.workspace_id,binding.project_id,'controller_attempts_exhausted',binding.agent_version_id,
        binding.agent_version_digest,binding.harness_profile_id,binding.harness_profile_digest
      FROM public.agent_run_bindings binding WHERE binding.run_id=exhausted.id AND binding.bound_sandbox_id IS NULL;
    IF NOT FOUND THEN RAISE EXCEPTION 'exhausted Agent Run already has placement' USING ERRCODE='40001'; END IF;
    UPDATE public.runs SET status='failed',version=version+1,completed_at=effective_now,error_code='controller_attempts_exhausted' WHERE id=exhausted.id;
    UPDATE public.agent_run_jobs AS exhausted_job SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,
      last_error_code='controller_attempts_exhausted',updated_at=effective_now WHERE exhausted_job.run_id=exhausted.id;
    PERFORM public.agent_run_append_event(exhausted.id,exhausted.workspace_id,exhausted.project_id,'agent.run.failed',
      jsonb_build_object('errorCode','controller_attempts_exhausted'));
  END IF;
  SELECT job.* INTO selected FROM public.agent_run_jobs job JOIN public.runs run ON run.id=job.run_id
    WHERE job.completed_at IS NULL AND job.available_at<=effective_now AND job.attempt_count<5
      AND (job.lease_expires_at IS NULL OR job.lease_expires_at<=effective_now) AND run.status IN ('queued','running')
    ORDER BY job.available_at,job.created_at,job.run_id FOR UPDATE OF job SKIP LOCKED LIMIT 1;
  IF NOT FOUND THEN RETURN; END IF;
  UPDATE public.agent_run_jobs job SET worker_id=p_worker_id,lease_token=gen_random_uuid(),
    lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds),attempt_count=job.attempt_count+1,updated_at=effective_now
    WHERE job.run_id=selected.run_id RETURNING job.lease_token,job.lease_expires_at,job.attempt_count
    INTO selected.lease_token,selected.lease_expires_at,selected.attempt_count;
  PERFORM public.agent_run_append_event(selected.run_id,selected.workspace_id,selected.project_id,
    CASE WHEN selected.attempt_count=1 THEN 'agent.run.claimed' ELSE 'agent.run.lease-recovered' END,
    jsonb_build_object('attempt',selected.attempt_count));
  RETURN QUERY SELECT run.id,run.workspace_id,run.project_id,run.version,selected.lease_token,selected.lease_expires_at,
    selected.attempt_count,run.requested_by,run.plan_digest,b.agent_version_id,b.agent_version_digest,av.document,
    b.harness_definition_id,b.harness_version_id,b.harness_version_digest,hv.document,b.harness_profile_id,
    b.harness_profile_digest,hp.document,b.template_version_id,b.template_digest,b.model_route_id,b.model_route_version,
    b.model_protocol,b.bound_sandbox_id,b.bound_node_id
    FROM public.runs run JOIN public.agent_run_bindings b ON b.run_id=run.id
    JOIN public.agent_versions av ON av.id=b.agent_version_id JOIN public.harness_versions hv ON hv.id=b.harness_version_id
    JOIN public.harness_profiles hp ON hp.id=b.harness_profile_id WHERE run.id=selected.run_id;
END $$;

CREATE FUNCTION agent_run_controller_renew(p_run_id uuid,p_worker_id text,p_lease_token uuid,p_lease_seconds integer)
RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE renewed timestamptz; effective_now timestamptz:=clock_timestamp();
BEGIN
  IF p_lease_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid Agent Run lease renewal' USING ERRCODE='22023'; END IF;
  UPDATE public.agent_run_jobs job SET lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds),updated_at=effective_now
    FROM public.runs run WHERE job.run_id=p_run_id AND run.id=job.run_id AND run.status IN ('queued','running')
      AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token AND job.completed_at IS NULL AND job.lease_expires_at>effective_now
    RETURNING job.lease_expires_at INTO renewed;
  RETURN renewed;
END $$;

CREATE FUNCTION agent_run_controller_bind_sandbox(p_run_id uuid,p_worker_id text,p_lease_token uuid,p_expected_run_version bigint,
  p_node_id uuid,p_sandbox_id uuid) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target record; effective_now timestamptz:=clock_timestamp();
BEGIN
  SELECT run.*,b.template_version_id,b.template_digest,b.bound_sandbox_id,b.bound_node_id INTO target
    FROM public.runs run JOIN public.agent_run_bindings b ON b.run_id=run.id JOIN public.agent_run_jobs job ON job.run_id=run.id
    WHERE run.id=p_run_id AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token AND job.completed_at IS NULL
      AND job.lease_expires_at>effective_now FOR UPDATE OF run,b,job;
  IF NOT FOUND THEN RETURN false; END IF;
  IF target.bound_sandbox_id IS NOT NULL THEN
    RETURN target.bound_sandbox_id=p_sandbox_id AND target.bound_node_id=p_node_id AND target.sandbox_id=p_sandbox_id AND target.node_id=p_node_id;
  END IF;
  IF target.version<>p_expected_run_version OR target.status<>'queued' OR
     NOT EXISTS(SELECT 1 FROM public.nodes n WHERE n.id=p_node_id AND n.workspace_id=target.workspace_id
       AND n.lifecycle_state='active' AND n.trust_state='verified' AND n.agent_eligible) OR
     NOT EXISTS(SELECT 1 FROM public.sandboxes s WHERE s.id=p_sandbox_id AND s.workspace_id=target.workspace_id
       AND s.requested_by=target.requested_by AND s.state IN ('ready','running') AND s.expires_at>effective_now
       AND s.template_version_id=target.template_version_id AND 'sha256:'||trim(s.template_digest)=target.template_digest) THEN RETURN false; END IF;
  UPDATE public.agent_run_bindings SET bound_sandbox_id=p_sandbox_id,bound_node_id=p_node_id,bound_at=effective_now WHERE run_id=p_run_id;
  UPDATE public.runs SET status='running',version=version+1,node_id=p_node_id,sandbox_id=p_sandbox_id,started_at=effective_now
    WHERE id=p_run_id AND version=p_expected_run_version AND status='queued';
  IF NOT FOUND THEN RAISE EXCEPTION 'canonical Agent Run changed' USING ERRCODE='40001'; END IF;
  PERFORM public.agent_run_append_event(target.id,target.workspace_id,target.project_id,'agent.run.sandbox-bound',
    jsonb_build_object('sandboxId',p_sandbox_id,'nodeId',p_node_id));
  RETURN true;
END $$;

CREATE FUNCTION agent_run_controller_retry(p_run_id uuid,p_worker_id text,p_lease_token uuid,p_delay_seconds integer,p_error_code text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target record; effective_now timestamptz:=clock_timestamp(); receipt jsonb;
BEGIN
  IF p_delay_seconds NOT BETWEEN 0 AND 3600 OR p_error_code !~ '^[a-z][a-z0-9_]{0,62}$' THEN
    RAISE EXCEPTION 'invalid Agent Run retry' USING ERRCODE='22023'; END IF;
  SELECT run.*,job.attempt_count INTO target FROM public.runs run JOIN public.agent_run_jobs job ON job.run_id=run.id
    WHERE run.id=p_run_id AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token AND job.completed_at IS NULL
      AND job.lease_expires_at>effective_now FOR UPDATE OF run,job;
  IF NOT FOUND THEN RETURN 'fenced'; END IF;
  IF target.status IN ('succeeded','failed','cancelled') THEN
    UPDATE public.agent_run_jobs SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,updated_at=effective_now WHERE run_id=p_run_id;
    RETURN 'terminal';
  END IF;
  IF target.attempt_count>=5 THEN
    receipt:=jsonb_build_object('schemaVersion','blazn.run/receipt/v1alpha1','proofClass','sandbox','outcome','failed',
      'planDigest',target.plan_digest,'artifactIds','[]'::jsonb,'summary',jsonb_build_object('steps',0,'warnings',jsonb_build_array('controller attempts exhausted')));
    INSERT INTO public.run_receipts(run_id,workspace_id,project_id,proof_class,outcome,plan_digest,receipt)
      VALUES(target.id,target.workspace_id,target.project_id,'sandbox','failed',target.plan_digest,receipt);
    INSERT INTO public.agent_run_preallocation_failures(run_id,workspace_id,project_id,error_code,agent_version_id,
      agent_version_digest,harness_profile_id,harness_profile_digest)
      SELECT binding.run_id,binding.workspace_id,binding.project_id,'controller_attempts_exhausted',binding.agent_version_id,
        binding.agent_version_digest,binding.harness_profile_id,binding.harness_profile_digest
      FROM public.agent_run_bindings binding WHERE binding.run_id=target.id AND binding.bound_sandbox_id IS NULL;
    IF NOT FOUND THEN RAISE EXCEPTION 'exhausted Agent Run already has placement' USING ERRCODE='40001'; END IF;
    UPDATE public.runs SET status='failed',version=version+1,completed_at=effective_now,error_code='controller_attempts_exhausted'
      WHERE id=target.id AND status IN ('queued','running');
    UPDATE public.agent_run_jobs SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,last_error_code=p_error_code,updated_at=effective_now WHERE run_id=target.id;
    PERFORM public.agent_run_append_event(target.id,target.workspace_id,target.project_id,'agent.run.failed',
      jsonb_build_object('errorCode','controller_attempts_exhausted'));
    RETURN 'failed';
  END IF;
  UPDATE public.agent_run_jobs SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,
    available_at=effective_now+make_interval(secs=>p_delay_seconds),last_error_code=p_error_code,updated_at=effective_now WHERE run_id=target.id;
  PERFORM public.agent_run_append_event(target.id,target.workspace_id,target.project_id,'agent.run.retry-scheduled',
    jsonb_build_object('attempt',target.attempt_count,'errorCode',p_error_code));
  RETURN 'retry_scheduled';
END $$;

CREATE FUNCTION agent_run_controller_finalize(p_run_id uuid,p_worker_id text,p_lease_token uuid,p_expected_run_version bigint,
  p_outcome text,p_error_code text,p_artifact_ids uuid[],p_steps bigint,p_warnings text[]) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target record; effective_now timestamptz:=clock_timestamp(); names text[]; receipt jsonb;
BEGIN
  IF p_outcome NOT IN ('succeeded','failed') OR p_steps<0 OR cardinality(p_artifact_ids)>100 OR cardinality(p_warnings)>100 OR
     (p_outcome='failed')<>(p_error_code IS NOT NULL AND p_error_code ~ '^[a-z][a-z0-9_]{0,62}$') THEN
    RAISE EXCEPTION 'invalid Agent Run finalization' USING ERRCODE='22023'; END IF;
  SELECT run.*,b.agent_version_id,b.agent_version_digest,b.harness_version_id,b.harness_version_digest,
    b.harness_profile_id,b.harness_profile_digest,b.bound_sandbox_id,b.bound_node_id INTO target
    FROM public.runs run JOIN public.agent_run_bindings b ON b.run_id=run.id JOIN public.agent_run_jobs job ON job.run_id=run.id
    WHERE run.id=p_run_id AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token AND job.completed_at IS NULL
      AND job.lease_expires_at>effective_now FOR UPDATE OF run,b,job;
  IF NOT FOUND OR target.version<>p_expected_run_version OR target.status<>'running' OR target.bound_sandbox_id IS NULL THEN RETURN false; END IF;
  SELECT coalesce(array_agg(a.name ORDER BY a.name),'{}'::text[]) INTO names FROM public.artifacts a
    WHERE a.id=ANY(p_artifact_ids) AND a.workspace_id=target.workspace_id AND a.project_id=target.project_id
      AND a.source_run_id=target.id AND a.status='ready' AND
      ((a.name='patch' AND a.kind='agent.patch' AND a.media_type='document') OR
       (a.name='summary' AND a.kind='agent.summary' AND a.media_type='document') OR
       (a.name NOT IN ('patch','summary') AND a.kind='agent.output'));
  IF cardinality(names)<>cardinality(p_artifact_ids) OR names<>ARRAY(SELECT unnest(target.output_names) ORDER BY 1) THEN RETURN false; END IF;
  receipt:=jsonb_build_object('schemaVersion','blazn.run/receipt/v1alpha1','proofClass','sandbox','outcome',p_outcome,
    'planDigest',target.plan_digest,'artifactIds',to_jsonb(p_artifact_ids),
    'summary',jsonb_build_object('steps',p_steps,'warnings',to_jsonb(p_warnings)));
  INSERT INTO public.run_receipts(run_id,workspace_id,project_id,proof_class,outcome,plan_digest,receipt)
    VALUES(target.id,target.workspace_id,target.project_id,'sandbox',p_outcome,target.plan_digest,receipt);
  UPDATE public.runs SET status=p_outcome,version=version+1,completed_at=effective_now,error_code=p_error_code
    WHERE id=target.id AND version=p_expected_run_version AND status='running';
  IF NOT FOUND THEN RAISE EXCEPTION 'canonical Agent Run changed' USING ERRCODE='40001'; END IF;
  UPDATE public.agent_run_jobs SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,updated_at=effective_now WHERE run_id=target.id;
  PERFORM public.agent_run_append_event(target.id,target.workspace_id,target.project_id,'agent.run.'||p_outcome,
    jsonb_build_object('artifactIds',to_jsonb(p_artifact_ids),'steps',p_steps));
  RETURN true;
END $$;

-- Foundation limitation: the deployment currently provisions one non-login
-- controller workload role.  Until bootstrap gains a dedicated Agent Run role,
-- expose only these SECURITY DEFINER entry points to that role and no tables.
REVOKE ALL ON TABLE agent_run_bindings,agent_run_jobs,agent_run_preallocation_failures FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
REVOKE ALL ON FUNCTION agent_run_append_event(uuid,uuid,uuid,text,jsonb),agent_run_enqueue(uuid,uuid,uuid,uuid),
  validate_agent_run_preallocation_failure(),
  agent_run_controller_claim(text,integer),agent_run_controller_renew(uuid,text,uuid,integer),
  agent_run_controller_bind_sandbox(uuid,text,uuid,bigint,uuid,uuid),agent_run_controller_retry(uuid,text,uuid,integer,text),
  agent_run_controller_finalize(uuid,text,uuid,bigint,text,text,uuid[],bigint,text[])
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
GRANT EXECUTE ON FUNCTION agent_run_enqueue(uuid,uuid,uuid,uuid) TO blazn_runtime;
GRANT EXECUTE ON FUNCTION agent_run_controller_claim(text,integer),agent_run_controller_renew(uuid,text,uuid,integer),
  agent_run_controller_bind_sandbox(uuid,text,uuid,bigint,uuid,uuid),agent_run_controller_retry(uuid,text,uuid,integer,text),
  agent_run_controller_finalize(uuid,text,uuid,bigint,text,text,uuid[],bigint,text[]) TO blazn_development_controller;
