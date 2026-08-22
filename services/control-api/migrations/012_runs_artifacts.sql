CREATE FUNCTION run_output_names_valid(input_value text[]) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT COALESCE(bool_and(value ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'), true)
  FROM unnest(input_value) AS value;
$$;

CREATE TABLE runs (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9.-]{0,95}$'),
  proof_class text NOT NULL CHECK (proof_class IN ('synthetic', 'local', 'sandbox', 'provider')),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  plan_digest text NOT NULL CHECK (plan_digest ~ '^sha256:[0-9a-f]{64}$'),
  output_names text[] NOT NULL DEFAULT '{}'::text[],
  requested_by uuid NOT NULL REFERENCES users(id),
  node_id uuid,
  sandbox_id uuid,
  model_route_id text CHECK (model_route_id IS NULL OR model_route_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,254}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  completed_at timestamptz,
  error_code text CHECK (error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,62}$'),
  FOREIGN KEY (project_id, workspace_id) REFERENCES projects(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id),
  FOREIGN KEY (sandbox_id, workspace_id) REFERENCES sandboxes(id, workspace_id),
  UNIQUE (id, workspace_id, project_id),
  CHECK (cardinality(output_names) <= 1000 AND run_output_names_valid(output_names)),
  CHECK ((status = 'queued' AND started_at IS NULL AND completed_at IS NULL) OR
         (status = 'running' AND started_at IS NOT NULL AND completed_at IS NULL) OR
         (status IN ('succeeded', 'failed', 'cancelled') AND completed_at IS NOT NULL)),
  CHECK (status <> 'succeeded' OR error_code IS NULL),
  CHECK (status <> 'failed' OR error_code IS NOT NULL),
  CHECK (proof_class IN ('local', 'sandbox') OR node_id IS NULL),
  CHECK (status = 'queued' OR proof_class NOT IN ('local', 'sandbox') OR node_id IS NOT NULL),
  CHECK (sandbox_id IS NULL OR proof_class = 'sandbox'),
  CHECK (status = 'queued' OR proof_class <> 'sandbox' OR sandbox_id IS NOT NULL),
  CHECK (model_route_id IS NULL OR proof_class = 'provider'),
  CHECK (status = 'queued' OR proof_class <> 'provider' OR model_route_id IS NOT NULL),
  CHECK (proof_class <> 'synthetic' OR (node_id IS NULL AND sandbox_id IS NULL AND model_route_id IS NULL))
);

CREATE TABLE run_events (
  run_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK (sequence >= 0),
  type text NOT NULL CHECK (type ~ '^[a-z][a-z0-9._-]{0,95}$'),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, sequence),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id) ON DELETE CASCADE,
  CHECK (NOT workspace_json_contains_secret_key(payload))
);

CREATE TABLE run_receipts (
  run_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  proof_class text NOT NULL CHECK (proof_class IN ('synthetic', 'local', 'sandbox', 'provider')),
  outcome text NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'cancelled')),
  plan_digest text NOT NULL CHECK (plan_digest ~ '^sha256:[0-9a-f]{64}$'),
  receipt jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id) ON DELETE CASCADE,
  CHECK (NOT workspace_json_contains_secret_key(receipt))
);

CREATE TABLE artifacts (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  source_run_id uuid,
  kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9.-]{0,95}$'),
  media_type text NOT NULL CHECK (media_type IN ('image', 'video', 'audio', 'document', 'data', 'other')),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 256),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'failed', 'deleted')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  digest text CHECK (digest IS NULL OR digest ~ '^sha256:[0-9a-f]{64}$'),
  size_bytes bigint CHECK (size_bytes IS NULL OR size_bytes >= 0),
  object_key text CHECK (object_key IS NULL OR (char_length(object_key) BETWEEN 1 AND 1024 AND object_key !~ '(^|/)\.\.?(/|$)' AND object_key !~ '[?#@]')),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (project_id, workspace_id) REFERENCES projects(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (source_run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id),
  UNIQUE (id, workspace_id, project_id),
  CHECK ((status = 'ready') = (digest IS NOT NULL AND size_bytes IS NOT NULL AND object_key IS NOT NULL)),
  CHECK (status <> 'deleted' OR object_key IS NULL)
);

