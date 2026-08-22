-- Phase 5 contract freeze: additive, forward-only sandbox persistence.
-- Runtime routes/controllers are intentionally not enabled by this migration.

ALTER TABLE sessions ADD CONSTRAINT sessions_id_user_unique UNIQUE (id, user_id);
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE OR REPLACE FUNCTION workspace_json_contains_secret_key(input_value jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
  entry record;
  normalized_key text;
BEGIN
  IF jsonb_typeof(input_value) = 'object' THEN
    FOR entry IN SELECT pair.key, pair.value AS child FROM jsonb_each(input_value) AS pair LOOP
      normalized_key := regexp_replace(lower(entry.key), '[^a-z0-9]', '', 'g');
      IF normalized_key IN ('token', 'invitetoken', 'accesstoken', 'refreshtoken', 'authorization',
        'password', 'secret', 'credential', 'apikey', 'privatekey', 'clientsecret', 'sessiontoken',
        'bearertoken', 'signingkey') THEN
        RETURN true;
      END IF;
      IF workspace_json_contains_secret_key(entry.child) THEN RETURN true; END IF;
    END LOOP;
  ELSIF jsonb_typeof(input_value) = 'array' THEN
    FOR entry IN SELECT item.value AS child FROM jsonb_array_elements(input_value) AS item LOOP
      IF workspace_json_contains_secret_key(entry.child) THEN RETURN true; END IF;
    END LOOP;
  END IF;
  RETURN false;
END;
$$;

CREATE TABLE sandbox_templates (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  draft_revision bigint NOT NULL DEFAULT 1 CHECK (draft_revision > 0),
  draft_spec jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(draft_spec)),
  draft_digest char(64) NOT NULL CHECK (draft_digest ~ '^[0-9a-f]{64}$'),
  current_published_version_id uuid,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, workspace_id),
  UNIQUE (id, workspace_id, name),
  UNIQUE (workspace_id, name)
);

CREATE TABLE sandbox_template_versions (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_id uuid NOT NULL,
  version text NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
  canonical_spec bytea NOT NULL CHECK (octet_length(canonical_spec) BETWEEN 2 AND 1048576),
  spec jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(spec)),
  content_digest char(64) NOT NULL CHECK (content_digest ~ '^[0-9a-f]{64}$'),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, workspace_id),
  UNIQUE (id, workspace_id, template_id),
  UNIQUE (id, workspace_id, template_id, version, content_digest),
  UNIQUE (template_id, version),
  UNIQUE (template_id, content_digest),
  FOREIGN KEY (template_id, workspace_id) REFERENCES sandbox_templates(id, workspace_id) ON DELETE CASCADE
);

ALTER TABLE sandbox_templates ADD CONSTRAINT sandbox_templates_current_version_fk
  FOREIGN KEY (current_published_version_id, workspace_id, id)
  REFERENCES sandbox_template_versions(id, workspace_id, template_id)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE sandbox_template_version_status (
  version_id uuid PRIMARY KEY REFERENCES sandbox_template_versions(id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_id uuid NOT NULL,
  status text NOT NULL CHECK (status IN ('published', 'deprecated', 'prohibited')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  changed_by uuid NOT NULL REFERENCES users(id),
  changed_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (version_id, workspace_id, template_id)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id) ON DELETE CASCADE
);

CREATE TABLE sandbox_template_version_variants (
  version_id uuid NOT NULL,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_id uuid NOT NULL,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  architecture text NOT NULL CHECK (architecture IN ('amd64', 'arm64')),
  image_index_digest text NOT NULL CHECK (image_index_digest ~ '^.+@sha256:[0-9a-f]{64}$'),
  image_child_digest text NOT NULL CHECK (image_child_digest ~ '^.+@sha256:[0-9a-f]{64}$'),
  placement_profile text NOT NULL CHECK (placement_profile IN ('poc-linux-amd64-v1', 'poc-mac-arm64-v1')),
  command jsonb NOT NULL CHECK (jsonb_typeof(command) = 'array' AND NOT workspace_json_contains_secret_key(command)),
  resources jsonb NOT NULL CHECK (jsonb_typeof(resources) = 'object' AND NOT workspace_json_contains_secret_key(resources)),
  PRIMARY KEY (version_id, architecture),
  UNIQUE (version_id, name),
  UNIQUE (version_id, workspace_id, name, architecture, image_index_digest, image_child_digest),
  FOREIGN KEY (version_id, workspace_id, template_id)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id) ON DELETE CASCADE,
  CHECK ((architecture = 'amd64' AND placement_profile = 'poc-linux-amd64-v1') OR
    (architecture = 'arm64' AND placement_profile = 'poc-mac-arm64-v1'))
);

