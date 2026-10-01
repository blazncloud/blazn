-- Agent Run execution: the pieces the Agent Run controller needs to run a
-- claimed Run inside its Sandbox, relay messages, and record results.
--
-- The controller role still has no table access. Every operation below is a
-- SECURITY DEFINER function fenced by the controller's live lease.

-- The Blazn reference harness is a reviewed kind next to the adapter kinds.
ALTER TABLE harness_definitions DROP CONSTRAINT harness_definitions_kind_check;
ALTER TABLE harness_definitions ADD CONSTRAINT harness_definitions_kind_check
  CHECK (kind IN ('hermes', 'codex-cli', 'claude-code', 'generic-cli', 'blazn-agent'));

-- Assistant replies are Run messages written by the controller. They are
-- delivered at creation and carry the harness event sequence that produced
-- them, so a replayed event cannot create a second reply.
ALTER TABLE run_messages DROP CONSTRAINT run_messages_kind_check;
ALTER TABLE run_messages ADD CONSTRAINT run_messages_kind_check CHECK (kind IN ('prompt', 'followup', 'steer', 'reply'));
ALTER TABLE run_messages ADD COLUMN source_sequence bigint CHECK (source_sequence IS NULL OR source_sequence > 0);
CREATE UNIQUE INDEX run_messages_run_source_sequence_idx ON run_messages(run_id, source_sequence) WHERE source_sequence IS NOT NULL;
ALTER TABLE run_messages DROP CONSTRAINT run_messages_check5;
ALTER TABLE run_messages ADD CONSTRAINT run_messages_delivered_check CHECK (
  (status = 'delivered') = (delivered_at IS NOT NULL AND lease_expires_at IS NULL AND
    ((role = 'user' AND claimed_by IS NOT NULL AND claim_id IS NOT NULL) OR
     (role <> 'user' AND claimed_by IS NULL AND claim_id IS NULL))));
ALTER TABLE run_messages ADD CONSTRAINT run_messages_reply_check CHECK (
  (role = 'user' AND kind <> 'reply' AND source_sequence IS NULL) OR
  (role <> 'user' AND kind = 'reply' AND status = 'delivered' AND source_sequence IS NOT NULL));

-- One execution per Agent Run: the Sandbox the API created for the requester
-- when the Run was accepted, and how far the controller has consumed the
-- harness outbox.
CREATE TABLE agent_run_executions (
  run_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  sandbox_id uuid NOT NULL UNIQUE,
  user_id uuid NOT NULL REFERENCES users(id),
  session_id uuid NOT NULL,
  outbox_sequence bigint NOT NULL DEFAULT 0 CHECK (outbox_sequence >= 0),
  released_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES agent_run_bindings(run_id, workspace_id, project_id) ON DELETE CASCADE,
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id)
);
CREATE INDEX agent_run_executions_unreleased_idx ON agent_run_executions(created_at) WHERE released_at IS NULL;

-- Output files of an Agent Run are small documents; they are stored with the
-- Run rather than in the object store.
CREATE TABLE agent_run_artifact_blobs (
  artifact_id uuid PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
  content bytea NOT NULL CHECK (octet_length(content) <= 8388608),
  created_at timestamptz NOT NULL DEFAULT now()
);

-- A controller-issued access grant is bound to its Run instead of a user
-- session: the requester may have signed out long before the Run ends.
ALTER TABLE sandbox_access_grants ADD COLUMN agent_run_id uuid REFERENCES runs(id) ON DELETE CASCADE;

-- API-side admission. Freezes the compatibility binding (agent_run_enqueue)
-- and records the Sandbox created for it, or does neither.
CREATE FUNCTION agent_run_start_execution(p_run_id uuid, p_workspace_id uuid, p_agent_version_id uuid, p_harness_profile_id uuid,
  p_sandbox_id uuid, p_user_id uuid, p_session_id uuid)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target public.runs%ROWTYPE; binding public.agent_run_bindings%ROWTYPE; existing public.agent_run_executions%ROWTYPE;
