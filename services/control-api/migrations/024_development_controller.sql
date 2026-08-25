CREATE TABLE development_build_jobs (
  build_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  worker_id text CHECK (worker_id IS NULL OR worker_id ~ '^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$' OR worker_id ~ '^[a-z0-9]$'),
  lease_token uuid,
  lease_expires_at timestamptz,
  attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 5),
  available_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (build_id,workspace_id,project_id) REFERENCES development_builds(id,workspace_id,project_id) ON DELETE CASCADE,
  CHECK ((worker_id IS NULL)=(lease_token IS NULL) AND (worker_id IS NULL)=(lease_expires_at IS NULL)),
  CHECK (completed_at IS NULL OR worker_id IS NOT NULL)
);
CREATE INDEX development_build_jobs_claim_idx ON development_build_jobs(available_at,created_at,build_id) WHERE completed_at IS NULL;

CREATE FUNCTION development_evidence_is_redacted(p_value jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE SET search_path=pg_catalog,public AS $$
DECLARE item jsonb; entry record; key_name text; string_value text;
BEGIN
  IF p_value IS NULL THEN RETURN false; END IF;
  CASE jsonb_typeof(p_value)
    WHEN 'object' THEN
      FOR entry IN SELECT key,value FROM jsonb_each(p_value) LOOP
        key_name:=lower(regexp_replace(entry.key,'[-_]','','g'));
        IF key_name IN ('authorization','credential','credentials','password','secret','secrets','token','accesstoken','refreshtoken','apikey','objectkey','signedurl','buildkitendpoint','buildkitclientcertificate','registrycredential') OR
           NOT public.development_evidence_is_redacted(entry.value) THEN RETURN false; END IF;
      END LOOP;
    WHEN 'array' THEN
      FOR item IN SELECT value FROM jsonb_array_elements(p_value) LOOP
        IF NOT public.development_evidence_is_redacted(item) THEN RETURN false; END IF;
      END LOOP;
    WHEN 'string' THEN
      string_value:=p_value#>>'{}';
      IF string_value ~* '(^|[?&])(x-amz-signature|x-goog-signature|signature|sig|token|access_token|api_key|credential)=' OR
         string_value ~* '^(tcp|https?)://[^[:space:]]*buildkit' OR string_value ~* '^(unix|npipe)://' OR
         string_value ~* '\m(bearer|basic)[[:space:]]+[A-Za-z0-9+/_=-]+' THEN RETURN false; END IF;
    ELSE NULL;
  END CASE;
  RETURN true;
END $$;

CREATE TABLE development_build_evidence (
  build_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  receipt_digest text NOT NULL CHECK (receipt_digest ~ '^sha256:[0-9a-f]{64}$'),
  evidence_document jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (build_id,workspace_id,project_id),
  FOREIGN KEY (build_id,workspace_id,project_id) REFERENCES development_builds(id,workspace_id,project_id) ON DELETE CASCADE,
  CHECK (jsonb_typeof(evidence_document)='object'),
  CHECK (development_evidence_is_redacted(evidence_document)),
  CHECK (NOT workspace_json_contains_secret_key(evidence_document))
);

CREATE TABLE development_build_evidence_artifacts (
  build_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  role text NOT NULL CHECK (role ~ '^[a-z][a-z0-9-]*(/[A-Za-z0-9._-]+)*$'),
  artifact_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind ~ '^development\.[a-z]+$'),
  media_type text NOT NULL CHECK (media_type='data'),
  content_digest text NOT NULL CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
  PRIMARY KEY (build_id,role),
  UNIQUE (build_id,artifact_id),
  UNIQUE (build_id,content_digest),
  FOREIGN KEY (build_id,workspace_id,project_id) REFERENCES development_build_evidence(build_id,workspace_id,project_id) ON DELETE CASCADE,
  FOREIGN KEY (artifact_id,workspace_id,project_id) REFERENCES artifacts(id,workspace_id,project_id)
);

-- The controller writes terminal receipts without direct table privileges. The
-- deferred consistency trigger therefore has to retain migration-owner
-- authority while resolving the matching Run and receipt.
ALTER FUNCTION validate_run_receipt_consistency() SECURITY DEFINER
  SET search_path=pg_catalog,public;

CREATE FUNCTION development_controller_enqueue() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
  INSERT INTO public.development_build_jobs(build_id,workspace_id,project_id) VALUES(NEW.id,NEW.workspace_id,NEW.project_id);
  RETURN NEW;
END $$;
CREATE TRIGGER development_build_enqueue AFTER INSERT ON development_builds FOR EACH ROW EXECUTE FUNCTION development_controller_enqueue();
INSERT INTO development_build_jobs(build_id,workspace_id,project_id)
  SELECT id,workspace_id,project_id FROM development_builds WHERE status IN ('queued','building','testing') ON CONFLICT DO NOTHING;