CREATE TABLE sandbox_template_version_repositories (
  version_id uuid NOT NULL,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_id uuid NOT NULL,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  url text NOT NULL CHECK (url ~ '^https://'),
  destination text NOT NULL CHECK (destination ~ '^/workspace/src(/[A-Za-z0-9._-]+)+$' AND destination !~ '/\.\.?(/|$)'),
  writable boolean NOT NULL,
  PRIMARY KEY (version_id, name),
  UNIQUE (version_id, destination),
  UNIQUE (version_id, workspace_id, name),
  FOREIGN KEY (version_id, workspace_id, template_id)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id) ON DELETE CASCADE
);

CREATE TABLE sandbox_template_version_artifacts (
  version_id uuid NOT NULL,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_id uuid NOT NULL,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  path text NOT NULL CHECK (path ~ '^/workspace/artifacts(/[A-Za-z0-9._-]+)+$' AND path !~ '/\.\.?(/|$)'),
  media_type text NOT NULL CHECK (char_length(media_type) BETWEEN 3 AND 128),
  required boolean NOT NULL,
  PRIMARY KEY (version_id, name),
  UNIQUE (version_id, path),
  UNIQUE (version_id, workspace_id, name, path, media_type, required),
  FOREIGN KEY (version_id, workspace_id, template_id)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id) ON DELETE CASCADE
);

CREATE FUNCTION sandbox_validate_template_version_children() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
  target_id uuid;
  template_spec jsonb;
BEGIN
  IF TG_TABLE_NAME = 'sandbox_template_versions' THEN
    target_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
  ELSE
    target_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.version_id ELSE NEW.version_id END;
  END IF;
  SELECT spec INTO template_spec FROM public.sandbox_template_versions WHERE id = target_id;
  IF template_spec IS NULL THEN RETURN NULL; END IF;
  IF jsonb_array_length(coalesce(template_spec->'variants', '[]'::jsonb)) < 1 OR
     jsonb_array_length(coalesce(template_spec->'variants', '[]'::jsonb)) <> (SELECT count(*) FROM public.sandbox_template_version_variants WHERE version_id=target_id) OR
     EXISTS (SELECT 1 FROM jsonb_array_elements(template_spec->'variants') item WHERE NOT EXISTS
       (SELECT 1 FROM public.sandbox_template_version_variants v WHERE v.version_id=target_id AND v.name=item->>'name' AND v.architecture=item->>'architecture'
         AND v.image_index_digest=item->>'imageIndex' AND v.image_child_digest=item->>'imageDigest'
         AND v.placement_profile=item->>'placementProfile' AND v.command=item->'command' AND v.resources=item->'resources')) THEN
    RAISE EXCEPTION 'normalized sandbox template variants do not exactly match spec' USING ERRCODE='23514';
  END IF;
  IF jsonb_array_length(coalesce(template_spec->'repositories', '[]'::jsonb)) <> (SELECT count(*) FROM public.sandbox_template_version_repositories WHERE version_id=target_id) OR
     EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(template_spec->'repositories', '[]'::jsonb)) item WHERE NOT EXISTS
       (SELECT 1 FROM public.sandbox_template_version_repositories r WHERE r.version_id=target_id AND r.name=item->>'name' AND r.url=item->>'url'
         AND r.destination=item->>'destination' AND r.writable=(item->>'writable')::boolean)) THEN
    RAISE EXCEPTION 'normalized sandbox template repositories do not exactly match spec' USING ERRCODE='23514';
  END IF;
  IF jsonb_array_length(coalesce(template_spec->'artifacts', '[]'::jsonb)) <> (SELECT count(*) FROM public.sandbox_template_version_artifacts WHERE version_id=target_id) OR
     EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(template_spec->'artifacts', '[]'::jsonb)) item WHERE NOT EXISTS
       (SELECT 1 FROM public.sandbox_template_version_artifacts a WHERE a.version_id=target_id AND a.name=item->>'name' AND a.path=item->>'path'
         AND a.media_type=item->>'mediaType' AND a.required=(item->>'required')::boolean)) THEN
    RAISE EXCEPTION 'normalized sandbox template artifacts do not exactly match spec' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER sandbox_template_version_children_complete
AFTER INSERT OR UPDATE ON sandbox_template_versions DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_template_version_children();
CREATE CONSTRAINT TRIGGER sandbox_template_variant_children_complete
AFTER INSERT OR UPDATE OR DELETE ON sandbox_template_version_variants DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_template_version_children();
CREATE CONSTRAINT TRIGGER sandbox_template_repository_children_complete
AFTER INSERT OR UPDATE OR DELETE ON sandbox_template_version_repositories DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_template_version_children();
CREATE CONSTRAINT TRIGGER sandbox_template_artifact_children_complete
AFTER INSERT OR UPDATE OR DELETE ON sandbox_template_version_artifacts DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_template_version_children();

