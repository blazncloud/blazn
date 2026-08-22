-- Phase 5 database authority for the sandbox reconciler.  The controller role
-- receives function execution only: it cannot read or mutate sandbox tables.

ALTER TABLE sandbox_operations ADD CONSTRAINT sandbox_operations_controller_identity_unique
  UNIQUE (id, workspace_id, sandbox_id, type);

ALTER TABLE sandbox_template_version_repositories ADD CONSTRAINT sandbox_repository_url_has_no_inline_capability
  CHECK (url !~ '^https://[^/]*@' AND url !~ '[?#]');

-- These pre-existing trigger functions must retain migration-owner authority
-- when a table mutation originates inside one of the controller functions.
ALTER FUNCTION sandbox_validate_create_children() SECURITY DEFINER;
ALTER FUNCTION sandbox_enforce_terminal_receipt() SECURITY DEFINER;

CREATE TABLE sandbox_reconcile_jobs (
  operation_id uuid PRIMARY KEY REFERENCES sandbox_operations(id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  sandbox_id uuid NOT NULL,
  operation_type text NOT NULL CHECK (operation_type IN ('create', 'stop', 'delete')),
  attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 5),
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  lease_owner text CHECK (lease_owner IS NULL OR lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
  lease_token uuid,
  lease_expires_at timestamptz,
  last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,95}$'),
  last_error_message text CHECK (last_error_message IS NULL OR char_length(last_error_message) BETWEEN 1 AND 512),
  completed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (operation_id, workspace_id, sandbox_id, operation_type),
  FOREIGN KEY (operation_id, workspace_id, sandbox_id, operation_type)
    REFERENCES sandbox_operations(id, workspace_id, sandbox_id, type) ON DELETE CASCADE,
  CHECK ((lease_owner IS NULL) = (lease_token IS NULL) AND (lease_token IS NULL) = (lease_expires_at IS NULL)),
  CHECK (completed_at IS NULL OR lease_token IS NULL)
);

CREATE INDEX sandbox_reconcile_jobs_due_idx
  ON sandbox_reconcile_jobs(next_attempt_at, created_at, operation_id)
  WHERE completed_at IS NULL;

CREATE FUNCTION sandbox_controller_enqueue_operation() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
  IF NEW.status IN ('pending','running') THEN
    INSERT INTO public.sandbox_reconcile_jobs(operation_id,workspace_id,sandbox_id,operation_type)
      VALUES(NEW.id,NEW.workspace_id,NEW.sandbox_id,NEW.type);
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER sandbox_operation_controller_enqueue
AFTER INSERT ON sandbox_operations
FOR EACH ROW EXECUTE FUNCTION sandbox_controller_enqueue_operation();

CREATE FUNCTION sandbox_controller_finish_job() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
  IF NEW.status IN ('succeeded','failed','recovery_required') AND OLD.status IS DISTINCT FROM NEW.status THEN
    UPDATE public.sandbox_reconcile_jobs SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,
      completed_at=coalesce(NEW.completed_at,clock_timestamp()),updated_at=clock_timestamp()
      WHERE operation_id=NEW.id AND completed_at IS NULL;
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER sandbox_operation_controller_finish
AFTER UPDATE OF status ON sandbox_operations
FOR EACH ROW EXECUTE FUNCTION sandbox_controller_finish_job();

CREATE FUNCTION sandbox_controller_append_event(
  p_operation_id uuid, p_workspace_id uuid, p_sandbox_id uuid, p_type text,
  p_reason_code text DEFAULT NULL, p_safe_message text DEFAULT NULL)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE next_sequence bigint;
BEGIN
  IF p_type !~ '^sandbox\.[a-z0-9_.]{1,86}$' OR
     (p_reason_code IS NOT NULL AND p_reason_code !~ '^[a-z][a-z0-9_]{0,95}$') OR
     (p_safe_message IS NOT NULL AND char_length(p_safe_message) NOT BETWEEN 1 AND 512) THEN
    RAISE EXCEPTION 'invalid sandbox controller event' USING ERRCODE='22023';
  END IF;
  SELECT coalesce(max(sequence)+1,0) INTO next_sequence
    FROM public.sandbox_events WHERE sandbox_id=p_sandbox_id;
  INSERT INTO public.sandbox_events(id,operation_id,workspace_id,sandbox_id,sequence,type,payload)
    VALUES(gen_random_uuid(),p_operation_id,p_workspace_id,p_sandbox_id,next_sequence,p_type,
      CASE WHEN p_reason_code IS NULL THEN '{}'::jsonb
           ELSE jsonb_build_object('reasonCode',p_reason_code,'message',p_safe_message) END);
