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
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (project_id, workspace_id) REFERENCES projects(id, workspace_id),
  FOREIGN KEY (template_version_id, workspace_id, publication_template_id, template_version, template_digest)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id, version, content_digest),
  FOREIGN KEY (publication_template_id, workspace_id) REFERENCES sandbox_templates(id, workspace_id),
  UNIQUE (project_id, workspace_id),
  CHECK (manifest->>'schemaVersion' = 'blazn.dev/project/v1alpha1'),
  CHECK (manifest->>'projectId' = project_id::text),
  CHECK (manifest#>>'{template,versionId}' = template_version_id::text),
  CHECK (manifest#>>'{template,digest}' = 'sha256:'||trim(template_digest)),
  CHECK (manifest#>>'{publicationTarget,templateId}' = publication_template_id::text),
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

CREATE INDEX development_builds_project_status_id_idx
  ON development_builds(workspace_id, project_id, status, id);

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
  IF target_status NOT IN ('succeeded','failed','cancelled') OR
     p_document->>'id'<>target.id::text OR
     p_document->>'workspaceId'<>target.workspace_id::text OR
     p_document->>'projectId'<>target.project_id::text OR
     p_document->>'runId'<>target.run_id::text OR
     (p_document->>'version')::bigint<>target.version+1 OR
     p_document#>>'{source,repository}'<>target.source_repository OR
     p_document#>>'{source,commit}'<>target.source_commit OR
     p_document->>'projectManifestDigest'<>target.project_manifest_digest OR
     p_document->>'planDigest'<>target.plan_digest OR
     p_document#>'{template}'<>(SELECT project.manifest#>'{template}' FROM public.development_projects project WHERE project.project_id=target.project_id AND project.workspace_id=target.workspace_id) OR
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
  IF NOT EXISTS (
    SELECT 1 FROM public.development_projects project
    JOIN public.sandbox_templates template ON template.id=project.publication_template_id AND template.workspace_id=project.workspace_id
    WHERE project.project_id=target.project_id AND project.workspace_id=target.workspace_id
      AND p_document#>>'{publicationTarget,templateId}'=project.publication_template_id::text
      AND (p_document#>>'{publicationTarget,expectedDraftVersion}')::bigint=template.draft_revision
      AND p_document#>>'{publicationTarget,candidateDigest}'='sha256:'||trim(template.draft_digest)
  ) THEN RETURN false; END IF;

  reference_id := (p_document#>>'{finalization,referenceBuild,id}')::uuid;
  IF reference_id=target.id OR NOT EXISTS (
    SELECT 1 FROM public.development_builds reference
    WHERE reference.id=reference_id AND reference.workspace_id=target.workspace_id
      AND reference.project_id=target.project_id AND reference.status='succeeded'
      AND reference.final_document->>'receiptDigest'=p_document#>>'{finalization,referenceBuild,receiptDigest}'
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

  eligible := (p_document#>>'{publication,eligible}')::boolean;
  SELECT coalesce(array_agg(value), '{}'::text[]) INTO reasons
    FROM jsonb_array_elements_text(coalesce(p_document#>'{publication,refusalReasons}','[]'::jsonb)) value;
  IF p_document#>'{publication,published}' <> 'null'::jsonb OR
     (eligible AND (target_status<>'succeeded' OR cardinality(reasons)<>0)) OR
     (NOT eligible AND cardinality(reasons)=0) THEN RETURN false; END IF;

  UPDATE public.development_builds SET status=target_status,version=version+1,
    publication_eligible=eligible,refusal_reasons=reasons,final_document=p_document,
    started_at=coalesce(started_at,clock_timestamp()),completed_at=clock_timestamp(),
    error_code=CASE WHEN target_status='failed' THEN p_document->>'errorCode' ELSE NULL END
    WHERE id=target.id AND version=p_expected_version;
  RETURN FOUND;
END
$$;

GRANT SELECT, INSERT, UPDATE ON TABLE development_projects TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE development_builds TO blazn_runtime;
REVOKE DELETE ON TABLE development_projects, development_builds FROM blazn_runtime;
REVOKE UPDATE ON TABLE development_builds FROM blazn_runtime;
REVOKE ALL ON TABLE development_projects, development_builds FROM PUBLIC, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION development_controller_finalize(uuid,bigint,jsonb)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