CREATE FUNCTION sandbox_publish_template_version(
  p_version_id uuid, p_workspace_id uuid, p_template_id uuid, p_expected_draft_revision bigint,
  p_canonical_spec bytea, p_content_digest text, p_actor_user_id uuid)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
  template_row public.sandbox_templates%ROWTYPE;
  decoded_spec jsonb;
  item jsonb;
BEGIN
  SELECT * INTO template_row FROM public.sandbox_templates
    WHERE id=p_template_id AND workspace_id=p_workspace_id FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'sandbox template not found' USING ERRCODE='P0002'; END IF;
  IF template_row.draft_revision <> p_expected_draft_revision THEN RAISE EXCEPTION 'sandbox template draft version conflict' USING ERRCODE='40001'; END IF;
  IF NOT EXISTS (SELECT 1 FROM public.workspace_memberships m WHERE m.workspace_id=p_workspace_id AND m.user_id=p_actor_user_id AND m.status='active' AND m.role IN ('owner','administrator')) THEN
    RAISE EXCEPTION 'sandbox template publish denied' USING ERRCODE='42501';
  END IF;
  BEGIN decoded_spec := convert_from(p_canonical_spec,'UTF8')::jsonb;
  EXCEPTION WHEN others THEN RAISE EXCEPTION 'invalid canonical sandbox template bytes' USING ERRCODE='22023'; END;
  IF decoded_spec <> template_row.draft_spec THEN RAISE EXCEPTION 'canonical sandbox template bytes do not equal draft spec' USING ERRCODE='23514'; END IF;
  IF p_content_digest !~ '^[0-9a-f]{64}$' OR encode(public.digest(p_canonical_spec,'sha256'),'hex') <> p_content_digest THEN
    RAISE EXCEPTION 'sandbox template canonical digest mismatch' USING ERRCODE='23514';
  END IF;
  INSERT INTO public.sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by)
    VALUES(p_version_id,p_workspace_id,p_template_id,decoded_spec->>'version',p_canonical_spec,decoded_spec,p_content_digest,p_actor_user_id);
  FOR item IN SELECT value FROM jsonb_array_elements(decoded_spec->'variants') LOOP
    INSERT INTO public.sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile,command,resources)
      VALUES(p_version_id,p_workspace_id,p_template_id,item->>'name',item->>'architecture',item->>'imageIndex',item->>'imageDigest',item->>'placementProfile',item->'command',item->'resources');
  END LOOP;
  FOR item IN SELECT value FROM jsonb_array_elements(coalesce(decoded_spec->'repositories','[]'::jsonb)) LOOP
    INSERT INTO public.sandbox_template_version_repositories(version_id,workspace_id,template_id,name,url,destination,writable)
      VALUES(p_version_id,p_workspace_id,p_template_id,item->>'name',item->>'url',item->>'destination',(item->>'writable')::boolean);
  END LOOP;
  FOR item IN SELECT value FROM jsonb_array_elements(coalesce(decoded_spec->'artifacts','[]'::jsonb)) LOOP
    INSERT INTO public.sandbox_template_version_artifacts(version_id,workspace_id,template_id,name,path,media_type,required)
      VALUES(p_version_id,p_workspace_id,p_template_id,item->>'name',item->>'path',item->>'mediaType',(item->>'required')::boolean);
  END LOOP;
  INSERT INTO public.sandbox_template_version_status(version_id,workspace_id,template_id,status,changed_by)
    VALUES(p_version_id,p_workspace_id,p_template_id,'published',p_actor_user_id);
  UPDATE public.sandbox_templates SET current_published_version_id=p_version_id,updated_at=clock_timestamp() WHERE id=p_template_id;
  RETURN p_version_id;
END
$$;

CREATE FUNCTION sandbox_reject_immutable_version_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
  RAISE EXCEPTION 'sandbox template versions are immutable' USING ERRCODE = '55000';
END
$$;

CREATE TRIGGER sandbox_template_versions_immutable
BEFORE UPDATE OR DELETE ON sandbox_template_versions
FOR EACH ROW EXECUTE FUNCTION sandbox_reject_immutable_version_mutation();