END
$$;

CREATE FUNCTION sandbox_controller_operation_is_current(
  p_type text, p_status text, p_expected_version bigint, p_attempt_count integer,
  p_state text, p_desired_state text, p_version bigint,
  p_backend_uid text, p_backend_resource_version text, p_admission_id text)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
SET search_path = pg_catalog, public
RETURN CASE p_type
  WHEN 'create' THEN p_desired_state='ready' AND (
    (p_status='pending' AND p_attempt_count=0 AND p_state='requested' AND p_version=p_expected_version
      AND p_backend_uid IS NULL AND p_backend_resource_version IS NULL AND p_admission_id IS NULL) OR
    (p_attempt_count>0 AND p_state='queued' AND p_version=p_expected_version+1
      AND p_backend_uid IS NULL AND p_backend_resource_version IS NULL AND p_admission_id IS NULL) OR
    (p_attempt_count>0 AND p_state='provisioning' AND p_version=p_expected_version+2
      AND p_backend_uid IS NOT NULL AND p_backend_resource_version IS NOT NULL AND p_admission_id IS NOT NULL)
  )
  WHEN 'stop' THEN (p_status='pending' OR (p_status='running' AND p_attempt_count>0)) AND
    p_state='stopping' AND p_desired_state='stopped' AND p_version=p_expected_version+1
  WHEN 'delete' THEN (p_status='pending' OR (p_status='running' AND p_attempt_count>0)) AND
    p_state='deleting' AND p_desired_state='deleted' AND p_version=p_expected_version+1
  ELSE false
END;

-- Only pristine API-created pending operations are safe to activate during the
-- upgrade.  A legacy running row or a row whose optimistic version/state no
-- longer identifies the same Sandbox intent is terminally quarantined without
-- changing the newer Sandbox state.
DO $legacy_backfill$
DECLARE target record; recovery_receipt uuid; effective_now timestamptz; ambiguous_sandboxes uuid[];
BEGIN
  SELECT coalesce(array_agg(sandbox_id),'{}'::uuid[]) INTO ambiguous_sandboxes
    FROM (SELECT sandbox_id FROM public.sandbox_operations WHERE status IN ('pending','running')
      GROUP BY sandbox_id HAVING count(*)>1) duplicates;
  FOR target IN
    SELECT o.id,o.workspace_id,o.sandbox_id,o.type,o.status,o.expected_sandbox_version,
      s.state,s.desired_state,s.version,s.backend_uid,s.backend_resource_version,s.admission_id
    FROM public.sandbox_operations o
    JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
    WHERE o.status IN ('pending','running')
    ORDER BY o.created_at,o.id
    FOR UPDATE OF o,s
  LOOP
    IF target.status='pending' AND NOT target.sandbox_id=ANY(ambiguous_sandboxes) AND
       public.sandbox_controller_operation_is_current(target.type,target.status,target.expected_sandbox_version,0,
        target.state,target.desired_state,target.version,target.backend_uid,target.backend_resource_version,target.admission_id) THEN
      INSERT INTO public.sandbox_reconcile_jobs(operation_id,workspace_id,sandbox_id,operation_type)
        VALUES(target.id,target.workspace_id,target.sandbox_id,target.type)
        ON CONFLICT (operation_id) DO NOTHING;
      CONTINUE;
    END IF;
    effective_now := clock_timestamp(); recovery_receipt := gen_random_uuid();
    INSERT INTO public.sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,operation_type,status,
      result,error,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,backend_present,backend_uid,backend_resource_version)
    VALUES(recovery_receipt,target.id,target.workspace_id,target.sandbox_id,target.type,'recovery_required',NULL,
      jsonb_build_object('code','legacy_operation_incompatible','message','legacy operation no longer matches Sandbox state','requestId',gen_random_uuid()::text),
      false,false,false,false,target.backend_uid IS NOT NULL,target.backend_uid,target.backend_resource_version);
    UPDATE public.sandbox_operations SET status='recovery_required',terminal_receipt_id=recovery_receipt,completed_at=effective_now
      WHERE id=target.id;
    PERFORM public.sandbox_controller_append_event(target.id,target.workspace_id,target.sandbox_id,
      'sandbox.operation.recovery_required','legacy_operation_incompatible','legacy operation no longer matches Sandbox state');
  END LOOP;
