CREATE TABLE run_messages (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  run_id uuid NOT NULL,
  ordinal bigint NOT NULL CHECK (ordinal >= 1),
  role text NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
  kind text NOT NULL CHECK (kind IN ('prompt', 'followup', 'steer')),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'claimed', 'delivered')),
  parent_message_id uuid,
  content text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 16384),
  content_digest text NOT NULL CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
  created_by uuid NOT NULL REFERENCES users(id),
  claimed_by uuid REFERENCES users(id),
  claim_id uuid,
  lease_expires_at timestamptz,
  delivered_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, ordinal),
  UNIQUE (id, run_id),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id) ON DELETE CASCADE,
  FOREIGN KEY (parent_message_id, run_id) REFERENCES run_messages(id, run_id),
  CHECK (content_digest = 'sha256:' || encode(digest(convert_to(content, 'UTF8'), 'sha256'), 'hex')),
  CHECK ((kind = 'prompt') = (ordinal = 1)),
  CHECK (kind <> 'prompt' OR parent_message_id IS NULL),
  CHECK ((status = 'queued') = (claimed_by IS NULL AND claim_id IS NULL AND lease_expires_at IS NULL AND delivered_at IS NULL)),
  CHECK ((status = 'claimed') = (claimed_by IS NOT NULL AND claim_id IS NOT NULL AND lease_expires_at IS NOT NULL AND delivered_at IS NULL)),
  CHECK ((status = 'delivered') = (claimed_by IS NOT NULL AND claim_id IS NOT NULL AND lease_expires_at IS NULL AND delivered_at IS NOT NULL))
);

CREATE INDEX run_messages_run_ordinal_idx ON run_messages(workspace_id, project_id, run_id, ordinal);

CREATE FUNCTION validate_run_message_completion() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
  IF NEW.status = 'succeeded' AND EXISTS (
    SELECT 1 FROM public.run_messages WHERE run_id = NEW.id AND status <> 'delivered'
  ) THEN
    RAISE EXCEPTION 'successful Run requires every accepted message to be delivered' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;

CREATE CONSTRAINT TRIGGER runs_message_completion
AFTER INSERT OR UPDATE OF status ON runs DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_run_message_completion();

GRANT SELECT, INSERT ON TABLE run_messages TO blazn_runtime;
GRANT UPDATE (status, claimed_by, claim_id, lease_expires_at, delivered_at) ON TABLE run_messages TO blazn_runtime;
REVOKE DELETE ON TABLE run_messages FROM blazn_runtime;
REVOKE ALL ON FUNCTION validate_run_message_completion() FROM PUBLIC, blazn_runtime, blazn_bootstrap;
REVOKE ALL ON TABLE run_messages FROM blazn_bootstrap;