CREATE TABLE sandboxes (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  requested_by uuid NOT NULL REFERENCES users(id),
  template_id uuid NOT NULL,
  template_version_id uuid NOT NULL,
  template_name text NOT NULL,
  template_version text NOT NULL,
  template_digest char(64) NOT NULL CHECK (template_digest ~ '^[0-9a-f]{64}$'),
  variant_name text NOT NULL,
  image_index_digest text NOT NULL CHECK (image_index_digest ~ '@sha256:[0-9a-f]{64}$'),
  image_child_digest text NOT NULL CHECK (image_child_digest ~ '@sha256:[0-9a-f]{64}$'),
  architecture text NOT NULL CHECK (architecture IN ('amd64', 'arm64')),
  allocation_mode text NOT NULL CHECK (allocation_mode IN ('direct', 'claim')),
  state text NOT NULL CHECK (state IN ('requested', 'queued', 'provisioning', 'ready', 'running', 'stopping', 'stopped', 'deleting', 'deleted', 'failed')),
  desired_state text NOT NULL CHECK (desired_state IN ('ready', 'stopped', 'deleted')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  backend_uid text CHECK (backend_uid IS NULL OR char_length(backend_uid) BETWEEN 1 AND 128),
  backend_resource_version text CHECK (backend_resource_version IS NULL OR char_length(backend_resource_version) BETWEEN 1 AND 128),
  queue_name text NOT NULL CHECK (char_length(queue_name) BETWEEN 1 AND 128),
  admission_id text CHECK (admission_id IS NULL OR char_length(admission_id) BETWEEN 1 AND 128),
  artifact_contract_digest char(64) NOT NULL CHECK (artifact_contract_digest ~ '^[0-9a-f]{64}$'),
  conditions jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(conditions) = 'array' AND NOT workspace_json_contains_secret_key(conditions)),
  isolation text NOT NULL CHECK (isolation = 'approved-non-sensitive-poc'),
  approved_non_sensitive boolean NOT NULL CHECK (approved_non_sensitive),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  stopped_at timestamptz,
  deleted_at timestamptz,
  UNIQUE (id, workspace_id),
  UNIQUE (id, workspace_id, requested_by),
  UNIQUE (id, workspace_id, template_version_id),
  FOREIGN KEY (template_id, workspace_id, template_name) REFERENCES sandbox_templates(id, workspace_id, name),
  FOREIGN KEY (template_version_id, workspace_id, template_id, template_version, template_digest)
    REFERENCES sandbox_template_versions(id, workspace_id, template_id, version, content_digest),
  FOREIGN KEY (template_version_id, workspace_id, variant_name, architecture, image_index_digest, image_child_digest)
    REFERENCES sandbox_template_version_variants(version_id, workspace_id, name, architecture, image_index_digest, image_child_digest),
  CHECK (expires_at > created_at),
  CHECK ((backend_uid IS NULL) = (backend_resource_version IS NULL)),
  CHECK (state NOT IN ('stopped', 'deleted') OR stopped_at IS NOT NULL),
  CHECK ((state = 'deleted') = (deleted_at IS NOT NULL))
);