END
$legacy_backfill$;

CREATE UNIQUE INDEX sandbox_operations_one_nonterminal_per_sandbox_idx
  ON sandbox_operations(sandbox_id)
  WHERE status IN ('pending', 'running');

CREATE FUNCTION sandbox_controller_claim(p_worker_id text, p_lease_seconds integer)
RETURNS TABLE(
  operation_id uuid, workspace_id uuid, sandbox_id uuid, operation_type text,
  expected_sandbox_version bigint, lease_token uuid, lease_expires_at timestamptz,
  attempt integer, allocation_mode text, desired_state text, architecture text,
  template_version_id uuid, template_digest text, variant_name text,
  image_index_digest text, image_child_digest text, placement_profile text,
  command text[], request_cpu text, request_memory text, request_ephemeral_storage text,
  limit_cpu text, limit_memory text, limit_ephemeral_storage text,
  queue_name text, admission_id text, backend_uid text, backend_resource_version text,
  expires_at timestamptz, source_names text[], source_urls text[], source_destinations text[],
  source_writable boolean[], source_commits text[])
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE selected record; effective_now timestamptz; new_token uuid; recovery_receipt uuid;
BEGIN
  IF p_worker_id IS NULL OR p_lease_seconds IS NULL OR
     p_worker_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR p_lease_seconds NOT BETWEEN 5 AND 300 THEN
    RAISE EXCEPTION 'invalid sandbox controller lease request' USING ERRCODE='22023';
  END IF;
  LOOP
    effective_now := clock_timestamp();
    SELECT j.operation_id,j.workspace_id,j.sandbox_id,j.operation_type,j.attempt_count,
           j.lease_token IS NOT NULL AS recovered,o.status,o.expected_sandbox_version,
           s.state,s.desired_state,s.version,s.backend_uid,s.backend_resource_version,s.admission_id
      INTO selected
      FROM public.sandbox_reconcile_jobs j
      JOIN public.sandbox_operations o ON o.id=j.operation_id
      JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
      WHERE j.completed_at IS NULL AND o.status IN ('pending','running')
        AND (j.lease_token IS NULL OR j.lease_expires_at<=effective_now)
        AND ((j.attempt_count<5 AND j.next_attempt_at<=effective_now) OR j.attempt_count>=5)
      ORDER BY (j.attempt_count>=5) DESC,j.next_attempt_at,j.created_at,j.operation_id
      FOR UPDATE OF j,o,s SKIP LOCKED LIMIT 1;
    IF NOT FOUND THEN RETURN; END IF;

    IF NOT public.sandbox_controller_operation_is_current(selected.operation_type,selected.status,
        selected.expected_sandbox_version,selected.attempt_count,selected.state,selected.desired_state,
        selected.version,selected.backend_uid,selected.backend_resource_version,selected.admission_id) THEN
      recovery_receipt := gen_random_uuid();
      INSERT INTO public.sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,operation_type,status,
        result,error,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,backend_present,backend_uid,backend_resource_version)
      VALUES(recovery_receipt,selected.operation_id,selected.workspace_id,selected.sandbox_id,selected.operation_type,'recovery_required',NULL,
        jsonb_build_object('code','stale_sandbox_operation','message','operation no longer matches Sandbox state','requestId',gen_random_uuid()::text),
        false,false,false,false,selected.backend_uid IS NOT NULL,selected.backend_uid,selected.backend_resource_version);
      UPDATE public.sandbox_operations SET status='recovery_required',terminal_receipt_id=recovery_receipt,completed_at=effective_now
        WHERE id=selected.operation_id;
      UPDATE public.sandbox_reconcile_jobs SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,
        last_error_code='stale_sandbox_operation',last_error_message='operation no longer matches Sandbox state',updated_at=effective_now
        WHERE operation_id=selected.operation_id;
      PERFORM public.sandbox_controller_append_event(selected.operation_id,selected.workspace_id,selected.sandbox_id,
        'sandbox.operation.recovery_required','stale_sandbox_operation','operation no longer matches Sandbox state');
      CONTINUE;
    END IF;

    IF selected.attempt_count>=5 THEN
      recovery_receipt := gen_random_uuid();
      INSERT INTO public.sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,operation_type,status,
        result,error,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,backend_present,backend_uid,backend_resource_version)
      VALUES(recovery_receipt,selected.operation_id,selected.workspace_id,selected.sandbox_id,selected.operation_type,'recovery_required',NULL,
        jsonb_build_object('code','lease_attempts_exhausted','message','controller lease attempts exhausted','requestId',gen_random_uuid()::text),
        false,false,false,false,selected.backend_uid IS NOT NULL,selected.backend_uid,selected.backend_resource_version);
      UPDATE public.sandbox_operations SET status='recovery_required',terminal_receipt_id=recovery_receipt,completed_at=effective_now
        WHERE id=selected.operation_id;
      UPDATE public.sandbox_reconcile_jobs SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,
        last_error_code='lease_attempts_exhausted',last_error_message='controller lease attempts exhausted',updated_at=effective_now
        WHERE operation_id=selected.operation_id;
      UPDATE public.sandboxes SET state='failed',version=version+1,updated_at=effective_now WHERE id=selected.sandbox_id;
      PERFORM public.sandbox_controller_append_event(selected.operation_id,selected.workspace_id,selected.sandbox_id,
        'sandbox.operation.recovery_required','lease_attempts_exhausted','controller lease attempts exhausted');
      RETURN;
    END IF;
    EXIT;
  END LOOP;

  new_token := gen_random_uuid();
  UPDATE public.sandbox_reconcile_jobs j SET lease_owner=p_worker_id,lease_token=new_token,
      lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds),
      attempt_count=j.attempt_count+1,updated_at=effective_now
    WHERE j.operation_id=selected.operation_id;
  UPDATE public.sandbox_operations SET status='running',started_at=coalesce(started_at,effective_now)
    WHERE id=selected.operation_id AND status IN ('pending','running');
  UPDATE public.sandboxes SET state='queued',version=version+1,updated_at=effective_now
    WHERE id=selected.sandbox_id AND selected.operation_type='create' AND state='requested';
  PERFORM public.sandbox_controller_append_event(selected.operation_id,selected.workspace_id,selected.sandbox_id,
    CASE WHEN selected.recovered THEN 'sandbox.operation.lease_recovered' ELSE 'sandbox.operation.claimed' END);

  RETURN QUERY
  SELECT o.id,o.workspace_id,o.sandbox_id,o.type,o.expected_sandbox_version,new_token,
    effective_now+make_interval(secs=>p_lease_seconds),selected.attempt_count+1,
    s.allocation_mode,s.desired_state,s.architecture,s.template_version_id,s.template_digest::text,
    s.variant_name,s.image_index_digest,s.image_child_digest,v.placement_profile,
    ARRAY(SELECT jsonb_array_elements_text(v.command)),
    v.resources->'requests'->>'cpu',v.resources->'requests'->>'memory',v.resources->'requests'->>'ephemeralStorage',
    v.resources->'limits'->>'cpu',v.resources->'limits'->>'memory',v.resources->'limits'->>'ephemeralStorage',
    s.queue_name,s.admission_id,s.backend_uid,s.backend_resource_version,s.expires_at,
    coalesce(src.names,'{}'::text[]),coalesce(src.urls,'{}'::text[]),coalesce(src.destinations,'{}'::text[]),
    coalesce(src.writable,'{}'::boolean[]),coalesce(src.commits,'{}'::text[])
  FROM public.sandbox_operations o
  JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
  JOIN public.sandbox_template_version_variants v ON v.version_id=s.template_version_id AND v.architecture=s.architecture
  LEFT JOIN LATERAL (
    SELECT array_agg(ss.repository_name ORDER BY ss.repository_name) names,
      array_agg(r.url ORDER BY ss.repository_name) urls,
      array_agg(r.destination ORDER BY ss.repository_name) destinations,
      array_agg(r.writable ORDER BY ss.repository_name) writable,
      array_agg(ss.commit ORDER BY ss.repository_name) commits
    FROM public.sandbox_sources ss
    JOIN public.sandbox_template_version_repositories r
      ON r.version_id=ss.template_version_id AND r.name=ss.repository_name
    WHERE ss.sandbox_id=s.id
  ) src ON true
  WHERE o.id=selected.operation_id;
