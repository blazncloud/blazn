-- The execution generation increments on every claim, including lease-expiry
-- recovery where no worker was able to call release. Unlike the bounded
-- attempt counter it never wraps and is safe to delegate without the lease
-- token to a pinned child process.
ALTER TABLE development_build_jobs ADD COLUMN failure_count bigint NOT NULL DEFAULT 0 CHECK (failure_count>=0);
ALTER TABLE development_build_jobs ADD COLUMN execution_generation bigint NOT NULL DEFAULT 0 CHECK (execution_generation>=0);
ALTER TABLE development_build_jobs ADD COLUMN last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,62}$');
ALTER TABLE development_build_jobs ADD COLUMN last_error_at timestamptz;

-- Controller-owned Artifact inserts fire cross-feature deferred validators.
-- Keep those validators at migration-owner authority; the controller role
-- receives no direct table reads for their referenced relations.
ALTER FUNCTION validate_synthetic_artifact_consistency() SECURITY DEFINER SET search_path=pg_catalog,public;
ALTER FUNCTION validate_project_profile_artifact() SECURITY DEFINER SET search_path=pg_catalog,public;

DROP FUNCTION development_controller_claim(text,integer);
CREATE FUNCTION development_controller_claim(p_worker_id text,p_lease_seconds integer)
RETURNS TABLE(build_id uuid,workspace_id uuid,project_id uuid,run_id uuid,build_version bigint,
  lease_token uuid,lease_expires_at timestamptz,attempt integer,generation bigint,requested_by uuid,source_repository text,
  source_commit text,project_manifest_digest text,project_snapshot jsonb,plan_digest text,created_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE selected public.development_build_jobs%ROWTYPE; effective_now timestamptz:=clock_timestamp();
BEGIN
  IF p_worker_id !~ '^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$' AND p_worker_id !~ '^[a-z0-9]$' OR p_lease_seconds NOT BETWEEN 10 AND 300 THEN
    RAISE EXCEPTION 'invalid development controller claim' USING ERRCODE='22023';
  END IF;
  SELECT job.* INTO selected FROM public.development_build_jobs job JOIN public.development_builds build ON build.id=job.build_id
    WHERE job.completed_at IS NULL AND job.available_at<=effective_now
      AND (job.lease_expires_at IS NULL OR job.lease_expires_at<=effective_now) AND build.status IN ('queued','building')
    ORDER BY job.available_at,job.created_at,job.build_id FOR UPDATE OF job SKIP LOCKED LIMIT 1;
  IF NOT FOUND THEN RETURN; END IF;
  UPDATE public.development_build_jobs job SET worker_id=p_worker_id,lease_token=gen_random_uuid(),
    lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds),
    attempt_count=CASE WHEN job.attempt_count>=5 THEN 1 ELSE job.attempt_count+1 END,
    execution_generation=job.execution_generation+1
    WHERE job.build_id=selected.build_id RETURNING job.lease_token,job.lease_expires_at,job.attempt_count,job.execution_generation
    INTO selected.lease_token,selected.lease_expires_at,selected.attempt_count,selected.execution_generation;
  UPDATE public.development_builds build SET status='building',version=build.version+1,started_at=coalesce(build.started_at,effective_now)
    WHERE build.id=selected.build_id AND build.status='queued';
  RETURN QUERY SELECT build.id,build.workspace_id,build.project_id,build.run_id,build.version,selected.lease_token,
    selected.lease_expires_at,selected.attempt_count,selected.execution_generation,build.requested_by,build.source_repository,build.source_commit,
    build.project_manifest_digest,build.project_snapshot,build.plan_digest,build.created_at
    FROM public.development_builds build WHERE build.id=selected.build_id;
END $$;