CREATE TABLE sandbox_sources (
  sandbox_id uuid NOT NULL,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_version_id uuid NOT NULL,
  repository_name text NOT NULL,
  commit text NOT NULL CHECK (commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
  PRIMARY KEY (sandbox_id, repository_name),
  FOREIGN KEY (sandbox_id, workspace_id, template_version_id) REFERENCES sandboxes(id, workspace_id, template_version_id) ON DELETE CASCADE,
  FOREIGN KEY (template_version_id, workspace_id, repository_name)
    REFERENCES sandbox_template_version_repositories(version_id, workspace_id, name)
);

CREATE TABLE sandbox_artifact_contract_entries (
  sandbox_id uuid NOT NULL,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  template_version_id uuid NOT NULL,
  name text NOT NULL,
  path text NOT NULL,
  media_type text NOT NULL,
  required boolean NOT NULL,
  PRIMARY KEY (sandbox_id, name),
  UNIQUE (sandbox_id, path),
  UNIQUE (sandbox_id, name, path),
  FOREIGN KEY (sandbox_id, workspace_id, template_version_id) REFERENCES sandboxes(id, workspace_id, template_version_id) ON DELETE CASCADE,
  FOREIGN KEY (template_version_id, workspace_id, name, path, media_type, required)
    REFERENCES sandbox_template_version_artifacts(version_id, workspace_id, name, path, media_type, required)
);

CREATE FUNCTION sandbox_validate_create_children() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
  target_id uuid;
  selected_version uuid;
BEGIN
  IF TG_TABLE_NAME = 'sandboxes' THEN
    target_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
  ELSE
    target_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.sandbox_id ELSE NEW.sandbox_id END;
  END IF;
  SELECT template_version_id INTO selected_version FROM public.sandboxes WHERE id=target_id;
  IF selected_version IS NULL THEN RETURN NULL; END IF;
  IF EXISTS (
    (SELECT name FROM public.sandbox_template_version_repositories WHERE version_id=selected_version
     EXCEPT SELECT repository_name FROM public.sandbox_sources WHERE sandbox_id=target_id)
    UNION ALL
    (SELECT repository_name FROM public.sandbox_sources WHERE sandbox_id=target_id
     EXCEPT SELECT name FROM public.sandbox_template_version_repositories WHERE version_id=selected_version)
  ) THEN
    RAISE EXCEPTION 'sandbox sources must cover every selected template repository exactly once' USING ERRCODE='23514';
  END IF;
  IF EXISTS (
    (SELECT name,path,media_type,required FROM public.sandbox_template_version_artifacts WHERE version_id=selected_version
     EXCEPT SELECT name,path,media_type,required FROM public.sandbox_artifact_contract_entries WHERE sandbox_id=target_id)
    UNION ALL
    (SELECT name,path,media_type,required FROM public.sandbox_artifact_contract_entries WHERE sandbox_id=target_id
     EXCEPT SELECT name,path,media_type,required FROM public.sandbox_template_version_artifacts WHERE version_id=selected_version)
  ) THEN
    RAISE EXCEPTION 'sandbox artifact contract must bind every selected template artifact exactly once' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER sandbox_create_children_complete
AFTER INSERT OR UPDATE ON sandboxes DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_create_children();
CREATE CONSTRAINT TRIGGER sandbox_source_children_complete
AFTER INSERT OR UPDATE OR DELETE ON sandbox_sources DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_create_children();
CREATE CONSTRAINT TRIGGER sandbox_artifact_contract_children_complete
AFTER INSERT OR UPDATE OR DELETE ON sandbox_artifact_contract_entries DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_validate_create_children();

CREATE FUNCTION sandbox_create_bound_sandbox(
  p_sandbox_id uuid, p_workspace_id uuid, p_template_version_id uuid, p_architecture text,
  p_allocation_mode text, p_expires_at timestamptz, p_queue_name text, p_sources jsonb,
  p_artifact_contract_canonical bytea, p_artifact_contract_digest text, p_actor_user_id uuid)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
  version_row public.sandbox_template_versions%ROWTYPE;
  template_row public.sandbox_templates%ROWTYPE;
  variant_row public.sandbox_template_version_variants%ROWTYPE;
  expected_contract jsonb;
  decoded_contract jsonb;
  item jsonb;
  effective_now timestamptz := clock_timestamp();
BEGIN
  IF NOT EXISTS (SELECT 1 FROM public.workspace_memberships m WHERE m.workspace_id=p_workspace_id AND m.user_id=p_actor_user_id AND m.status='active' AND m.role IN ('owner','administrator','operator')) THEN
    RAISE EXCEPTION 'sandbox create denied' USING ERRCODE='42501';
  END IF;
  SELECT * INTO version_row FROM public.sandbox_template_versions WHERE id=p_template_version_id AND workspace_id=p_workspace_id;
  IF NOT FOUND OR NOT EXISTS (SELECT 1 FROM public.sandbox_template_version_status s WHERE s.version_id=p_template_version_id AND s.status='published') THEN
    RAISE EXCEPTION 'sandbox template version unavailable' USING ERRCODE='P0002';
  END IF;
  SELECT * INTO template_row FROM public.sandbox_templates WHERE id=version_row.template_id AND workspace_id=p_workspace_id;
  SELECT * INTO variant_row FROM public.sandbox_template_version_variants WHERE version_id=p_template_version_id AND architecture=p_architecture;
  IF NOT FOUND THEN RAISE EXCEPTION 'sandbox architecture unavailable' USING ERRCODE='P0002'; END IF;
  IF p_expires_at < effective_now + interval '60 seconds' OR p_expires_at > effective_now + interval '7200 seconds' THEN RAISE EXCEPTION 'sandbox expiry out of bounds' USING ERRCODE='22023'; END IF;
  IF jsonb_typeof(p_sources) <> 'array' OR jsonb_array_length(p_sources) <> (SELECT count(*) FROM public.sandbox_template_version_repositories WHERE version_id=p_template_version_id) OR
     (SELECT count(DISTINCT value->>'repository') FROM jsonb_array_elements(p_sources)) <> jsonb_array_length(p_sources) OR
     EXISTS (SELECT 1 FROM jsonb_array_elements(p_sources) source WHERE NOT EXISTS
       (SELECT 1 FROM public.sandbox_template_version_repositories repository WHERE repository.version_id=p_template_version_id AND repository.name=source->>'repository')) THEN
    RAISE EXCEPTION 'sandbox sources must exactly cover selected repositories' USING ERRCODE='23514';
  END IF;
  SELECT jsonb_build_object('items',coalesce(jsonb_agg(jsonb_build_object('name',a.name,'path',a.path,'mediaType',a.media_type,'required',a.required) ORDER BY a.name),'[]'::jsonb))
    INTO expected_contract FROM public.sandbox_template_version_artifacts a WHERE a.version_id=p_template_version_id;
  BEGIN decoded_contract := convert_from(p_artifact_contract_canonical,'UTF8')::jsonb;
  EXCEPTION WHEN others THEN RAISE EXCEPTION 'invalid canonical artifact contract bytes' USING ERRCODE='22023'; END;
  IF decoded_contract <> expected_contract OR p_artifact_contract_digest !~ '^[0-9a-f]{64}$' OR
     encode(public.digest(p_artifact_contract_canonical,'sha256'),'hex') <> p_artifact_contract_digest THEN
    RAISE EXCEPTION 'sandbox artifact contract digest mismatch' USING ERRCODE='23514';
  END IF;
  INSERT INTO public.sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,
    variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,artifact_contract_digest,
    isolation,approved_non_sensitive,expires_at)
    VALUES(p_sandbox_id,p_workspace_id,p_actor_user_id,version_row.template_id,p_template_version_id,template_row.name,version_row.version,
      version_row.content_digest,variant_row.name,variant_row.image_index_digest,variant_row.image_child_digest,p_architecture,p_allocation_mode,
      'requested','ready',p_queue_name,p_artifact_contract_digest,'approved-non-sensitive-poc',true,p_expires_at);
  FOR item IN SELECT value FROM jsonb_array_elements(p_sources) LOOP
    INSERT INTO public.sandbox_sources(sandbox_id,workspace_id,template_version_id,repository_name,commit)
      VALUES(p_sandbox_id,p_workspace_id,p_template_version_id,item->>'repository',item->>'commit');
  END LOOP;
  INSERT INTO public.sandbox_artifact_contract_entries(sandbox_id,workspace_id,template_version_id,name,path,media_type,required)
    SELECT p_sandbox_id,p_workspace_id,p_template_version_id,a.name,a.path,a.media_type,a.required
      FROM public.sandbox_template_version_artifacts a WHERE a.version_id=p_template_version_id;
  RETURN p_sandbox_id;