END
$$;

CREATE FUNCTION sandbox_controller_renew(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid, p_lease_seconds integer)
RETURNS timestamptz
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE effective_now timestamptz := clock_timestamp(); renewed_until timestamptz;
BEGIN
  IF p_worker_id IS NULL OR p_lease_seconds IS NULL OR
     p_worker_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR p_lease_seconds NOT BETWEEN 5 AND 300 THEN
    RAISE EXCEPTION 'invalid sandbox controller lease renewal' USING ERRCODE='22023';
  END IF;
  UPDATE public.sandbox_reconcile_jobs SET lease_expires_at=effective_now+make_interval(secs=>p_lease_seconds),updated_at=effective_now
    FROM public.sandbox_operations o
    WHERE sandbox_reconcile_jobs.operation_id=p_operation_id AND o.id=sandbox_reconcile_jobs.operation_id
      AND o.status='running' AND sandbox_reconcile_jobs.completed_at IS NULL AND lease_owner=p_worker_id
      AND lease_token=p_lease_token AND lease_expires_at>effective_now
    RETURNING sandbox_reconcile_jobs.lease_expires_at INTO renewed_until;
  RETURN renewed_until;
END
$$;

CREATE FUNCTION sandbox_controller_bind_backend(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_backend_uid text, p_backend_resource_version text, p_admission_id text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; current_backend record; effective_now timestamptz := clock_timestamp();
BEGIN
  IF p_backend_uid IS NULL OR p_backend_resource_version IS NULL OR p_admission_id IS NULL OR
     char_length(p_backend_uid) NOT BETWEEN 1 AND 128 OR char_length(p_backend_resource_version) NOT BETWEEN 1 AND 128 OR
     char_length(p_admission_id) NOT BETWEEN 1 AND 128 THEN
    RAISE EXCEPTION 'invalid sandbox backend identity' USING ERRCODE='22023';
  END IF;
  SELECT o.workspace_id,o.sandbox_id,o.type INTO target
    FROM public.sandbox_reconcile_jobs j JOIN public.sandbox_operations o ON o.id=j.operation_id
    WHERE j.operation_id=p_operation_id AND j.completed_at IS NULL AND j.lease_owner=p_worker_id
      AND j.lease_token=p_lease_token AND j.lease_expires_at>effective_now AND o.status='running'
    FOR UPDATE OF j,o;
  IF NOT FOUND OR target.type<>'create' THEN RETURN false; END IF;
  SELECT backend_uid,backend_resource_version,admission_id INTO current_backend
    FROM public.sandboxes WHERE id=target.sandbox_id AND workspace_id=target.workspace_id FOR UPDATE;
  IF (current_backend.backend_uid,current_backend.backend_resource_version,current_backend.admission_id) IS NOT DISTINCT FROM
      (p_backend_uid,p_backend_resource_version,p_admission_id) THEN RETURN true; END IF;
  IF current_backend.backend_uid IS NOT NULL OR current_backend.backend_resource_version IS NOT NULL OR current_backend.admission_id IS NOT NULL THEN
    RETURN false;
  END IF;
  UPDATE public.sandboxes SET backend_uid=p_backend_uid,backend_resource_version=p_backend_resource_version,
      admission_id=p_admission_id,state='provisioning',version=version+1,updated_at=effective_now
    WHERE id=target.sandbox_id AND workspace_id=target.workspace_id
      AND backend_uid IS NULL AND backend_resource_version IS NULL AND admission_id IS NULL;
  IF NOT FOUND THEN RETURN false; END IF;
  PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,'sandbox.backend.bound');
  RETURN true;