CREATE TABLE run_input_artifacts (
  run_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  artifact_id uuid NOT NULL,
  ordinal integer NOT NULL CHECK (ordinal >= 0 AND ordinal < 1000),
  PRIMARY KEY (run_id, artifact_id),
  UNIQUE (run_id, ordinal),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id) ON DELETE CASCADE,
  FOREIGN KEY (artifact_id, workspace_id, project_id) REFERENCES artifacts(id, workspace_id, project_id)
);

CREATE FUNCTION validate_run_receipt_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  target_run_id uuid;
  run_row runs%ROWTYPE;
  receipt_row run_receipts%ROWTYPE;
BEGIN
  IF TG_TABLE_NAME = 'runs' THEN
    target_run_id := COALESCE(NEW.id, OLD.id);
  ELSE
    target_run_id := COALESCE(NEW.run_id, OLD.run_id);
  END IF;
  SELECT * INTO run_row FROM runs WHERE id = target_run_id;
  IF NOT FOUND THEN
    IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
  END IF;
  SELECT * INTO receipt_row FROM run_receipts WHERE run_id = target_run_id;
  IF run_row.status IN ('succeeded', 'failed', 'cancelled') THEN
    IF NOT FOUND THEN
      RAISE EXCEPTION 'terminal Run requires a receipt' USING ERRCODE = '23514';
    END IF;
    IF receipt_row.workspace_id <> run_row.workspace_id OR receipt_row.project_id <> run_row.project_id OR receipt_row.proof_class <> run_row.proof_class OR receipt_row.outcome <> run_row.status OR receipt_row.plan_digest <> run_row.plan_digest THEN
      RAISE EXCEPTION 'Run receipt does not match terminal Run' USING ERRCODE = '23514';
    END IF;
  ELSIF FOUND THEN
    RAISE EXCEPTION 'nonterminal Run cannot have a receipt' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;

CREATE CONSTRAINT TRIGGER runs_receipt_consistency_from_run
AFTER INSERT OR UPDATE ON runs DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_run_receipt_consistency();

CREATE CONSTRAINT TRIGGER runs_receipt_consistency_from_receipt
AFTER INSERT OR UPDATE OR DELETE ON run_receipts DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_run_receipt_consistency();

CREATE INDEX runs_project_status_id_idx ON runs(workspace_id, project_id, status, id);
CREATE INDEX run_events_project_created_idx ON run_events(workspace_id, project_id, created_at, run_id, sequence);
CREATE INDEX artifacts_project_status_id_idx ON artifacts(workspace_id, project_id, status, id);
CREATE INDEX artifacts_source_run_idx ON artifacts(source_run_id) WHERE source_run_id IS NOT NULL;
CREATE INDEX run_input_artifacts_artifact_idx ON run_input_artifacts(artifact_id, run_id);

GRANT SELECT, INSERT, UPDATE ON TABLE runs TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE run_events TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE run_receipts TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE artifacts TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE run_input_artifacts TO blazn_runtime;
REVOKE ALL ON FUNCTION validate_run_receipt_consistency() FROM PUBLIC;
REVOKE ALL ON FUNCTION run_output_names_valid(text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION validate_run_receipt_consistency() TO blazn_runtime;
GRANT EXECUTE ON FUNCTION run_output_names_valid(text[]) TO blazn_runtime;
REVOKE DELETE ON TABLE runs, run_events, run_receipts, artifacts, run_input_artifacts FROM blazn_runtime;
REVOKE ALL ON TABLE runs, run_events, run_receipts, artifacts, run_input_artifacts FROM blazn_bootstrap;
REVOKE ALL ON FUNCTION validate_run_receipt_consistency() FROM blazn_bootstrap;
REVOKE ALL ON FUNCTION run_output_names_valid(text[]) FROM blazn_bootstrap;
