CREATE TABLE development_policy_profiles (
  kind text NOT NULL CHECK (kind IN ('builder','network','resource','publication')),
  name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  active boolean NOT NULL DEFAULT true,
  PRIMARY KEY (kind,name)
);
INSERT INTO development_policy_profiles(kind,name) VALUES
  ('builder','trusted-buildkit-v1'),('network','build-egress-v1'),
  ('resource','poc-build-small-v1'),('publication','poc-development-v1');

CREATE TABLE development_registry_repositories (
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  repository text NOT NULL CHECK (repository ~ '^[a-z0-9].*/[a-z0-9._/-]+$' AND repository !~ '[@:]'),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
  authorized_by uuid NOT NULL REFERENCES users(id),
  audit_event_id uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id,repository)
);

ALTER TABLE workspace_audit_events ADD CONSTRAINT workspace_audit_events_id_workspace_unique UNIQUE(id,workspace_id);
ALTER TABLE development_registry_repositories ADD CONSTRAINT development_registry_audit_fk
  FOREIGN KEY (audit_event_id,workspace_id) REFERENCES workspace_audit_events(id,workspace_id);

CREATE TABLE development_projects (
  project_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  manifest jsonb NOT NULL,
  manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
  template_version_id uuid NOT NULL,
  template_version text NOT NULL,
  template_digest char(64) NOT NULL CHECK (template_digest ~ '^[0-9a-f]{64}$'),
  publication_template_id uuid NOT NULL,
  registry_repository text NOT NULL,
  policy_snapshot jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (project_id, workspace_id) REFERENCES projects(id, workspace_id),
  FOREIGN KEY (template_version_id, workspace_id, publication_template_id, template_version, template_digest)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id, version, content_digest),
  FOREIGN KEY (publication_template_id, workspace_id) REFERENCES sandbox_templates(id, workspace_id),
  FOREIGN KEY (workspace_id,registry_repository) REFERENCES development_registry_repositories(workspace_id,repository),
  UNIQUE (project_id, workspace_id),
  CHECK (manifest->>'schemaVersion' = 'blazn.dev/project/v1alpha1'),
  CHECK (manifest->>'projectId' = project_id::text),
  CHECK (manifest#>>'{template,versionId}' = template_version_id::text),
  CHECK (manifest#>>'{template,digest}' = 'sha256:'||trim(template_digest)),
  CHECK (manifest#>>'{publicationTarget,templateId}' = publication_template_id::text),
  CHECK (manifest#>>'{build,registryRepository}' = registry_repository),
  CHECK (manifest->'policy'=policy_snapshot),
  CHECK (NOT workspace_json_contains_secret_key(manifest))
);

CREATE TABLE development_builds (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  run_id uuid NOT NULL,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','building','testing','succeeded','failed','cancelled')),
  requested_by uuid NOT NULL REFERENCES users(id),
  source_repository text NOT NULL CHECK (source_repository ~ '^https://'),
  source_commit text NOT NULL CHECK (source_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
  project_manifest_digest text NOT NULL CHECK (project_manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
  project_version bigint NOT NULL CHECK (project_version > 0),
  project_snapshot jsonb NOT NULL,
  template_version_id uuid NOT NULL,
  template_version text NOT NULL,
  template_digest char(64) NOT NULL,
  publication_template_id uuid NOT NULL,
  publication_candidate_version_id uuid NOT NULL,
  publication_draft_version bigint NOT NULL CHECK (publication_draft_version > 0),
  publication_candidate_digest char(64) NOT NULL CHECK (publication_candidate_digest ~ '^[0-9a-f]{64}$'),
  registry_repository text NOT NULL,
  policy_snapshot jsonb NOT NULL,
  plan_digest text NOT NULL CHECK (plan_digest ~ '^sha256:[0-9a-f]{64}$'),
  publication_eligible boolean NOT NULL DEFAULT false,
  refusal_reasons text[] NOT NULL DEFAULT ARRAY['build_not_succeeded']::text[],
  final_document jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  completed_at timestamptz,
  error_code text CHECK (error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,62}$'),
  FOREIGN KEY (project_id, workspace_id) REFERENCES development_projects(project_id, workspace_id),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id),
  UNIQUE (id, workspace_id, project_id),
  UNIQUE (run_id),
  CHECK (cardinality(refusal_reasons) BETWEEN 0 AND 32),
  CHECK (NOT publication_eligible OR cardinality(refusal_reasons) = 0),
  CHECK (publication_eligible OR cardinality(refusal_reasons) > 0),
  CHECK ((status IN ('queued','building','testing')) = (completed_at IS NULL)),
  CHECK ((status IN ('succeeded','failed','cancelled')) = (final_document IS NOT NULL)),
  CHECK (final_document IS NULL OR NOT workspace_json_contains_secret_key(final_document))
);

CREATE TABLE development_reproducibility_baselines (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  reference_document jsonb NOT NULL,
  audit_event_id uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (project_id,workspace_id) REFERENCES development_projects(project_id,workspace_id),
  FOREIGN KEY (audit_event_id,workspace_id) REFERENCES workspace_audit_events(id,workspace_id),
  UNIQUE(id,workspace_id,project_id),
  CHECK (reference_document->>'id'=id::text),
  CHECK (reference_document->>'workspaceId'=workspace_id::text),
  CHECK (reference_document->>'projectId'=project_id::text),
  CHECK (reference_document->>'receiptDigest' ~ '^sha256:[0-9a-f]{64}$'),
  CHECK (NOT workspace_json_contains_secret_key(reference_document))
);

CREATE INDEX development_builds_project_status_id_idx
  ON development_builds(workspace_id, project_id, status, id);

CREATE FUNCTION development_runtime_authorized(p_workspace_id uuid,p_project_id uuid,p_operate boolean)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
  SELECT EXISTS(SELECT 1 FROM public.workspaces w JOIN public.workspace_memberships m ON m.workspace_id=w.id
    JOIN public.projects p ON p.workspace_id=w.id AND p.id=p_project_id
    WHERE w.id=p_workspace_id AND w.status='active' AND p.status='active' AND m.status='active'
      AND m.user_id=current_setting('blazn.development_user_id',true)::uuid
      AND (NOT p_operate OR m.role IN ('owner','administrator','operator')))
$$;

CREATE FUNCTION development_runtime_get_project(p_workspace_id uuid,p_project_id uuid)
RETURNS SETOF development_projects LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public
AS $$ SELECT * FROM public.development_projects WHERE workspace_id=p_workspace_id AND project_id=p_project_id
  AND public.development_runtime_authorized(p_workspace_id,p_project_id,false) $$;

CREATE FUNCTION development_runtime_put_project(p_workspace_id uuid,p_project_id uuid,p_expected_version bigint,
  p_manifest jsonb,p_manifest_digest text,p_template_version_id uuid,p_template_digest text,
  p_publication_template_id uuid,p_created_by uuid)
RETURNS SETOF development_projects LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE raw_digest text:=substring(p_template_digest from 8); registry text:=p_manifest#>>'{build,registryRepository}';
BEGIN
  IF NOT public.development_runtime_authorized(p_workspace_id,p_project_id,true) OR
     NOT EXISTS(SELECT 1 FROM public.projects WHERE id=p_project_id AND workspace_id=p_workspace_id AND status='active') OR
     NOT EXISTS(SELECT 1 FROM public.development_registry_repositories WHERE workspace_id=p_workspace_id AND repository=registry AND status='active') OR
     NOT EXISTS(SELECT 1 FROM public.development_policy_profiles WHERE kind='builder' AND name=p_manifest#>>'{policy,builderProfile}' AND active) OR
     NOT EXISTS(SELECT 1 FROM public.development_policy_profiles WHERE kind='network' AND name=p_manifest#>>'{policy,networkProfile}' AND active) OR
     NOT EXISTS(SELECT 1 FROM public.development_policy_profiles WHERE kind='resource' AND name=p_manifest#>>'{policy,resourceProfile}' AND active) OR
     NOT EXISTS(SELECT 1 FROM public.development_policy_profiles WHERE kind='publication' AND name=p_manifest#>>'{policy,publicationPolicy}' AND active) THEN RETURN; END IF;
  IF p_expected_version=0 THEN
    RETURN QUERY INSERT INTO public.development_projects(project_id,workspace_id,manifest,manifest_digest,template_version_id,template_version,template_digest,publication_template_id,registry_repository,policy_snapshot,created_by)
      SELECT p_project_id,p_workspace_id,p_manifest,p_manifest_digest,v.id,v.version,v.content_digest,p_publication_template_id,registry,p_manifest->'policy',p_created_by
      FROM public.sandbox_template_versions v JOIN public.sandbox_template_version_status s ON s.version_id=v.id
      WHERE v.id=p_template_version_id AND v.workspace_id=p_workspace_id AND v.template_id=p_publication_template_id AND v.content_digest=raw_digest AND s.status='published'
      ON CONFLICT(project_id) DO NOTHING RETURNING *;
  ELSE
    RETURN QUERY UPDATE public.development_projects project SET manifest=p_manifest,manifest_digest=p_manifest_digest,
      template_version_id=v.id,template_version=v.version,template_digest=v.content_digest,
      publication_template_id=p_publication_template_id,registry_repository=registry,policy_snapshot=p_manifest->'policy',
      version=project.version+1,updated_at=clock_timestamp()
      FROM public.sandbox_template_versions v JOIN public.sandbox_template_version_status s ON s.version_id=v.id
      WHERE project.workspace_id=p_workspace_id AND project.project_id=p_project_id AND project.version=p_expected_version
        AND v.id=p_template_version_id AND v.workspace_id=p_workspace_id AND v.template_id=p_publication_template_id AND v.content_digest=raw_digest AND s.status='published'
      RETURNING project.*;
  END IF;
END $$;

CREATE FUNCTION development_runtime_create_build(p_id uuid,p_workspace_id uuid,p_project_id uuid,p_run_id uuid,
  p_requested_by uuid,p_commit text,p_plan_digest text)
RETURNS SETOF development_builds LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE project public.development_projects%ROWTYPE; target public.sandbox_templates%ROWTYPE;
BEGIN
  IF NOT public.development_runtime_authorized(p_workspace_id,p_project_id,true) THEN RETURN; END IF;
  SELECT d.* INTO project FROM public.projects p JOIN public.development_projects d ON d.project_id=p.id AND d.workspace_id=p.workspace_id
    WHERE p.id=p_project_id AND p.workspace_id=p_workspace_id AND p.status='active' FOR UPDATE OF p,d;
  IF NOT FOUND OR NOT EXISTS(SELECT 1 FROM public.development_registry_repositories r WHERE r.workspace_id=project.workspace_id AND r.repository=project.registry_repository AND r.status='active') THEN RETURN; END IF;
  SELECT * INTO target FROM public.sandbox_templates WHERE id=project.publication_template_id AND workspace_id=project.workspace_id FOR UPDATE;
  IF NOT FOUND THEN RETURN; END IF;
  INSERT INTO public.runs(id,workspace_id,project_id,kind,proof_class,plan_digest,requested_by)
    VALUES(p_run_id,p_workspace_id,p_project_id,'development.build','sandbox',p_plan_digest,p_requested_by);
  RETURN QUERY INSERT INTO public.development_builds(id,workspace_id,project_id,run_id,requested_by,source_repository,source_commit,
      project_manifest_digest,project_version,project_snapshot,template_version_id,template_version,template_digest,
      publication_template_id,publication_candidate_version_id,publication_draft_version,publication_candidate_digest,registry_repository,policy_snapshot,plan_digest)
    VALUES(p_id,p_workspace_id,p_project_id,p_run_id,p_requested_by,project.manifest#>>'{repository,url}',p_commit,
      project.manifest_digest,project.version,project.manifest,project.template_version_id,project.template_version,project.template_digest,
      project.publication_template_id,gen_random_uuid(),target.draft_revision,target.draft_digest,project.registry_repository,project.policy_snapshot,p_plan_digest)
    RETURNING *;
END $$;

CREATE FUNCTION development_runtime_get_build(p_workspace_id uuid,p_project_id uuid,p_id uuid)
RETURNS SETOF development_builds LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public
AS $$ SELECT * FROM public.development_builds WHERE workspace_id=p_workspace_id AND project_id=p_project_id AND id=p_id
  AND public.development_runtime_authorized(p_workspace_id,p_project_id,false) $$;
CREATE FUNCTION development_runtime_list_builds(p_workspace_id uuid,p_project_id uuid,p_status text,p_cursor uuid)
RETURNS SETOF development_builds LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public
AS $$ SELECT * FROM public.development_builds WHERE workspace_id=p_workspace_id AND project_id=p_project_id
  AND public.development_runtime_authorized(p_workspace_id,p_project_id,false)
  AND (p_status='all' OR status=p_status) AND (p_cursor IS NULL OR id>p_cursor) ORDER BY id LIMIT 101 $$;

-- This is the only terminal-write boundary. It is intentionally not granted to
-- the API role; a later controller executable must authenticate separately and
-- receive EXECUTE on this one function only.
CREATE FUNCTION development_controller_finalize(
  p_build_id uuid, p_expected_version bigint, p_document jsonb)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
  target development_builds%ROWTYPE;
  artifact jsonb;
  reference_id uuid;
  target_status text;
  eligible boolean;
  reasons text[];
BEGIN
  IF p_build_id IS NULL OR p_expected_version IS NULL OR p_expected_version < 1 OR
     p_document IS NULL OR jsonb_typeof(p_document) <> 'object' OR
     public.workspace_json_contains_secret_key(p_document) THEN
    RAISE EXCEPTION 'invalid development finalization' USING ERRCODE='22023';
  END IF;
  SELECT * INTO target FROM public.development_builds
    WHERE id=p_build_id FOR UPDATE;
  IF NOT FOUND OR target.version<>p_expected_version OR target.status IN ('succeeded','failed','cancelled') THEN
    RETURN false;
  END IF;
  target_status := p_document->>'status';
  IF p_document->>'schemaVersion'<>'blazn.dev/build/v1alpha1' OR target_status NOT IN ('succeeded','failed','cancelled') OR
     p_document->>'id'<>target.id::text OR
     p_document->>'workspaceId'<>target.workspace_id::text OR
     p_document->>'projectId'<>target.project_id::text OR
     p_document->>'runId'<>target.run_id::text OR
     (p_document->>'version')::bigint<>target.version+1 OR
     p_document#>>'{source,repository}'<>target.source_repository OR
     p_document#>>'{source,commit}'<>target.source_commit OR
     p_document->>'projectManifestDigest'<>target.project_manifest_digest OR
     p_document->>'planDigest'<>target.plan_digest OR
     p_document#>'{template}'<>target.project_snapshot#>'{template}' OR
     p_document#>'{dependencyLocks}'<>target.project_snapshot#>'{dependencyLocks}' OR
     p_document#>'{platforms}'<>target.project_snapshot#>'{platforms}' OR
     p_document#>>'{builder,profile}'<>target.policy_snapshot->>'builderProfile' OR
     p_document->>'receiptDigest' !~ '^sha256:[0-9a-f]{64}$' OR
     p_document#>>'{finalization,authority,contractVersion}'<>'blazn.dev/finalizer/v1alpha1' OR
     p_document#>>'{finalization,authority,kind}'<>'controller' OR
     p_document#>>'{finalization,authority,principal}'<>'blazn-development-controller' OR
     p_document#>>'{finalization,authority,authentication}'<>'mtls-workload-identity' OR
     p_document#>>'{finalization,run,id}'<>target.run_id::text OR
     p_document#>>'{finalization,run,workspaceId}'<>target.workspace_id::text OR
     p_document#>>'{finalization,run,projectId}'<>target.project_id::text OR
     NOT EXISTS (SELECT 1 FROM public.runs r WHERE r.id=target.run_id AND r.workspace_id=target.workspace_id AND r.project_id=target.project_id) THEN
    RETURN false;
  END IF;
  IF p_document#>>'{publicationTarget,templateId}'<>target.publication_template_id::text OR
     p_document#>>'{publicationTarget,candidateVersionId}'<>target.publication_candidate_version_id::text OR
     (p_document#>>'{publicationTarget,expectedDraftVersion}')::bigint<>target.publication_draft_version OR
     p_document#>>'{publicationTarget,candidateDigest}'<>'sha256:'||trim(target.publication_candidate_digest) THEN RETURN false; END IF;

  reference_id := (p_document#>>'{finalization,referenceBuild,id}')::uuid;
  IF reference_id=target.id OR NOT (
    EXISTS (SELECT 1 FROM public.development_builds reference WHERE reference.id=reference_id AND reference.workspace_id=target.workspace_id
      AND reference.project_id=target.project_id AND reference.status='succeeded' AND reference.final_document->>'receiptDigest'=p_document#>>'{finalization,referenceBuild,receiptDigest}') OR
    EXISTS (SELECT 1 FROM public.development_reproducibility_baselines baseline WHERE baseline.id=reference_id AND baseline.workspace_id=target.workspace_id
      AND baseline.project_id=target.project_id AND baseline.reference_document->>'receiptDigest'=p_document#>>'{finalization,referenceBuild,receiptDigest}')
  ) THEN RETURN false; END IF;

  IF jsonb_typeof(p_document#>'{finalization,artifacts}')<>'array' OR
     jsonb_array_length(p_document#>'{finalization,artifacts}')>100 THEN RETURN false; END IF;
  FOR artifact IN SELECT value FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') LOOP
    IF artifact->>'workspaceId'<>target.workspace_id::text OR artifact->>'projectId'<>target.project_id::text OR
       NOT EXISTS (SELECT 1 FROM public.artifacts a
         WHERE a.id=(artifact->>'id')::uuid AND a.workspace_id=target.workspace_id AND a.project_id=target.project_id
           AND a.status='ready' AND a.kind=artifact->>'kind' AND a.media_type=artifact->>'mediaType'
           AND a.digest=artifact->>'contentDigest') THEN RETURN false; END IF;
  END LOOP;
  IF (SELECT count(*)<>count(DISTINCT value->>'id') FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') value) OR
     (SELECT coalesce(array_agg(value->>'id' ORDER BY value->>'id'),'{}'::text[]) FROM jsonb_array_elements(p_document#>'{finalization,artifacts}') value)<>
     (SELECT coalesce(array_agg(value ORDER BY value),'{}'::text[]) FROM jsonb_array_elements_text(coalesce(p_document#>'{evidence,artifactIds}','[]'::jsonb)) value) THEN RETURN false; END IF;

  eligible := (p_document#>>'{publication,eligible}')::boolean;
  SELECT coalesce(array_agg(value), '{}'::text[]) INTO reasons
    FROM jsonb_array_elements_text(coalesce(p_document#>'{publication,refusalReasons}','[]'::jsonb)) value;
  IF p_document#>'{publication,published}' <> 'null'::jsonb OR
     (eligible AND (target_status<>'succeeded' OR cardinality(reasons)<>0)) OR
     (NOT eligible AND cardinality(reasons)=0) OR
     (target_status='succeeded' AND (p_document#>>'{outputs,imageIndexDigest}' !~ ('^'||replace(target.registry_repository,'.','\\.')||'@sha256:[0-9a-f]{64}$') OR
       jsonb_array_length(coalesce(p_document#>'{outputs,images}','[]'::jsonb))<>2 OR
       p_document#>>'{evidence,projectTests,sourceCommit}'<>target.source_commit OR
       (p_document#>>'{evidence,secretScan,passed}')::boolean IS NOT TRUE OR (p_document#>>'{evidence,secretScan,findings}')::integer<>0 OR
       (p_document#>>'{evidence,cleanup,passed}')::boolean IS NOT TRUE OR
       EXISTS(SELECT 1 FROM jsonb_each(coalesce(p_document#>'{evidence,projectTests,results}','{}'::jsonb)) entry WHERE (entry.value->>'passed')::boolean IS NOT TRUE) OR
       EXISTS(SELECT 1 FROM jsonb_array_elements(coalesce(p_document#>'{evidence,securityTests}','[]'::jsonb)) value WHERE (value->>'passed')::boolean IS NOT TRUE) OR
       EXISTS(SELECT 1 FROM jsonb_array_elements(coalesce(p_document#>'{evidence,lifecycleTests}','[]'::jsonb)) value WHERE (value->>'passed')::boolean IS NOT TRUE) OR
       p_document#>>'{evidence,reproducibility,comparison,candidateBuildId}'<>target.id::text OR
       p_document#>>'{evidence,reproducibility,comparison,referenceBuildId}'<>reference_id::text OR
       p_document#>>'{evidence,reproducibility,comparison,referenceInputDigest}'<>p_document#>>'{evidence,reproducibility,comparison,candidateInputDigest}')) OR
     (target_status IN ('failed','cancelled') AND eligible) THEN RETURN false; END IF;

  UPDATE public.development_builds SET status=target_status,version=version+1,
    publication_eligible=eligible,refusal_reasons=reasons,final_document=p_document,
    started_at=coalesce(started_at,clock_timestamp()),completed_at=clock_timestamp(),
    error_code=CASE WHEN target_status='failed' THEN p_document->>'errorCode' ELSE NULL END
    WHERE id=target.id AND version=p_expected_version;
  RETURN FOUND;
END
$$;

REVOKE ALL ON TABLE development_policy_profiles,development_registry_repositories,development_projects,
  development_builds,development_reproducibility_baselines
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller;
REVOKE ALL ON FUNCTION development_runtime_authorized(uuid,uuid,boolean),development_runtime_get_project(uuid,uuid),
  development_runtime_put_project(uuid,uuid,bigint,jsonb,text,uuid,text,uuid,uuid),
  development_runtime_create_build(uuid,uuid,uuid,uuid,uuid,text,text),
  development_runtime_get_build(uuid,uuid,uuid),development_runtime_list_builds(uuid,uuid,text,uuid)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION development_runtime_get_project(uuid,uuid),
  development_runtime_put_project(uuid,uuid,bigint,jsonb,text,uuid,text,uuid,uuid),
  development_runtime_create_build(uuid,uuid,uuid,uuid,uuid,text,text),
  development_runtime_get_build(uuid,uuid,uuid),development_runtime_list_builds(uuid,uuid,text,uuid)
  TO blazn_runtime;
REVOKE ALL ON FUNCTION development_controller_finalize(uuid,bigint,jsonb)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