BEGIN
  SELECT * INTO target FROM public.runs WHERE id = p_run_id AND workspace_id = p_workspace_id FOR UPDATE;
  IF NOT FOUND OR target.requested_by <> p_user_id THEN RETURN 'run_not_found'; END IF;
  SELECT * INTO existing FROM public.agent_run_executions WHERE run_id = p_run_id;
  IF FOUND THEN
    RETURN CASE WHEN existing.sandbox_id = p_sandbox_id AND existing.user_id = p_user_id THEN 'accepted' ELSE 'conflict' END;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM public.sessions s WHERE s.id = p_session_id AND s.user_id = p_user_id AND s.revoked_at IS NULL) THEN
    RETURN 'session_invalid';
  END IF;
  IF NOT public.agent_run_enqueue(p_run_id, p_workspace_id, p_agent_version_id, p_harness_profile_id) THEN RETURN 'incompatible'; END IF;
  SELECT * INTO binding FROM public.agent_run_bindings WHERE run_id = p_run_id;
  IF NOT EXISTS (SELECT 1 FROM public.sandboxes s WHERE s.id = p_sandbox_id AND s.workspace_id = p_workspace_id
      AND s.requested_by = p_user_id AND s.template_version_id = binding.template_version_id
      AND 'sha256:' || trim(s.template_digest) = binding.template_digest
      AND s.state NOT IN ('stopping', 'stopped', 'deleting', 'deleted', 'failed')) THEN
    RAISE EXCEPTION 'Sandbox does not match the Agent Run binding' USING ERRCODE = '23514';
  END IF;
  INSERT INTO public.agent_run_executions(run_id, workspace_id, project_id, sandbox_id, user_id, session_id)
    VALUES (p_run_id, target.workspace_id, target.project_id, p_sandbox_id, p_user_id, p_session_id);
  RETURN 'accepted';
END $$;

-- True while the caller holds the live lease of an active Run.
CREATE FUNCTION agent_run_controller_lease_held(p_run_id uuid, p_worker_id text, p_lease_token uuid)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public AS $$
  SELECT EXISTS (SELECT 1 FROM public.agent_run_jobs job JOIN public.runs run ON run.id = job.run_id
    WHERE job.run_id = p_run_id AND job.worker_id = p_worker_id AND job.lease_token = p_lease_token
      AND job.completed_at IS NULL AND job.lease_expires_at > clock_timestamp() AND run.status IN ('queued', 'running'))
$$;

