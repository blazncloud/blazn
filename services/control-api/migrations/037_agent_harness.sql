CREATE TABLE harness_definitions (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('hermes', 'codex-cli', 'claude-code', 'generic-cli')),
  status text NOT NULL CHECK (status IN ('approved', 'deprecated', 'prohibited')),
  resource_version bigint NOT NULL CHECK (resource_version > 0),
  document jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, kind),
  UNIQUE (id, workspace_id),
  CHECK ((document ->> 'id') IS NOT NULL AND (document ->> 'id')::uuid = id),
  CHECK (document ->> 'kind' IS NOT NULL AND document ->> 'kind' = kind),
  CHECK (document ->> 'status' IS NOT NULL AND document ->> 'status' = status),
  CHECK ((document ->> 'resourceVersion') IS NOT NULL AND (document ->> 'resourceVersion')::bigint = resource_version)
);

CREATE INDEX harness_definitions_workspace_idx ON harness_definitions(workspace_id, kind);

CREATE TABLE harness_versions (
  id uuid PRIMARY KEY,
  definition_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  version text NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
  digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  document jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (definition_id, version),
  UNIQUE (definition_id, digest),
  UNIQUE (id, workspace_id),
  FOREIGN KEY (definition_id, workspace_id) REFERENCES harness_definitions(id, workspace_id) ON DELETE CASCADE,
  CHECK ((document ->> 'id') IS NOT NULL AND (document ->> 'id')::uuid = id),
  CHECK ((document ->> 'definitionId') IS NOT NULL AND (document ->> 'definitionId')::uuid = definition_id),
  CHECK (document ->> 'version' IS NOT NULL AND document ->> 'version' = version),
  CHECK (document ->> 'digest' IS NOT NULL AND document ->> 'digest' = digest)
);

CREATE INDEX harness_versions_workspace_idx ON harness_versions(workspace_id, definition_id, created_at);

CREATE TABLE harness_profiles (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9._-]{0,95}$'),
  harness_version_id uuid NOT NULL,
  status text NOT NULL CHECK (status IN ('approved', 'disabled')),
  resource_version bigint NOT NULL CHECK (resource_version > 0),
  digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  document jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, name),
  UNIQUE (id, workspace_id),
  FOREIGN KEY (harness_version_id, workspace_id) REFERENCES harness_versions(id, workspace_id),
  CHECK ((document ->> 'id') IS NOT NULL AND (document ->> 'id')::uuid = id),
  CHECK ((document ->> 'workspaceId') IS NOT NULL AND (document ->> 'workspaceId')::uuid = workspace_id),
  CHECK (document ->> 'name' IS NOT NULL AND document ->> 'name' = name),
  CHECK ((document ->> 'harnessVersionId') IS NOT NULL AND (document ->> 'harnessVersionId')::uuid = harness_version_id),
  CHECK (document ->> 'status' IS NOT NULL AND document ->> 'status' = status),
  CHECK ((document ->> 'resourceVersion') IS NOT NULL AND (document ->> 'resourceVersion')::bigint = resource_version),
  CHECK (document ->> 'digest' IS NOT NULL AND document ->> 'digest' = digest)
);

CREATE TABLE harness_profile_revisions (
  id uuid PRIMARY KEY,
  profile_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  resource_version bigint NOT NULL CHECK (resource_version > 0),
  digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  document jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (profile_id, resource_version),
  FOREIGN KEY (profile_id, workspace_id) REFERENCES harness_profiles(id, workspace_id) ON DELETE CASCADE,
  CHECK ((document ->> 'resourceVersion') IS NOT NULL AND (document ->> 'resourceVersion')::bigint = resource_version),
  CHECK (document ->> 'digest' IS NOT NULL AND document ->> 'digest' = digest)
);

CREATE TABLE agents (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  owner_id uuid NOT NULL REFERENCES users(id),
  name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9._-]{0,95}$'),
  tags jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(tags) = 'array'),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'archived')),
  current_version_id uuid,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, name),
  UNIQUE (id, workspace_id)
);

CREATE INDEX agents_workspace_status_idx ON agents(workspace_id, status, id);

CREATE TABLE agent_versions (
  id uuid PRIMARY KEY,
  agent_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  document jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (agent_id, version),
  UNIQUE (agent_id, digest),
  UNIQUE (id, workspace_id),
  FOREIGN KEY (agent_id, workspace_id) REFERENCES agents(id, workspace_id) ON DELETE CASCADE,
  CHECK ((document ->> 'id') IS NOT NULL AND (document ->> 'id')::uuid = id),
  CHECK ((document ->> 'agentId') IS NOT NULL AND (document ->> 'agentId')::uuid = agent_id),
  CHECK ((document ->> 'workspaceId') IS NOT NULL AND (document ->> 'workspaceId')::uuid = workspace_id),
  CHECK ((document ->> 'version') IS NOT NULL AND (document ->> 'version')::bigint = version),
  CHECK (document ->> 'digest' IS NOT NULL AND document ->> 'digest' = digest)
);

ALTER TABLE agent_versions ADD CONSTRAINT agent_versions_id_agent_unique UNIQUE (id, agent_id);

ALTER TABLE agents
  ADD CONSTRAINT agents_current_version_fk
  FOREIGN KEY (current_version_id, id) REFERENCES agent_versions(id, agent_id) DEFERRABLE INITIALLY DEFERRED;

GRANT SELECT, INSERT ON TABLE harness_definitions TO blazn_runtime;
GRANT UPDATE (status, resource_version, document, updated_at) ON TABLE harness_definitions TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE harness_versions TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE harness_profiles TO blazn_runtime;
GRANT UPDATE (name, harness_version_id, status, resource_version, digest, document, updated_at) ON TABLE harness_profiles TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE harness_profile_revisions TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE agents TO blazn_runtime;
GRANT UPDATE (name, tags, status, current_version_id, version, updated_at) ON TABLE agents TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE agent_versions TO blazn_runtime;

REVOKE DELETE ON TABLE harness_definitions, harness_versions, harness_profiles, harness_profile_revisions, agents, agent_versions FROM blazn_runtime;
REVOKE ALL ON TABLE harness_definitions, harness_versions, harness_profiles, harness_profile_revisions, agents, agent_versions FROM blazn_bootstrap;