END
$$;

CREATE FUNCTION sandbox_controller_retry(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_delay_seconds integer, p_error_code text, p_safe_message text, p_request_id uuid)
RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; effective_now timestamptz := clock_timestamp(); receipt_id uuid;
BEGIN
  IF p_delay_seconds IS NULL OR p_error_code IS NULL OR p_safe_message IS NULL OR p_request_id IS NULL OR
     p_delay_seconds NOT BETWEEN 1 AND 3600 OR p_error_code !~ '^[a-z][a-z0-9_]{0,95}$' OR
     char_length(p_safe_message) NOT BETWEEN 1 AND 512 THEN
    RAISE EXCEPTION 'invalid sandbox controller retry' USING ERRCODE='22023';
  END IF;
  SELECT o.workspace_id,o.sandbox_id,o.type,j.attempt_count,s.backend_uid,s.backend_resource_version
    INTO target
    FROM public.sandbox_reconcile_jobs j JOIN public.sandbox_operations o ON o.id=j.operation_id
    JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
    WHERE j.operation_id=p_operation_id AND j.completed_at IS NULL AND j.lease_owner=p_worker_id
      AND j.lease_token=p_lease_token AND j.lease_expires_at>effective_now AND o.status='running'
    FOR UPDATE OF j,o,s;
  IF NOT FOUND THEN RETURN 'fenced'; END IF;
  IF target.attempt_count<5 THEN
    UPDATE public.sandbox_reconcile_jobs SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,
      next_attempt_at=effective_now+make_interval(secs=>p_delay_seconds),last_error_code=p_error_code,
      last_error_message=p_safe_message,updated_at=effective_now WHERE operation_id=p_operation_id;
    UPDATE public.sandbox_operations SET status='pending' WHERE id=p_operation_id;
    PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
      'sandbox.operation.retry_scheduled',p_error_code,p_safe_message);
    RETURN 'retry_scheduled';
  END IF;
  receipt_id := gen_random_uuid();
  INSERT INTO public.sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,operation_type,status,
    result,error,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,backend_present,backend_uid,backend_resource_version)
  VALUES(receipt_id,p_operation_id,target.workspace_id,target.sandbox_id,target.type,'recovery_required',NULL,
    jsonb_build_object('code',p_error_code,'message',p_safe_message,'requestId',p_request_id::text),
    false,false,false,false,target.backend_uid IS NOT NULL,target.backend_uid,target.backend_resource_version);
  UPDATE public.sandbox_operations SET status='recovery_required',terminal_receipt_id=receipt_id,completed_at=effective_now WHERE id=p_operation_id;
  UPDATE public.sandbox_reconcile_jobs SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,
    last_error_code=p_error_code,last_error_message=p_safe_message,updated_at=effective_now WHERE operation_id=p_operation_id;
  UPDATE public.sandboxes SET state='failed',version=version+1,updated_at=effective_now WHERE id=target.sandbox_id;
  PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
    'sandbox.operation.recovery_required',p_error_code,p_safe_message);
  RETURN 'recovery_required';
