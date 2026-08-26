CREATE TABLE run_messages (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  project_id uuid NOT NULL,
  run_id uuid NOT NULL,
  ordinal bigint NOT NULL CHECK (ordinal >= 1),
  role text NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
  kind text NOT NULL CHECK (kind IN ('prompt', 'followup', 'steer')),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'claimed', 'delivered', 'rejected')),
  parent_message_id uuid,
  content text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 16384 AND position(chr(0) IN content) = 0),
  content_digest text NOT NULL CHECK (content_digest ~ '^sha256:[0-9a-f]{64}$'),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, ordinal),
  UNIQUE (id, run_id),
  FOREIGN KEY (run_id, workspace_id, project_id) REFERENCES runs(id, workspace_id, project_id) ON DELETE CASCADE,
  FOREIGN KEY (parent_message_id, run_id) REFERENCES run_messages(id, run_id),
  CHECK (content_digest = 'sha256:' || encode(digest(convert_to(content, 'UTF8'), 'sha256'), 'hex')),
  CHECK ((kind = 'prompt') = (ordinal = 1)),
  CHECK (kind <> 'prompt' OR parent_message_id IS NULL)
);

CREATE INDEX run_messages_run_ordinal_idx ON run_messages(workspace_id, project_id, run_id, ordinal);

GRANT SELECT, INSERT ON TABLE run_messages TO blazn_runtime;
REVOKE UPDATE, DELETE ON TABLE run_messages FROM blazn_runtime;
REVOKE ALL ON TABLE run_messages FROM blazn_bootstrap;