END
$$;

CREATE TABLE sandbox_operation_terminal_receipts (
  id uuid PRIMARY KEY,
  operation_id uuid NOT NULL UNIQUE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid NOT NULL,
  operation_type text NOT NULL CHECK (operation_type IN ('create', 'stop', 'delete')),
  status text NOT NULL CHECK (status IN ('succeeded', 'failed', 'recovery_required')),
  result jsonb CHECK (result IS NULL OR NOT workspace_json_contains_secret_key(result)),
  error jsonb CHECK (error IS NULL OR NOT workspace_json_contains_secret_key(error)),
  cleanup_complete boolean NOT NULL,
  artifact_export_complete boolean NOT NULL,
  grants_revoked boolean NOT NULL,
  backend_destroyed boolean NOT NULL,
  backend_present boolean NOT NULL,
  backend_uid text,
  backend_resource_version text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, operation_id, workspace_id, sandbox_id, operation_type, status),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id),
  CHECK ((backend_uid IS NULL) = (backend_resource_version IS NULL)),
  CHECK (backend_present = (backend_uid IS NOT NULL)),
  CHECK (status <> 'succeeded' OR
    (operation_type = 'create' AND backend_present AND NOT backend_destroyed) OR
    (operation_type IN ('stop','delete') AND NOT backend_present AND cleanup_complete AND artifact_export_complete AND grants_revoked AND backend_destroyed))
);

CREATE TABLE sandbox_operations (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid NOT NULL,
  type text NOT NULL CHECK (type IN ('create', 'stop', 'delete')),
  status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'recovery_required')),
  expected_sandbox_version bigint NOT NULL CHECK (expected_sandbox_version > 0),
  requested_by uuid NOT NULL REFERENCES users(id),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  terminal_receipt_id uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  completed_at timestamptz,
  UNIQUE (requested_by, type, idempotency_key),
  UNIQUE (id, workspace_id, sandbox_id),
  UNIQUE (id, workspace_id, sandbox_id, type, status),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id),
  FOREIGN KEY (terminal_receipt_id, id, workspace_id, sandbox_id, type, status)
    REFERENCES sandbox_operation_terminal_receipts(id, operation_id, workspace_id, sandbox_id, operation_type, status)
    DEFERRABLE INITIALLY DEFERRED,
  CHECK ((status IN ('succeeded', 'failed', 'recovery_required')) = (completed_at IS NOT NULL)),
  CHECK ((status IN ('succeeded', 'failed', 'recovery_required')) = (terminal_receipt_id IS NOT NULL))
);