END
$$;

CREATE FUNCTION sandbox_controller_complete(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid, p_status text,
  p_expected_backend_uid text, p_expected_backend_resource_version text, p_expected_admission_id text,
  p_cleanup_complete boolean, p_artifact_export_complete boolean, p_grants_revoked boolean,
  p_backend_destroyed boolean, p_artifact_ids uuid[], p_warning_codes text[],
  p_error_code text, p_safe_message text, p_request_id uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; effective_now timestamptz := clock_timestamp(); receipt_id uuid := gen_random_uuid();
  backend_present boolean; result_value jsonb; error_value jsonb;
BEGIN
  IF p_status IS NULL OR p_artifact_ids IS NULL OR p_warning_codes IS NULL OR
     p_cleanup_complete IS NULL OR p_artifact_export_complete IS NULL OR p_grants_revoked IS NULL OR p_backend_destroyed IS NULL OR
     p_status NOT IN ('succeeded','failed','recovery_required') OR coalesce(array_length(p_artifact_ids,1),0)>128 OR
     coalesce(array_length(p_warning_codes,1),0)>32 OR EXISTS(SELECT 1 FROM unnest(coalesce(p_warning_codes,'{}')) v WHERE v !~ '^[a-z][a-z0-9_]{0,95}$') THEN
    RAISE EXCEPTION 'invalid sandbox completion' USING ERRCODE='22023';
  END IF;
  IF p_status='succeeded' AND (p_error_code IS NOT NULL OR p_safe_message IS NOT NULL OR p_request_id IS NOT NULL) OR
     p_status<>'succeeded' AND (p_error_code IS NULL OR p_safe_message IS NULL OR p_request_id IS NULL OR
       p_error_code !~ '^[a-z][a-z0-9_]{0,95}$' OR char_length(p_safe_message) NOT BETWEEN 1 AND 512) THEN
    RAISE EXCEPTION 'invalid sandbox completion error identity' USING ERRCODE='22023';
  END IF;
  SELECT o.workspace_id,o.sandbox_id,o.type,s.backend_uid,s.backend_resource_version,s.admission_id
    INTO target
    FROM public.sandbox_reconcile_jobs j JOIN public.sandbox_operations o ON o.id=j.operation_id
    JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
    WHERE j.operation_id=p_operation_id AND j.completed_at IS NULL AND j.lease_owner=p_worker_id
      AND j.lease_token=p_lease_token AND j.lease_expires_at>effective_now AND o.status='running'
    FOR UPDATE OF j,o,s;
  IF NOT FOUND OR (target.backend_uid,target.backend_resource_version,target.admission_id) IS DISTINCT FROM
      (p_expected_backend_uid,p_expected_backend_resource_version,p_expected_admission_id) THEN RETURN false; END IF;
  IF EXISTS(SELECT 1 FROM unnest(coalesce(p_artifact_ids,'{}')) id WHERE NOT EXISTS
      (SELECT 1 FROM public.sandbox_artifacts a WHERE a.id=id AND a.sandbox_id=target.sandbox_id)) THEN
    RAISE EXCEPTION 'sandbox completion artifact identity mismatch' USING ERRCODE='23514';
  END IF;
  IF p_artifact_export_complete AND EXISTS(
      SELECT 1 FROM public.sandbox_artifact_contract_entries expected
      WHERE expected.sandbox_id=target.sandbox_id AND expected.required
        AND NOT EXISTS(SELECT 1 FROM public.sandbox_artifacts artifact
          WHERE artifact.sandbox_id=expected.sandbox_id AND artifact.name=expected.name
            AND artifact.id=ANY(coalesce(p_artifact_ids,'{}'::uuid[])))) THEN
    RAISE EXCEPTION 'required sandbox artifact export is incomplete' USING ERRCODE='23514';
  END IF;
  IF p_status='succeeded' AND target.type='create' AND (target.backend_uid IS NULL OR target.admission_id IS NULL OR p_backend_destroyed) THEN
    RAISE EXCEPTION 'successful create requires exact live backend and admission identity' USING ERRCODE='23514';
  END IF;
  IF p_status='succeeded' AND target.type IN ('stop','delete') AND
     (NOT p_cleanup_complete OR NOT p_artifact_export_complete OR NOT p_grants_revoked OR NOT p_backend_destroyed) THEN
    RAISE EXCEPTION 'successful cleanup completion is incomplete' USING ERRCODE='23514';
  END IF;
  IF p_grants_revoked THEN
    PERFORM public.sandbox_revoke_access_grants(target.workspace_id,target.sandbox_id);
    IF EXISTS(SELECT 1 FROM public.sandbox_access_grants WHERE sandbox_id=target.sandbox_id AND state='active') THEN
      RAISE EXCEPTION 'sandbox access grant revocation is incomplete' USING ERRCODE='23514';
    END IF;
  END IF;
  backend_present := target.backend_uid IS NOT NULL AND NOT p_backend_destroyed;
  result_value := CASE WHEN p_status='succeeded' THEN jsonb_build_object(
    'artifactIds',to_jsonb(coalesce(p_artifact_ids,'{}'::uuid[])),
    'warnings',to_jsonb(coalesce(p_warning_codes,'{}'::text[]))) ELSE NULL END;
  error_value := CASE WHEN p_status='succeeded' THEN NULL ELSE jsonb_build_object(
    'code',p_error_code,'message',p_safe_message,'requestId',p_request_id::text) END;
  INSERT INTO public.sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,operation_type,status,
    result,error,cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,backend_present,backend_uid,backend_resource_version)
  VALUES(receipt_id,p_operation_id,target.workspace_id,target.sandbox_id,target.type,p_status,result_value,error_value,
    p_cleanup_complete,p_artifact_export_complete,p_grants_revoked,p_backend_destroyed,backend_present,
    CASE WHEN backend_present THEN target.backend_uid END,CASE WHEN backend_present THEN target.backend_resource_version END);
  UPDATE public.sandbox_operations SET status=p_status,terminal_receipt_id=receipt_id,completed_at=effective_now WHERE id=p_operation_id;
  UPDATE public.sandbox_reconcile_jobs SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=effective_now,updated_at=effective_now
    WHERE operation_id=p_operation_id;
  IF p_status='succeeded' AND target.type='create' THEN
    UPDATE public.sandboxes SET state='ready',version=version+1,updated_at=effective_now WHERE id=target.sandbox_id;
  ELSIF p_status='succeeded' AND target.type='stop' THEN
    UPDATE public.sandboxes SET state='stopped',backend_uid=NULL,backend_resource_version=NULL,admission_id=NULL,
      version=version+1,stopped_at=effective_now,updated_at=effective_now WHERE id=target.sandbox_id;
  ELSIF p_status='succeeded' AND target.type='delete' THEN
    UPDATE public.sandboxes SET state='deleted',backend_uid=NULL,backend_resource_version=NULL,admission_id=NULL,
      version=version+1,stopped_at=coalesce(stopped_at,effective_now),deleted_at=effective_now,updated_at=effective_now WHERE id=target.sandbox_id;
  ELSE
    UPDATE public.sandboxes SET state='failed',
      backend_uid=CASE WHEN p_backend_destroyed THEN NULL ELSE backend_uid END,
      backend_resource_version=CASE WHEN p_backend_destroyed THEN NULL ELSE backend_resource_version END,
      admission_id=CASE WHEN p_backend_destroyed THEN NULL ELSE admission_id END,
      version=version+1,updated_at=effective_now WHERE id=target.sandbox_id;
  END IF;
  PERFORM public.sandbox_controller_append_event(p_operation_id,target.workspace_id,target.sandbox_id,
    'sandbox.operation.'||p_status,CASE WHEN p_status='succeeded' THEN NULL ELSE p_error_code END,
    CASE WHEN p_status='succeeded' THEN NULL ELSE p_safe_message END);
  RETURN true;
