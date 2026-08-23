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

GRANT SELECT, INSERT ON TABLE run_synthetic_progress TO blazn_runtime;
GRANT INSERT ON TABLE synthetic_artifact_blobs TO blazn_runtime;
REVOKE UPDATE, DELETE ON TABLE run_synthetic_progress, synthetic_artifact_blobs FROM blazn_runtime;
REVOKE ALL ON TABLE run_synthetic_progress, synthetic_artifact_blobs FROM blazn_bootstrap;