ALTER TABLE sandbox_operation_terminal_receipts ADD CONSTRAINT sandbox_operation_terminal_receipt_operation_fk
  FOREIGN KEY (operation_id, workspace_id, sandbox_id, operation_type, status)
  REFERENCES sandbox_operations(id, workspace_id, sandbox_id, type, status)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE sandbox_events (
  id uuid PRIMARY KEY,
  operation_id uuid,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK (sequence >= 0),
  type text NOT NULL CHECK (char_length(type) BETWEEN 1 AND 96),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (NOT workspace_json_contains_secret_key(payload)),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (sandbox_id, sequence),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (operation_id, workspace_id, sandbox_id)
    REFERENCES sandbox_operations(id, workspace_id, sandbox_id) ON DELETE CASCADE
);

CREATE TABLE sandbox_access_grants (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid NOT NULL,
  user_id uuid NOT NULL REFERENCES users(id),
  session_id uuid NOT NULL,
  scope text NOT NULL CHECK (scope IN ('sandbox.exec', 'sandbox.upload', 'sandbox.download')),
  kind text NOT NULL CHECK (kind IN ('exec', 'upload', 'download')),
  token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
  token_key_id text NOT NULL CHECK (token_key_id = 'sandbox-access-grant/v1'),
  state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'consumed', 'expired', 'revoked')),
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, workspace_id, sandbox_id),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (session_id, user_id) REFERENCES sessions(id, user_id) ON DELETE CASCADE,
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '60 seconds'),
  CHECK (scope = 'sandbox.' || kind),
  CHECK ((state = 'consumed') = (consumed_at IS NOT NULL)),
  CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
  CHECK (NOT (consumed_at IS NOT NULL AND revoked_at IS NOT NULL))
);

CREATE FUNCTION sandbox_enforce_grant_transition() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
  IF (NEW.workspace_id, NEW.sandbox_id, NEW.user_id, NEW.session_id, NEW.scope, NEW.kind,
      NEW.token_hash, NEW.token_key_id, NEW.expires_at, NEW.created_at) IS DISTINCT FROM
     (OLD.workspace_id, OLD.sandbox_id, OLD.user_id, OLD.session_id, OLD.scope, OLD.kind,
      OLD.token_hash, OLD.token_key_id, OLD.expires_at, OLD.created_at) THEN
    RAISE EXCEPTION 'sandbox access grant identity is immutable' USING ERRCODE='55000';
  END IF;
  IF OLD.state <> 'active' OR NEW.state NOT IN ('consumed', 'expired', 'revoked') THEN
    RAISE EXCEPTION 'sandbox access grant state is monotonic' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER sandbox_access_grants_monotonic
BEFORE UPDATE ON sandbox_access_grants
FOR EACH ROW EXECUTE FUNCTION sandbox_enforce_grant_transition();

CREATE FUNCTION sandbox_consume_access_grant(p_grant_id uuid, p_token_hash char(64), p_kind text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE changed bigint; effective_now timestamptz := clock_timestamp();
BEGIN
  UPDATE public.sandbox_access_grants SET state='expired'
    WHERE id=p_grant_id AND state='active' AND expires_at <= effective_now;
  UPDATE public.sandbox_access_grants SET state='consumed', consumed_at=effective_now
    WHERE id=p_grant_id AND state='active' AND expires_at > effective_now
      AND token_hash=p_token_hash AND kind=p_kind;
  GET DIAGNOSTICS changed = ROW_COUNT;
  RETURN changed = 1;
END
$$;

CREATE FUNCTION sandbox_revoke_access_grants(p_workspace_id uuid, p_sandbox_id uuid)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE changed bigint; effective_now timestamptz := clock_timestamp();
BEGIN
  UPDATE public.sandbox_access_grants SET state='revoked', revoked_at=effective_now
    WHERE workspace_id=p_workspace_id AND sandbox_id=p_sandbox_id AND state='active';
  GET DIAGNOSTICS changed = ROW_COUNT;
  RETURN changed;
END
$$;

CREATE TABLE sandbox_artifacts (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid NOT NULL,
  name text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  path text NOT NULL CHECK (path ~ '^/workspace/artifacts(/[A-Za-z0-9._-]+)+$' AND path !~ '/\.\.?(/|$)'),
  object_key text NOT NULL CHECK (object_key ~ '^workspaces/[0-9a-f-]{36}/sandboxes/[0-9a-f-]{36}/artifacts/'),
  media_type text NOT NULL CHECK (char_length(media_type) BETWEEN 3 AND 128),
  content_digest char(64) NOT NULL CHECK (content_digest ~ '^[0-9a-f]{64}$'),
  size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
  exported_at timestamptz NOT NULL,
  UNIQUE (sandbox_id, name),
  UNIQUE (workspace_id, object_key),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (sandbox_id, name, path) REFERENCES sandbox_artifact_contract_entries(sandbox_id, name, path),
  CHECK (object_key = 'workspaces/' || workspace_id::text || '/sandboxes/' || sandbox_id::text || '/artifacts/' || name)
);

CREATE TABLE sandbox_idempotency_receipts (
  principal_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  operation text NOT NULL CHECK (char_length(operation) BETWEEN 1 AND 96 AND operation <> 'sandbox.access_grant.create'),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  response_status integer NOT NULL CHECK (response_status BETWEEN 200 AND 599),
  response_body jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(response_body)),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (principal_id, operation, idempotency_key)
);