CREATE FUNCTION development_controller_claim(p_worker_id text,p_lease_seconds integer)
RETURNS TABLE(build_id uuid,workspace_id uuid,project_id uuid,run_id uuid,build_version bigint,
  lease_token uuid,lease_expires_at timestamptz,attempt integer,requested_by uuid,source_repository text,
  source_commit text,project_manifest_digest text,project_snapshot jsonb,plan_digest text,created_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE selected public.development_build_jobs%ROWTYPE; effective_now timestamptz:=clock_timestamp();
BEGIN
  IF p_worker_id !~ '^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$' AND p_worker_id !~ '^[a-z0-9]$' OR p_lease_seconds NOT BETWEEN 10 AND 300 THEN
    RAISE EXCEPTION 'invalid development controller claim' USING ERRCODE='22023';
  END IF;
  SELECT job.* INTO selected FROM public.development_build_jobs job JOIN public.development_builds build ON build.id=job.build_id
    WHERE job.completed_at IS NULL AND job.available_at<=effective_now AND job.attempt_count<5
      AND (job.lease_expires_at IS NULL OR job.lease_expires_at<=effective_now) AND build.status IN ('queued','building')
    ORDER BY job.available_at,job.created_at,job.build_id FOR UPDATE OF job SKIP LOCKED LIMIT 1;
  IF NOT FOUND THEN RETURN; END IF;
  UPDATE public.development_build_jobs job SET worker_id=p_worker_id,lease_token=gen_random_uuid(),
    lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds),attempt_count=job.attempt_count+1
    WHERE job.build_id=selected.build_id RETURNING job.lease_token,job.lease_expires_at,job.attempt_count
    INTO selected.lease_token,selected.lease_expires_at,selected.attempt_count;
  UPDATE public.development_builds build SET status='building',version=build.version+1,started_at=coalesce(build.started_at,effective_now)
    WHERE build.id=selected.build_id AND build.status='queued';
  RETURN QUERY SELECT build.id,build.workspace_id,build.project_id,build.run_id,build.version,selected.lease_token,
    selected.lease_expires_at,selected.attempt_count,build.requested_by,build.source_repository,build.source_commit,
    build.project_manifest_digest,build.project_snapshot,build.plan_digest,build.created_at
    FROM public.development_builds build WHERE build.id=selected.build_id;
END $$;

CREATE FUNCTION development_controller_renew(p_build_id uuid,p_worker_id text,p_lease_token uuid,p_lease_seconds integer)
RETURNS timestamptz LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE renewed timestamptz; effective_now timestamptz:=clock_timestamp();
BEGIN
  IF p_lease_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid development controller renewal' USING ERRCODE='22023'; END IF;
  UPDATE public.development_build_jobs job SET lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds)
    WHERE job.build_id=p_build_id AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token
      AND job.completed_at IS NULL AND job.lease_expires_at>effective_now
    RETURNING job.lease_expires_at INTO renewed;
  RETURN renewed;
END $$;