CREATE FUNCTION agent_run_controller_execution(p_run_id uuid, p_worker_id text, p_lease_token uuid)
RETURNS TABLE(sandbox_id uuid, sandbox_state text, sandbox_expires_at timestamptz, architecture text, node_id uuid,
  outbox_sequence bigint, run_status text, run_version bigint, instructions text, purpose text, repository_destination text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN; END IF;
  RETURN QUERY SELECT sandbox.id, sandbox.state, sandbox.expires_at, sandbox.architecture, observation.node_id,
      execution.outbox_sequence, run.status, run.version, av.document->>'instructions', av.document->>'purpose',
      (SELECT repository.destination FROM public.sandbox_sources source
         JOIN public.sandbox_template_version_repositories repository ON repository.version_id = source.template_version_id
           AND repository.workspace_id = source.workspace_id AND repository.name = source.repository_name
         WHERE source.sandbox_id = sandbox.id AND repository.writable ORDER BY repository.name LIMIT 1)
    FROM public.agent_run_executions execution
    JOIN public.runs run ON run.id = execution.run_id
    JOIN public.agent_run_bindings binding ON binding.run_id = execution.run_id
    JOIN public.agent_versions av ON av.id = binding.agent_version_id
    JOIN public.sandboxes sandbox ON sandbox.id = execution.sandbox_id
    LEFT JOIN public.agent_run_sandbox_node_observations observation ON observation.sandbox_id = sandbox.id
    WHERE execution.run_id = p_run_id;
END $$;

CREATE FUNCTION agent_run_controller_issue_grant(p_run_id uuid, p_worker_id text, p_lease_token uuid, p_grant_id uuid,
  p_token_hash text, p_kind text, p_ttl_seconds integer)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target record; effective_now timestamptz := clock_timestamp();
BEGIN
  IF p_kind NOT IN ('exec', 'upload', 'download') OR p_ttl_seconds NOT BETWEEN 1 AND 60 OR p_token_hash !~ '^[0-9a-f]{64}$' OR p_grant_id IS NULL THEN
    RAISE EXCEPTION 'invalid Agent Run access grant' USING ERRCODE = '22023';
  END IF;
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN false; END IF;
  SELECT execution.*, sandbox.state AS sandbox_state INTO target
    FROM public.agent_run_executions execution
    JOIN public.runs run ON run.id = execution.run_id
    JOIN public.agent_run_bindings binding ON binding.run_id = execution.run_id
    JOIN public.sandboxes sandbox ON sandbox.id = execution.sandbox_id
    WHERE execution.run_id = p_run_id AND run.status = 'running' AND binding.bound_sandbox_id = execution.sandbox_id
      AND sandbox.state IN ('ready', 'running');
  IF NOT FOUND THEN RETURN false; END IF;
  INSERT INTO public.sandbox_access_grants(id, workspace_id, sandbox_id, user_id, session_id, scope, kind, token_hash, token_key_id,
      state, expires_at, created_at, agent_run_id)
    VALUES (p_grant_id, target.workspace_id, target.sandbox_id, target.user_id, target.session_id, 'sandbox.' || p_kind, p_kind,
      p_token_hash, 'sandbox-access-grant/v1', 'active', effective_now + make_interval(secs => p_ttl_seconds), effective_now, p_run_id);
  RETURN true;
END $$;

-- The Sandbox controller consumes a grant before it touches the Sandbox. A
-- user grant needs a live session; a controller-issued grant needs its Run to
-- be running, bound to this Sandbox, and under a live controller lease.
CREATE OR REPLACE FUNCTION sandbox_controller_consume_access_grant_v1(
  p_grant_id uuid,
  p_token_hash char(64),
  p_kind text
)
RETURNS TABLE(
  workspace_id uuid,
  sandbox_id uuid,
  requested_by uuid,
  backend_uid text,
  backend_resource_version text
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
  grant_row public.sandbox_access_grants%ROWTYPE;
  sandbox_row public.sandboxes%ROWTYPE;
  authority_valid boolean;
  effective_now timestamptz := clock_timestamp();
BEGIN
  SELECT * INTO grant_row
  FROM public.sandbox_access_grants AS candidate_grant
  WHERE candidate_grant.id = p_grant_id
  FOR UPDATE;

  IF NOT FOUND THEN RETURN; END IF;
  IF grant_row.state = 'active' AND grant_row.expires_at <= effective_now THEN
    UPDATE public.sandbox_access_grants SET state='expired' WHERE id=p_grant_id;
    RETURN;
  END IF;
  IF grant_row.state <> 'active' OR grant_row.expires_at <= effective_now OR
     grant_row.token_hash <> p_token_hash OR grant_row.kind <> p_kind THEN
    RETURN;
  END IF;

  IF grant_row.agent_run_id IS NULL THEN
    SELECT EXISTS(
      SELECT 1 FROM public.sessions AS candidate_session
      WHERE candidate_session.id=grant_row.session_id AND candidate_session.user_id=grant_row.user_id
        AND candidate_session.revoked_at IS NULL AND candidate_session.access_expires_at > effective_now
        AND candidate_session.refresh_expires_at > effective_now
    ) INTO authority_valid;
  ELSE
    SELECT EXISTS(
      SELECT 1 FROM public.runs AS candidate_run
      JOIN public.agent_run_bindings AS candidate_binding ON candidate_binding.run_id=candidate_run.id
      JOIN public.agent_run_jobs AS candidate_job ON candidate_job.run_id=candidate_run.id
      WHERE candidate_run.id=grant_row.agent_run_id AND candidate_run.status='running'
        AND candidate_binding.bound_sandbox_id=grant_row.sandbox_id
        AND candidate_job.completed_at IS NULL AND candidate_job.lease_expires_at > effective_now
    ) INTO authority_valid;
  END IF;
  IF NOT authority_valid THEN RETURN; END IF;

  SELECT * INTO sandbox_row
  FROM public.sandboxes AS candidate_sandbox
  WHERE candidate_sandbox.id=grant_row.sandbox_id AND candidate_sandbox.workspace_id=grant_row.workspace_id
  FOR SHARE;
  IF NOT FOUND OR sandbox_row.state NOT IN ('ready','running') OR
     sandbox_row.backend_uid IS NULL OR sandbox_row.backend_resource_version IS NULL THEN
    RETURN;
  END IF;

  UPDATE public.sandbox_access_grants SET state='consumed', consumed_at=effective_now WHERE id=p_grant_id;

  workspace_id := sandbox_row.workspace_id;
  sandbox_id := sandbox_row.id;
  requested_by := sandbox_row.requested_by;
  backend_uid := sandbox_row.backend_uid;
  backend_resource_version := sandbox_row.backend_resource_version;
  RETURN NEXT;
END
$$;

-- Hands the controller the Run's next user message. A message it already
-- holds is returned again, so a restarted controller resumes the same turn.
CREATE FUNCTION agent_run_controller_claim_message(p_run_id uuid, p_worker_id text, p_lease_token uuid, p_lease_seconds integer)
RETURNS TABLE(message_id uuid, claim_id uuid, ordinal bigint, kind text, content text, resumed boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target public.runs%ROWTYPE; selected public.run_messages%ROWTYPE; effective_now timestamptz := clock_timestamp(); was_claimed boolean := false;
BEGIN
  IF p_lease_seconds NOT BETWEEN 5 AND 300 THEN RAISE EXCEPTION 'invalid Agent Run message lease' USING ERRCODE = '22023'; END IF;
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN; END IF;
  SELECT * INTO target FROM public.runs WHERE id = p_run_id FOR UPDATE;
  SELECT * INTO selected FROM public.run_messages m WHERE m.run_id = p_run_id AND m.role = 'user' AND m.status = 'claimed'
    ORDER BY m.ordinal LIMIT 1 FOR UPDATE;
  IF FOUND THEN
    was_claimed := true;
    UPDATE public.run_messages m SET lease_expires_at = effective_now + make_interval(secs => p_lease_seconds) WHERE m.id = selected.id;
  ELSE
    -- Same order as the Management API: the prompt, then steering, then follow-ups.
    SELECT * INTO selected FROM public.run_messages m WHERE m.run_id = p_run_id AND m.role = 'user' AND m.status = 'queued'
      ORDER BY CASE m.kind WHEN 'prompt' THEN 0 WHEN 'steer' THEN 1 ELSE 2 END, m.ordinal LIMIT 1 FOR UPDATE;
    IF NOT FOUND THEN RETURN; END IF;
    selected.claim_id := gen_random_uuid();
    UPDATE public.run_messages m SET status = 'claimed', claimed_by = target.requested_by, claim_id = selected.claim_id,
      lease_expires_at = effective_now + make_interval(secs => p_lease_seconds) WHERE m.id = selected.id;
    PERFORM public.agent_run_append_event(target.id, target.workspace_id, target.project_id, 'agent.run.message-claimed',
      jsonb_build_object('messageId', selected.id, 'ordinal', selected.ordinal));
  END IF;
  message_id := selected.id; claim_id := selected.claim_id; ordinal := selected.ordinal; kind := selected.kind;
  content := selected.content; resumed := was_claimed;
  RETURN NEXT;
END $$;

CREATE FUNCTION agent_run_controller_deliver_message(p_run_id uuid, p_worker_id text, p_lease_token uuid, p_message_id uuid, p_claim_id uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target public.runs%ROWTYPE; delivered public.run_messages%ROWTYPE;
BEGIN
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN false; END IF;
  SELECT * INTO target FROM public.runs WHERE id = p_run_id FOR UPDATE;
  UPDATE public.run_messages m SET status = 'delivered', lease_expires_at = NULL, delivered_at = clock_timestamp()
    WHERE m.run_id = p_run_id AND m.id = p_message_id AND m.role = 'user' AND m.status = 'claimed' AND m.claim_id = p_claim_id
    RETURNING m.* INTO delivered;
  IF NOT FOUND THEN
    RETURN EXISTS (SELECT 1 FROM public.run_messages m WHERE m.run_id = p_run_id AND m.id = p_message_id
      AND m.status = 'delivered' AND m.claim_id = p_claim_id);
  END IF;
  PERFORM public.agent_run_append_event(target.id, target.workspace_id, target.project_id, 'agent.run.message-delivered',
    jsonb_build_object('messageId', delivered.id, 'ordinal', delivered.ordinal));
  RETURN true;
END $$;

-- Records one assistant reply. p_sequence is the harness event sequence, so
-- recording the same event twice is a no-op.
CREATE FUNCTION agent_run_controller_record_reply(p_run_id uuid, p_worker_id text, p_lease_token uuid, p_sequence bigint,
  p_parent_message_id uuid, p_content text)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target public.runs%ROWTYPE; reply_id uuid; next_ordinal bigint;
BEGIN
  IF p_sequence IS NULL OR p_sequence < 1 OR p_content IS NULL OR char_length(p_content) NOT BETWEEN 1 AND 16384 THEN
    RAISE EXCEPTION 'invalid Agent Run reply' USING ERRCODE = '22023';
  END IF;
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN NULL; END IF;
  SELECT * INTO target FROM public.runs WHERE id = p_run_id FOR UPDATE;
  SELECT m.id INTO reply_id FROM public.run_messages m WHERE m.run_id = p_run_id AND m.source_sequence = p_sequence;
  IF FOUND THEN RETURN reply_id; END IF;
  IF p_parent_message_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.run_messages m WHERE m.run_id = p_run_id AND m.id = p_parent_message_id) THEN
    RAISE EXCEPTION 'Agent Run reply parent is not in this Run' USING ERRCODE = '23503';
  END IF;
  SELECT coalesce(max(m.ordinal) + 1, 1) INTO next_ordinal FROM public.run_messages m WHERE m.run_id = p_run_id;
  reply_id := gen_random_uuid();
  INSERT INTO public.run_messages(id, workspace_id, project_id, run_id, ordinal, role, kind, status, parent_message_id, content,
      content_digest, created_by, delivered_at, source_sequence)
    VALUES (reply_id, target.workspace_id, target.project_id, target.id, next_ordinal, 'assistant', 'reply', 'delivered', p_parent_message_id,
      p_content, 'sha256:' || encode(public.digest(convert_to(p_content, 'UTF8'), 'sha256'), 'hex'), target.requested_by,
      clock_timestamp(), p_sequence);
  PERFORM public.agent_run_append_event(target.id, target.workspace_id, target.project_id, 'agent.run.reply',
    jsonb_build_object('messageId', reply_id, 'ordinal', next_ordinal, 'parentMessageId', p_parent_message_id));
  RETURN reply_id;
END $$;

-- Appends one harness-derived event and advances the consumed outbox position.
CREATE FUNCTION agent_run_controller_record_event(p_run_id uuid, p_worker_id text, p_lease_token uuid, p_sequence bigint,
  p_type text, p_payload jsonb)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target public.agent_run_executions%ROWTYPE;
BEGIN
  IF p_sequence IS NULL OR p_sequence < 1 OR (p_type IS NOT NULL AND p_type !~ '^[a-z][a-z0-9-]{0,40}$') THEN
    RAISE EXCEPTION 'invalid Agent Run harness event' USING ERRCODE = '22023';
  END IF;
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN false; END IF;
  SELECT * INTO target FROM public.agent_run_executions WHERE run_id = p_run_id FOR UPDATE;
  IF NOT FOUND THEN RETURN false; END IF;
  IF p_sequence <= target.outbox_sequence THEN RETURN true; END IF;
  IF p_type IS NOT NULL THEN
    PERFORM public.agent_run_append_event(target.run_id, target.workspace_id, target.project_id, 'agent.run.' || p_type,
      coalesce(p_payload, '{}'::jsonb));
  END IF;
  UPDATE public.agent_run_executions SET outbox_sequence = p_sequence WHERE run_id = p_run_id;
  RETURN true;
END $$;

-- Stores one output document of the Run as a ready Artifact.
CREATE FUNCTION agent_run_controller_record_artifact(p_run_id uuid, p_worker_id text, p_lease_token uuid, p_name text, p_content bytea)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target public.runs%ROWTYPE; existing public.artifacts%ROWTYPE; artifact_id uuid; content_digest text;
BEGIN
  IF p_name NOT IN ('patch', 'summary') OR p_content IS NULL OR octet_length(p_content) > 8388608 THEN
    RAISE EXCEPTION 'invalid Agent Run artifact' USING ERRCODE = '22023';
  END IF;
  IF NOT public.agent_run_controller_lease_held(p_run_id, p_worker_id, p_lease_token) THEN RETURN NULL; END IF;
  SELECT * INTO target FROM public.runs WHERE id = p_run_id FOR UPDATE;
  IF target.status <> 'running' OR NOT p_name = ANY (target.output_names) THEN RETURN NULL; END IF;
  content_digest := 'sha256:' || encode(public.digest(p_content, 'sha256'), 'hex');
  SELECT * INTO existing FROM public.artifacts a WHERE a.source_run_id = p_run_id AND a.name = p_name AND a.status <> 'deleted';
  IF FOUND THEN
    RETURN CASE WHEN existing.digest = content_digest AND existing.status = 'ready' THEN existing.id ELSE NULL END;
  END IF;
  artifact_id := gen_random_uuid();
  INSERT INTO public.artifacts(id, workspace_id, project_id, source_run_id, kind, media_type, name, status, digest, size_bytes, object_key, created_by)
    VALUES (artifact_id, target.workspace_id, target.project_id, target.id, 'agent.' || p_name, 'document', p_name, 'ready',
      content_digest, octet_length(p_content), 'agent-run-db/' || artifact_id::text, target.requested_by);
  INSERT INTO public.agent_run_artifact_blobs(artifact_id, content) VALUES (artifact_id, p_content);
  RETURN artifact_id;
END $$;

-- Stops the Sandbox of every finished Agent Run. Runs that ended while no
-- controller held them (a user cancel, a crash) are picked up here too.
CREATE FUNCTION agent_run_controller_release_sandboxes(p_limit integer)
RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE target record; new_operation uuid; released integer := 0; effective_now timestamptz := clock_timestamp();
BEGIN
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100 THEN RAISE EXCEPTION 'invalid Agent Run release batch size' USING ERRCODE = '22023'; END IF;
  FOR target IN
    SELECT execution.run_id, sandbox.id AS sandbox_id, sandbox.workspace_id, sandbox.requested_by, sandbox.version, sandbox.state
      FROM public.agent_run_executions execution
      JOIN public.runs run ON run.id = execution.run_id
      JOIN public.sandboxes sandbox ON sandbox.id = execution.sandbox_id
      WHERE execution.released_at IS NULL AND run.status IN ('succeeded', 'failed', 'cancelled')
        AND NOT EXISTS (SELECT 1 FROM public.sandbox_operations o WHERE o.sandbox_id = sandbox.id AND o.status IN ('pending', 'running'))
      ORDER BY execution.created_at FOR UPDATE OF execution, sandbox SKIP LOCKED LIMIT p_limit
  LOOP
    IF target.state NOT IN ('stopping', 'stopped', 'deleting', 'deleted', 'failed') THEN
      new_operation := gen_random_uuid();
      UPDATE public.sandboxes SET state = 'stopping', desired_state = 'stopped', version = version + 1, updated_at = effective_now WHERE id = target.sandbox_id;
      INSERT INTO public.sandbox_operations(id, workspace_id, sandbox_id, type, status, expected_sandbox_version, requested_by, idempotency_key, request_digest)
        VALUES (new_operation, target.workspace_id, target.sandbox_id, 'stop', 'pending', target.version, target.requested_by,
          'agent-run-' || target.run_id::text,
          encode(public.digest(convert_to('agent-run-release-v1\n' || target.run_id::text, 'UTF8'), 'sha256'), 'hex'));
      PERFORM public.sandbox_controller_append_event(new_operation, target.workspace_id, target.sandbox_id, 'sandbox.agent_run.released');
    END IF;
    UPDATE public.agent_run_executions SET released_at = effective_now WHERE run_id = target.run_id;
    released := released + 1;
  END LOOP;
  RETURN released;
END $$;

REVOKE ALL ON TABLE agent_run_executions, agent_run_artifact_blobs
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller, blazn_development_controller, blazn_agent_run_controller;
GRANT SELECT ON TABLE agent_run_executions, agent_run_artifact_blobs TO blazn_runtime;
REVOKE ALL ON FUNCTION agent_run_start_execution(uuid, uuid, uuid, uuid, uuid, uuid, uuid),
  agent_run_controller_lease_held(uuid, text, uuid),
  agent_run_controller_execution(uuid, text, uuid),
  agent_run_controller_issue_grant(uuid, text, uuid, uuid, text, text, integer),
  agent_run_controller_claim_message(uuid, text, uuid, integer),
  agent_run_controller_deliver_message(uuid, text, uuid, uuid, uuid),
  agent_run_controller_record_reply(uuid, text, uuid, bigint, uuid, text),
  agent_run_controller_record_event(uuid, text, uuid, bigint, text, jsonb),
  agent_run_controller_record_artifact(uuid, text, uuid, text, bytea),
  agent_run_controller_release_sandboxes(integer)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller, blazn_development_controller, blazn_agent_run_controller;
GRANT EXECUTE ON FUNCTION agent_run_start_execution(uuid, uuid, uuid, uuid, uuid, uuid, uuid) TO blazn_runtime;
GRANT EXECUTE ON FUNCTION agent_run_controller_execution(uuid, text, uuid),
  agent_run_controller_issue_grant(uuid, text, uuid, uuid, text, text, integer),
  agent_run_controller_claim_message(uuid, text, uuid, integer),
  agent_run_controller_deliver_message(uuid, text, uuid, uuid, uuid),
  agent_run_controller_record_reply(uuid, text, uuid, bigint, uuid, text),
  agent_run_controller_record_event(uuid, text, uuid, bigint, text, jsonb),
  agent_run_controller_record_artifact(uuid, text, uuid, text, bytea),
  agent_run_controller_release_sandboxes(integer)
  TO blazn_agent_run_controller;
