CREATE TABLE projects (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  slug text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$'),
  kind text NOT NULL DEFAULT 'general' CHECK (kind ~ '^[a-z][a-z0-9-]{0,62}$'),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 160),
  description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 4000),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, slug),
  UNIQUE (id, workspace_id)
);

CREATE INDEX projects_workspace_status_id_idx ON projects(workspace_id, status, id);
CREATE INDEX projects_creator_workspace_idx ON projects(created_by, workspace_id, id);

GRANT SELECT, INSERT, UPDATE ON TABLE projects TO blazn_runtime;
REVOKE DELETE ON TABLE projects FROM blazn_runtime;
REVOKE ALL ON TABLE projects FROM blazn_bootstrap;