CREATE FUNCTION development_controller_resolve(p_build_id uuid,p_worker_id text,p_lease_token uuid)
RETURNS TABLE(build_id uuid,workspace_id uuid,project_id uuid,run_id uuid,build_version bigint,
  lease_token uuid,lease_expires_at timestamptz,attempt integer,requested_by uuid,source_repository text,
  source_commit text,project_manifest_digest text,project_snapshot jsonb,plan_digest text,created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
  SELECT build.id,build.workspace_id,build.project_id,build.run_id,
    CASE WHEN job.completed_at IS NULL THEN build.version ELSE build.version-1 END,job.lease_token,job.lease_expires_at,
    job.attempt_count,build.requested_by,build.source_repository,build.source_commit,build.project_manifest_digest,
    build.project_snapshot,build.plan_digest,build.created_at
  FROM public.development_build_jobs job JOIN public.development_builds build ON build.id=job.build_id
  WHERE job.build_id=p_build_id AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token AND (
    (job.completed_at IS NULL AND job.lease_expires_at>clock_timestamp() AND build.status IN ('building','testing')) OR
    (job.completed_at IS NOT NULL AND build.status IN ('succeeded','failed','cancelled')))
$$;

CREATE FUNCTION development_controller_finalize_v1(p_build_id uuid,p_worker_id text,p_lease_token uuid,p_expected_version bigint,
  p_node_id uuid,p_sandbox_id uuid,p_document jsonb) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target public.development_builds%ROWTYPE; job public.development_build_jobs%ROWTYPE; artifact jsonb;
  target_status text; eligible boolean; reasons text[]; reference_id uuid; effective_now timestamptz:=clock_timestamp();
BEGIN
  IF p_document IS NULL OR jsonb_typeof(p_document)<>'object' OR NOT public.development_evidence_is_redacted(p_document) OR
     public.workspace_json_contains_secret_key(p_document) THEN RAISE EXCEPTION 'invalid development finalization' USING ERRCODE='22023'; END IF;
  SELECT * INTO target FROM public.development_builds WHERE id=p_build_id FOR UPDATE;
  IF NOT FOUND THEN RETURN false; END IF;
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF target.status IN ('succeeded','failed','cancelled') THEN
    RETURN target.version=p_expected_version+1 AND target.final_document=p_document AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token;
  END IF;
  IF target.version<>p_expected_version OR target.status NOT IN ('building','testing') OR job.worker_id<>p_worker_id OR
     job.lease_token<>p_lease_token OR job.completed_at IS NOT NULL OR job.lease_expires_at<=effective_now THEN RETURN false; END IF;
  target_status:=p_document->>'status';
  IF p_document->>'schemaVersion'<>'blazn.dev/build/v1alpha1' OR target_status NOT IN ('succeeded','failed','cancelled') OR
     p_document->>'id'<>target.id::text OR p_document->>'workspaceId'<>target.workspace_id::text OR
     p_document->>'projectId'<>target.project_id::text OR p_document->>'runId'<>target.run_id::text OR
     p_document->>'version'<>(target.version+1)::text OR p_document#>>'{source,repository}'<>target.source_repository OR
     p_document#>>'{source,commit}'<>target.source_commit OR p_document->>'projectManifestDigest'<>target.project_manifest_digest OR
     p_document->>'planDigest'<>target.plan_digest OR p_document->>'receiptDigest' !~ '^sha256:[0-9a-f]{64}$' OR
     p_document#>>'{finalization,authority,contractVersion}'<>'blazn.dev/finalizer/v1alpha1' OR
     p_document#>>'{finalization,authority,kind}'<>'controller' OR
     p_document#>>'{finalization,authority,principal}'<>'blazn-development-controller' OR
     p_document#>>'{finalization,authority,authentication}'<>'mtls-workload-identity' OR
     p_document#>>'{finalization,run,id}'<>target.run_id::text OR
     p_document#>>'{finalization,run,workspaceId}'<>target.workspace_id::text OR
     p_document#>>'{finalization,run,projectId}'<>target.project_id::text THEN RETURN false; END IF;
  BEGIN reference_id:=(p_document#>>'{finalization,referenceBuild,id}')::uuid; EXCEPTION WHEN invalid_text_representation THEN RETURN false; END;
  IF reference_id=target.id OR NOT (EXISTS(SELECT 1 FROM public.development_builds reference WHERE reference.id=reference_id AND
      reference.workspace_id=target.workspace_id AND reference.project_id=target.project_id AND reference.status='succeeded' AND
      reference.final_document->>'receiptDigest'=p_document#>>'{finalization,referenceBuild,receiptDigest}') OR
    EXISTS(SELECT 1 FROM public.development_reproducibility_baselines baseline WHERE baseline.id=reference_id AND
      baseline.workspace_id=target.workspace_id AND baseline.project_id=target.project_id AND
      baseline.reference_document->>'receiptDigest'=p_document#>>'{finalization,referenceBuild,receiptDigest}')) THEN RETURN false; END IF;
  IF NOT EXISTS(SELECT 1 FROM public.nodes node WHERE node.id=p_node_id AND node.workspace_id=target.workspace_id AND
      node.lifecycle_state='active' AND node.trust_state='verified' AND node.agent_eligible) OR
     NOT EXISTS(SELECT 1 FROM public.sandboxes sandbox WHERE sandbox.id=p_sandbox_id AND sandbox.workspace_id=target.workspace_id AND
      sandbox.requested_by=target.requested_by AND sandbox.state IN ('ready','running')) THEN RETURN false; END IF;
  IF jsonb_typeof(p_document#>'{finalization,artifacts}')<>'array' OR jsonb_array_length(p_document#>'{finalization,artifacts}') NOT BETWEEN 1 AND 100 OR
     jsonb_typeof(p_document#>'{evidence,artifactIds}')<>'array' OR jsonb_typeof(p_document#>'{evidence,artifactManifest}')<>'array' THEN RETURN false; END IF;
  FOR artifact IN SELECT value FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') LOOP
    IF artifact->>'workspaceId'<>target.workspace_id::text OR artifact->>'projectId'<>target.project_id::text OR artifact->>'mediaType'<>'data' OR
       NOT EXISTS(SELECT 1 FROM public.artifacts stored WHERE stored.id=(artifact->>'id')::uuid AND stored.workspace_id=target.workspace_id AND
         stored.project_id=target.project_id AND stored.source_run_id=target.run_id AND stored.status='ready' AND stored.kind=artifact->>'kind' AND
         stored.media_type=artifact->>'mediaType' AND stored.digest=artifact->>'contentDigest') THEN RETURN false; END IF;
  END LOOP;
  IF (SELECT count(*)<>count(DISTINCT value->>'id') OR count(*)<>count(DISTINCT value->>'role') OR count(*)<>count(DISTINCT value->>'contentDigest')
      FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') value) OR
     (SELECT array_agg(value->>'id' ORDER BY value->>'id') FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') value)<>
     (SELECT array_agg(value ORDER BY value) FROM jsonb_array_elements_text(p_document#>'{evidence,artifactIds}') value) THEN RETURN false; END IF;
  eligible:=(p_document#>>'{publication,eligible}')::boolean;
  SELECT coalesce(array_agg(value ORDER BY value),'{}'::text[]) INTO reasons FROM jsonb_array_elements_text(p_document#>'{publication,refusalReasons}') value;
  IF p_document#>'{publication,published}'<>'null'::jsonb OR (eligible AND (target_status<>'succeeded' OR cardinality(reasons)<>0)) OR
     (NOT eligible AND cardinality(reasons)=0) OR (target_status<>'succeeded' AND eligible) THEN RETURN false; END IF;

  INSERT INTO public.development_build_evidence(build_id,workspace_id,project_id,receipt_digest,evidence_document)
    VALUES(target.id,target.workspace_id,target.project_id,p_document->>'receiptDigest',p_document->'evidence');
  INSERT INTO public.development_build_evidence_artifacts(build_id,workspace_id,project_id,role,artifact_id,kind,media_type,content_digest)
    SELECT target.id,target.workspace_id,target.project_id,value->>'role',(value->>'id')::uuid,value->>'kind',value->>'mediaType',value->>'contentDigest'
    FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') value;
  INSERT INTO public.run_receipts(run_id,workspace_id,project_id,proof_class,outcome,plan_digest,receipt)
    VALUES(target.run_id,target.workspace_id,target.project_id,'sandbox',target_status,target.plan_digest,
      jsonb_build_object('schemaVersion','blazn.dev/development-run-receipt/v1','buildId',target.id,'buildReceiptDigest',p_document->>'receiptDigest',
        'evidenceArtifactIds',p_document#>'{evidence,artifactIds}'));
  UPDATE public.runs run SET status=target_status,version=run.version+1,node_id=p_node_id,sandbox_id=p_sandbox_id,
    output_names=ARRAY(SELECT translate(value->>'role','/','-') FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') value ORDER BY value->>'role'),
    started_at=coalesce(run.started_at,target.started_at,effective_now),completed_at=effective_now,
    error_code=CASE WHEN target_status='failed' THEN p_document->>'errorCode' ELSE NULL END
    WHERE run.id=target.run_id AND run.workspace_id=target.workspace_id AND run.project_id=target.project_id AND run.status='queued';
  IF NOT FOUND THEN RAISE EXCEPTION 'canonical Development Run changed' USING ERRCODE='40001'; END IF;
  UPDATE public.development_builds build SET status=target_status,version=build.version+1,publication_eligible=eligible,
    refusal_reasons=reasons,final_document=p_document,completed_at=effective_now,
    error_code=CASE WHEN target_status='failed' THEN p_document->>'errorCode' ELSE NULL END
    WHERE build.id=target.id AND build.version=p_expected_version;
  IF NOT FOUND THEN RAISE EXCEPTION 'Development Build changed' USING ERRCODE='40001'; END IF;
  UPDATE public.development_build_jobs SET completed_at=effective_now WHERE build_id=target.id;
  RETURN true;
END $$;

DROP FUNCTION development_controller_finalize(uuid,bigint,jsonb);

REVOKE ALL ON TABLE development_build_jobs,development_build_evidence,development_build_evidence_artifacts
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
REVOKE ALL ON FUNCTION development_evidence_is_redacted(jsonb),development_controller_enqueue(),
  development_controller_claim(text,integer),development_controller_renew(uuid,text,uuid,integer),
  development_controller_resolve(uuid,text,uuid),development_controller_finalize_v1(uuid,text,uuid,bigint,uuid,uuid,jsonb)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
GRANT EXECUTE ON FUNCTION development_controller_claim(text,integer),development_controller_renew(uuid,text,uuid,integer),
  development_controller_resolve(uuid,text,uuid),development_controller_finalize_v1(uuid,text,uuid,bigint,uuid,uuid,jsonb)
  TO blazn_development_controller;