CREATE TABLE sandbox_audit_events (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid,
  actor_user_id uuid REFERENCES users(id),
  actor_session_id uuid,
  event_type text NOT NULL CHECK (char_length(event_type) BETWEEN 1 AND 96),
  override_used boolean NOT NULL DEFAULT false,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (NOT workspace_json_contains_secret_key(payload)),
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id),
  FOREIGN KEY (actor_session_id, actor_user_id) REFERENCES sessions(id, user_id),
  CHECK ((actor_session_id IS NULL) = (actor_user_id IS NULL))
);

CREATE INDEX sandbox_templates_workspace_created_idx ON sandbox_templates(workspace_id, created_at, id);
CREATE INDEX sandbox_template_versions_template_created_idx ON sandbox_template_versions(template_id, created_at, id);
CREATE INDEX sandboxes_workspace_state_created_idx ON sandboxes(workspace_id, state, created_at, id);
CREATE INDEX sandbox_access_grants_active_expiry_idx ON sandbox_access_grants(expires_at) WHERE state = 'active';
CREATE INDEX sandbox_audit_workspace_created_idx ON sandbox_audit_events(workspace_id, created_at, id);

REVOKE ALL ON TABLE sandbox_templates, sandbox_template_versions, sandbox_template_version_status,
  sandbox_template_version_variants, sandbox_template_version_repositories, sandbox_template_version_artifacts,
  sandboxes, sandbox_sources, sandbox_artifact_contract_entries, sandbox_operation_terminal_receipts, sandbox_operations, sandbox_events,
  sandbox_access_grants, sandbox_artifacts, sandbox_idempotency_receipts, sandbox_audit_events
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker;
REVOKE ALL ON FUNCTION sandbox_reject_immutable_version_mutation(), sandbox_validate_template_version_children(),
  sandbox_publish_template_version(uuid, uuid, uuid, bigint, bytea, text, uuid),
  sandbox_validate_create_children(), sandbox_create_bound_sandbox(uuid, uuid, uuid, text, text, timestamptz, text, jsonb, bytea, text, uuid),
  sandbox_enforce_grant_transition(), sandbox_consume_access_grant(uuid, char(64), text),
  sandbox_revoke_access_grants(uuid, uuid)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker;

GRANT SELECT, INSERT ON TABLE sandbox_templates TO blazn_runtime;
GRANT UPDATE (draft_revision, draft_spec, draft_digest, updated_at)
  ON TABLE sandbox_templates TO blazn_runtime;
GRANT SELECT ON TABLE sandbox_template_versions TO blazn_runtime;
GRANT SELECT ON TABLE sandbox_template_version_variants, sandbox_template_version_repositories,
  sandbox_template_version_artifacts TO blazn_runtime;
GRANT SELECT ON TABLE sandbox_template_version_status TO blazn_runtime;
GRANT UPDATE (status, version, changed_by, changed_at) ON TABLE sandbox_template_version_status TO blazn_runtime;
GRANT SELECT ON TABLE sandboxes TO blazn_runtime;
GRANT SELECT ON TABLE sandbox_sources, sandbox_artifact_contract_entries TO blazn_runtime;
GRANT UPDATE (state, desired_state, version, backend_uid, backend_resource_version, queue_name,
  admission_id, conditions, updated_at, stopped_at, deleted_at) ON TABLE sandboxes TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE sandbox_operation_terminal_receipts, sandbox_operations, sandbox_access_grants TO blazn_runtime;
GRANT UPDATE (status, terminal_receipt_id, started_at, completed_at) ON TABLE sandbox_operations TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE sandbox_events, sandbox_artifacts,
  sandbox_idempotency_receipts, sandbox_audit_events TO blazn_runtime;

GRANT EXECUTE ON FUNCTION sandbox_publish_template_version(uuid, uuid, uuid, bigint, bytea, text, uuid),
  sandbox_create_bound_sandbox(uuid, uuid, uuid, text, text, timestamptz, text, jsonb, bytea, text, uuid),
  sandbox_consume_access_grant(uuid, char(64), text), sandbox_revoke_access_grants(uuid, uuid) TO blazn_runtime;

-- Immutable columns and stored grant bindings cannot be changed by the runtime role.
REVOKE UPDATE, DELETE ON TABLE sandbox_template_versions FROM blazn_runtime;
