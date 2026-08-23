CREATE TABLE run_synthetic_progress (
  run_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK (sequence >= 0),
  request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  phase text NOT NULL CHECK (phase ~ '^[a-z][a-z0-9._-]{0,95}$'),
  percent integer NOT NULL CHECK (percent BETWEEN 0 AND 100),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, sequence),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id) ON DELETE CASCADE
);

CREATE TABLE synthetic_artifact_blobs (
  artifact_id uuid PRIMARY KEY REFERENCES artifacts(id) ON DELETE CASCADE,
  content bytea NOT NULL CHECK (octet_length(content) <= 16777216),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX artifacts_source_run_live_name_idx
  ON artifacts(source_run_id, name)
  WHERE source_run_id IS NOT NULL AND status <> 'deleted';

CREATE FUNCTION validate_synthetic_artifact_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  target_artifact_id uuid;
  artifact_row artifacts%ROWTYPE;
  blob_content bytea;
  run_proof_class text;
  run_requested_by uuid;
BEGIN
  IF TG_TABLE_NAME = 'artifacts' THEN
    target_artifact_id := COALESCE(NEW.id, OLD.id);
  ELSE
    target_artifact_id := COALESCE(NEW.artifact_id, OLD.artifact_id);
  END IF;
  SELECT * INTO artifact_row FROM artifacts WHERE id = target_artifact_id;
  SELECT content INTO blob_content FROM synthetic_artifact_blobs WHERE artifact_id = target_artifact_id;
  IF artifact_row.object_key LIKE 'synthetic-db/%' OR blob_content IS NOT NULL THEN
    IF artifact_row.id IS NULL OR blob_content IS NULL OR artifact_row.status <> 'ready' OR artifact_row.source_run_id IS NULL OR
       artifact_row.object_key <> 'synthetic-db/' || artifact_row.id::text OR artifact_row.size_bytes <> octet_length(blob_content) OR
       artifact_row.digest <> 'sha256:' || encode(digest(blob_content, 'sha256'), 'hex') THEN
      RAISE EXCEPTION 'synthetic Artifact blob does not match ready metadata' USING ERRCODE = '23514';
    END IF;
    SELECT proof_class, requested_by INTO run_proof_class, run_requested_by FROM runs WHERE id = artifact_row.source_run_id;
    IF run_proof_class <> 'synthetic' OR artifact_row.created_by <> run_requested_by THEN
      RAISE EXCEPTION 'synthetic Artifact authority does not match its Run' USING ERRCODE = '23514';
    END IF;
  END IF;
  IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;

CREATE CONSTRAINT TRIGGER synthetic_artifact_consistency_from_artifact
AFTER INSERT OR UPDATE ON artifacts DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_synthetic_artifact_consistency();

CREATE CONSTRAINT TRIGGER synthetic_artifact_consistency_from_blob
AFTER INSERT OR UPDATE OR DELETE ON synthetic_artifact_blobs DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_synthetic_artifact_consistency();

GRANT SELECT, INSERT ON TABLE run_synthetic_progress TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE synthetic_artifact_blobs TO blazn_runtime;
REVOKE UPDATE, DELETE ON TABLE run_synthetic_progress, synthetic_artifact_blobs FROM blazn_runtime;
REVOKE ALL ON TABLE run_synthetic_progress, synthetic_artifact_blobs FROM blazn_bootstrap;
REVOKE ALL ON FUNCTION validate_synthetic_artifact_consistency() FROM PUBLIC;
REVOKE ALL ON FUNCTION validate_synthetic_artifact_consistency() FROM blazn_bootstrap;
GRANT EXECUTE ON FUNCTION validate_synthetic_artifact_consistency() TO blazn_runtime;