END
$$;

CREATE FUNCTION sandbox_controller_enqueue_expired(p_limit integer)
RETURNS TABLE(operation_id uuid, sandbox_id uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; new_operation uuid; effective_now timestamptz := clock_timestamp();
BEGIN
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100 THEN RAISE EXCEPTION 'invalid sandbox expiry batch size' USING ERRCODE='22023'; END IF;
  FOR target IN
    SELECT s.id,s.workspace_id,s.requested_by,s.version FROM public.sandboxes s
    WHERE s.expires_at<=effective_now AND s.state NOT IN ('stopping','stopped','deleting','deleted','failed')
      AND NOT EXISTS (SELECT 1 FROM public.sandbox_operations o WHERE o.sandbox_id=s.id AND o.status IN ('pending','running'))
    ORDER BY s.expires_at,s.id FOR UPDATE OF s SKIP LOCKED LIMIT p_limit
  LOOP
    new_operation := gen_random_uuid();
    UPDATE public.sandboxes SET state='stopping',desired_state='stopped',version=version+1,updated_at=effective_now WHERE id=target.id;
    INSERT INTO public.sandbox_operations(id,workspace_id,sandbox_id,type,status,expected_sandbox_version,requested_by,idempotency_key,request_digest)
      VALUES(new_operation,target.workspace_id,target.id,'stop','pending',target.version,target.requested_by,
        'expiry-'||target.id::text,encode(digest(convert_to('sandbox-expiry-v1\n'||target.id::text,'UTF8'),'sha256'),'hex'));
    PERFORM public.sandbox_controller_append_event(new_operation,target.workspace_id,target.id,'sandbox.expiry.enqueued');
    operation_id := new_operation; sandbox_id := target.id; RETURN NEXT;
  END LOOP;
END
$$;

REVOKE ALL ON TABLE sandbox_reconcile_jobs FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION sandbox_controller_enqueue_operation(), sandbox_controller_finish_job(),
  sandbox_controller_append_event(uuid,uuid,uuid,text,text,text),
  sandbox_controller_operation_is_current(text,text,bigint,integer,text,text,bigint,text,text,text),
  sandbox_controller_claim(text,integer), sandbox_controller_renew(uuid,text,uuid,integer),
  sandbox_controller_bind_backend(uuid,text,uuid,text,text,text),
  sandbox_controller_retry(uuid,text,uuid,integer,text,text,uuid),
  sandbox_controller_complete(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid),
  sandbox_controller_enqueue_expired(integer)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;

GRANT EXECUTE ON FUNCTION sandbox_controller_claim(text,integer),
  sandbox_controller_renew(uuid,text,uuid,integer),
  sandbox_controller_bind_backend(uuid,text,uuid,text,text,text),
  sandbox_controller_retry(uuid,text,uuid,integer,text,text,uuid),
  sandbox_controller_complete(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid),
  sandbox_controller_enqueue_expired(integer)
  TO blazn_sandbox_controller;

-- The API remains the only creator of ordinary operations.  The controller can
-- create only deterministic expiry-stop operations through the trusted function.
REVOKE ALL ON TABLE sandbox_reconcile_jobs FROM blazn_sandbox_controller;
