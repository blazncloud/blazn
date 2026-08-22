-- Freeze Sandbox lifetime against one database clock sample. The original
-- timestamp entrypoint remains for old binaries but is no longer executable by
-- the runtime role after this migration.
CREATE FUNCTION sandbox_create_bound_sandbox_for_duration(
  p_sandbox_id uuid, p_workspace_id uuid, p_template_version_id uuid, p_architecture text,
  p_allocation_mode text, p_expires_in_seconds integer, p_queue_name text, p_sources jsonb,
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
  effective_expires_at timestamptz;
BEGIN
  IF p_expires_in_seconds < 60 OR p_expires_in_seconds > 7200 THEN
    RAISE EXCEPTION 'sandbox expiry out of bounds' USING ERRCODE='22023';
  END IF;
  effective_expires_at := effective_now + make_interval(secs => p_expires_in_seconds);
  IF NOT EXISTS (
    SELECT 1 FROM public.workspace_memberships m
    WHERE m.workspace_id=p_workspace_id AND m.user_id=p_actor_user_id
      AND m.status='active' AND m.role IN ('owner','administrator','operator')
  ) THEN
    RAISE EXCEPTION 'sandbox create denied' USING ERRCODE='42501';
  END IF;
  SELECT * INTO version_row FROM public.sandbox_template_versions
    WHERE id=p_template_version_id AND workspace_id=p_workspace_id;
  IF NOT FOUND OR NOT EXISTS (
    SELECT 1 FROM public.sandbox_template_version_status s
    WHERE s.version_id=p_template_version_id AND s.status='published'
  ) THEN
    RAISE EXCEPTION 'sandbox template version unavailable' USING ERRCODE='P0002';
  END IF;
  SELECT * INTO template_row FROM public.sandbox_templates
    WHERE id=version_row.template_id AND workspace_id=p_workspace_id;
  SELECT * INTO variant_row FROM public.sandbox_template_version_variants
    WHERE version_id=p_template_version_id AND architecture=p_architecture;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'sandbox architecture unavailable' USING ERRCODE='P0002';
  END IF;
  IF jsonb_typeof(p_sources) <> 'array'
     OR jsonb_array_length(p_sources) <> (
       SELECT count(*) FROM public.sandbox_template_version_repositories
       WHERE version_id=p_template_version_id)
     OR (SELECT count(DISTINCT value->>'repository') FROM jsonb_array_elements(p_sources)) <> jsonb_array_length(p_sources)
     OR EXISTS (
       SELECT 1 FROM jsonb_array_elements(p_sources) source
       WHERE NOT EXISTS (
         SELECT 1 FROM public.sandbox_template_version_repositories repository
         WHERE repository.version_id=p_template_version_id
           AND repository.name=source->>'repository')) THEN
    RAISE EXCEPTION 'sandbox sources must exactly cover selected repositories' USING ERRCODE='23514';
  END IF;
  SELECT jsonb_build_object(
      'items',coalesce(jsonb_agg(jsonb_build_object(
        'name',a.name,'path',a.path,'mediaType',a.media_type,'required',a.required)
        ORDER BY a.name),'[]'::jsonb))
    INTO expected_contract
    FROM public.sandbox_template_version_artifacts a
    WHERE a.version_id=p_template_version_id;
  BEGIN
    decoded_contract := convert_from(p_artifact_contract_canonical,'UTF8')::jsonb;
  EXCEPTION WHEN others THEN
    RAISE EXCEPTION 'invalid canonical artifact contract bytes' USING ERRCODE='22023';
  END;
  IF decoded_contract <> expected_contract
     OR p_artifact_contract_digest !~ '^[0-9a-f]{64}$'
     OR encode(public.digest(p_artifact_contract_canonical,'sha256'),'hex') <> p_artifact_contract_digest THEN
    RAISE EXCEPTION 'sandbox artifact contract digest mismatch' USING ERRCODE='23514';
  END IF;
  INSERT INTO public.sandboxes(
    id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,
    variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,
    artifact_contract_digest,isolation,approved_non_sensitive,expires_at)
  VALUES(
    p_sandbox_id,p_workspace_id,p_actor_user_id,version_row.template_id,p_template_version_id,template_row.name,
    version_row.version,version_row.content_digest,variant_row.name,variant_row.image_index_digest,
    variant_row.image_child_digest,p_architecture,p_allocation_mode,'requested','ready',p_queue_name,
    p_artifact_contract_digest,'approved-non-sensitive-poc',true,effective_expires_at);
  FOR item IN SELECT value FROM jsonb_array_elements(p_sources) LOOP
    INSERT INTO public.sandbox_sources(
      sandbox_id,workspace_id,template_version_id,repository_name,commit)
    VALUES(p_sandbox_id,p_workspace_id,p_template_version_id,item->>'repository',item->>'commit');
  END LOOP;
  INSERT INTO public.sandbox_artifact_contract_entries(
    sandbox_id,workspace_id,template_version_id,name,path,media_type,required)
  SELECT p_sandbox_id,p_workspace_id,p_template_version_id,a.name,a.path,a.media_type,a.required
    FROM public.sandbox_template_version_artifacts a
    WHERE a.version_id=p_template_version_id;
  RETURN p_sandbox_id;
END
$$;

REVOKE ALL ON FUNCTION sandbox_create_bound_sandbox(
  uuid, uuid, uuid, text, text, timestamptz, text, jsonb, bytea, text, uuid)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker;
REVOKE ALL ON FUNCTION sandbox_create_bound_sandbox_for_duration(
  uuid, uuid, uuid, text, text, integer, text, jsonb, bytea, text, uuid)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker;
GRANT EXECUTE ON FUNCTION sandbox_create_bound_sandbox_for_duration(
  uuid, uuid, uuid, text, text, integer, text, jsonb, bytea, text, uuid)
  TO blazn_runtime;