DROP FUNCTION development_controller_resolve(uuid,text,uuid);
CREATE FUNCTION development_controller_resolve(p_build_id uuid,p_worker_id text,p_lease_token uuid)
RETURNS TABLE(build_id uuid,workspace_id uuid,project_id uuid,run_id uuid,build_version bigint,
  lease_token uuid,lease_expires_at timestamptz,attempt integer,generation bigint,requested_by uuid,source_repository text,
  source_commit text,project_manifest_digest text,project_snapshot jsonb,plan_digest text,created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
  SELECT build.id,build.workspace_id,build.project_id,build.run_id,
    CASE WHEN job.completed_at IS NULL THEN build.version ELSE build.version-1 END,job.lease_token,job.lease_expires_at,
    job.attempt_count,job.execution_generation,build.requested_by,build.source_repository,build.source_commit,
    build.project_manifest_digest,build.project_snapshot,build.plan_digest,build.created_at
  FROM public.development_build_jobs job JOIN public.development_builds build ON build.id=job.build_id
  WHERE job.build_id=p_build_id AND job.worker_id=p_worker_id AND job.lease_token=p_lease_token AND (
    (job.completed_at IS NULL AND job.lease_expires_at>clock_timestamp() AND build.status IN ('building','testing')) OR
    (job.completed_at IS NOT NULL AND build.status IN ('succeeded','failed','cancelled')))
$$;

CREATE TABLE development_artifact_blobs (
  artifact_id uuid PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
  content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 16777216),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION development_controller_store_artifact_v1(
  p_build_id uuid,p_worker_id text,p_lease_token uuid,p_artifact_id uuid,
  p_role text,p_kind text,p_content_digest text,p_content bytea
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE target public.development_builds%ROWTYPE; job public.development_build_jobs%ROWTYPE;
  stored public.artifacts%ROWTYPE; stored_content bytea; content_text text; content_document jsonb; effective_now timestamptz:=clock_timestamp();
BEGIN
  BEGIN content_text:=convert_from(p_content,'UTF8');content_document:=content_text::jsonb;
  EXCEPTION WHEN others THEN RAISE EXCEPTION 'invalid Development Artifact JSON' USING ERRCODE='22023'; END;
  IF p_role !~ '^[a-z][a-z0-9-]*(/[A-Za-z0-9._-]+)*$' OR
     p_kind !~ '^development\.[a-z]+$' OR
     p_content_digest !~ '^sha256:[0-9a-f]{64}$' OR
     octet_length(p_content) NOT BETWEEN 1 AND 16777216 OR
     p_content_digest <> 'sha256:' || encode(digest(p_content,'sha256'),'hex') OR jsonb_typeof(content_document)<>'object' OR
     NOT public.development_evidence_is_redacted(content_document) OR public.workspace_json_contains_secret_key(content_document) OR
     content_text ~ '(sk-(proj-|svcacct-|ant-)?[A-Za-z0-9_-]{16,}|github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|(AKIA|ASIA)[A-Z0-9]{16}|-----BEGIN ([A-Z0-9 ]+ )?PRIVATE KEY-----|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})' THEN
    RAISE EXCEPTION 'invalid Development Artifact' USING ERRCODE='22023';
  END IF;
  SELECT * INTO target FROM public.development_builds WHERE id=p_build_id;
  SELECT * INTO job FROM public.development_build_jobs WHERE build_id=p_build_id FOR UPDATE;
  IF target.id IS NULL OR job.worker_id<>p_worker_id OR job.lease_token<>p_lease_token THEN RETURN false; END IF;

  SELECT * INTO stored FROM public.artifacts WHERE id=p_artifact_id OR
    (source_run_id=target.run_id AND name=p_role AND status<>'deleted') FOR UPDATE;
  -- A caller that lost the successful commit response may replay only the exact
  -- immutable bytes. Completed jobs can neither insert nor alter an Artifact.
  IF job.completed_at IS NOT NULL THEN
    IF target.status NOT IN ('succeeded','failed','cancelled') OR NOT FOUND THEN RETURN false; END IF;
    SELECT content INTO stored_content FROM public.development_artifact_blobs WHERE artifact_id=stored.id;
    RETURN stored.id=p_artifact_id AND stored.workspace_id=target.workspace_id AND
      stored.project_id=target.project_id AND stored.source_run_id=target.run_id AND
      stored.kind=p_kind AND stored.media_type='data' AND stored.name=p_role AND
      stored.status='ready' AND stored.digest=p_content_digest AND
      stored.size_bytes=octet_length(p_content) AND stored.object_key='development-db/'||p_artifact_id::text AND
      stored.created_by=target.requested_by AND stored_content=p_content;
  END IF;
  IF job.lease_expires_at<=effective_now OR target.status NOT IN ('building','testing') THEN RETURN false; END IF;
  IF FOUND THEN
    SELECT content INTO stored_content FROM public.development_artifact_blobs WHERE artifact_id=stored.id;
    RETURN stored.id=p_artifact_id AND stored.workspace_id=target.workspace_id AND
      stored.project_id=target.project_id AND stored.source_run_id=target.run_id AND
      stored.kind=p_kind AND stored.media_type='data' AND stored.name=p_role AND
      stored.status='ready' AND stored.digest=p_content_digest AND
      stored.size_bytes=octet_length(p_content) AND stored.object_key='development-db/'||p_artifact_id::text AND
      stored.created_by=target.requested_by AND stored_content=p_content;
  END IF;

  INSERT INTO public.artifacts(id,workspace_id,project_id,source_run_id,kind,media_type,name,status,digest,size_bytes,object_key,created_by)
    VALUES(p_artifact_id,target.workspace_id,target.project_id,target.run_id,p_kind,'data',p_role,'ready',
      p_content_digest,octet_length(p_content),'development-db/'||p_artifact_id::text,target.requested_by);
  INSERT INTO public.development_artifact_blobs(artifact_id,content) VALUES(p_artifact_id,p_content);
  RETURN true;
END $$;

CREATE FUNCTION development_controller_release_v1(
  p_build_id uuid,p_worker_id text,p_lease_token uuid,p_delay_seconds integer,p_error_code text
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
  IF p_delay_seconds NOT BETWEEN 0 AND 300 OR p_error_code !~ '^[a-z][a-z0-9_]{0,62}$' THEN
    RAISE EXCEPTION 'invalid Development retry delay' USING ERRCODE='22023';
  END IF;
  UPDATE public.development_build_jobs SET worker_id=NULL,lease_token=NULL,lease_expires_at=NULL,
    available_at=clock_timestamp()+make_interval(secs=>p_delay_seconds),attempt_count=CASE WHEN attempt_count>=5 THEN 0 ELSE attempt_count END,
    failure_count=failure_count+1,last_error_code=p_error_code,last_error_at=clock_timestamp()
    WHERE build_id=p_build_id AND worker_id=p_worker_id AND lease_token=p_lease_token AND
      completed_at IS NULL AND lease_expires_at>clock_timestamp();
  RETURN FOUND;
END $$;

CREATE FUNCTION development_controller_commit_execution_v1(
  p_build_id uuid,p_worker_id text,p_lease_token uuid,p_expected_version bigint,
  p_node_id uuid,p_sandbox_id uuid,p_document jsonb,p_artifacts jsonb
) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE artifact jsonb; stored boolean; completed boolean; content bytea; artifact_count bigint; evidence_count bigint;
BEGIN
  IF jsonb_typeof(p_artifacts)<>'array' OR jsonb_array_length(p_artifacts) NOT BETWEEN 1 AND 100 THEN
    RAISE EXCEPTION 'invalid Development Artifact set' USING ERRCODE='22023';
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
  -- The replay payload must describe the exact Artifact set bound by the
  -- terminal evidence, not merely a subset (or duplicated aliases) of it.
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

REVOKE ALL ON TABLE development_artifact_blobs FROM PUBLIC,blazn_runtime,blazn_bootstrap,
  blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
REVOKE ALL ON FUNCTION development_controller_store_artifact_v1(uuid,text,uuid,uuid,text,text,text,bytea),
  development_controller_release_v1(uuid,text,uuid,integer,text),
  development_controller_commit_execution_v1(uuid,text,uuid,bigint,uuid,uuid,jsonb,jsonb),development_controller_claim(text,integer),
  development_controller_resolve(uuid,text,uuid)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller;
GRANT EXECUTE ON FUNCTION development_controller_store_artifact_v1(uuid,text,uuid,uuid,text,text,text,bytea),
  development_controller_release_v1(uuid,text,uuid,integer,text),
  development_controller_commit_execution_v1(uuid,text,uuid,bigint,uuid,uuid,jsonb,jsonb),development_controller_claim(text,integer),
  development_controller_resolve(uuid,text,uuid)
  TO blazn_development_controller;
