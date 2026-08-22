CREATE TABLE workspaces (
  id uuid PRIMARY KEY,
  slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$'),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 160),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspace_memberships (
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role text NOT NULL CHECK (role IN ('owner', 'administrator', 'operator', 'member', 'viewer')),
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'removed')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  invited_by uuid REFERENCES users(id),
  joined_at timestamptz NOT NULL DEFAULT now(),
  removed_at timestamptz,
  PRIMARY KEY (workspace_id, user_id),
  CHECK ((status = 'active' AND removed_at IS NULL) OR (status = 'removed' AND removed_at IS NOT NULL))
);

CREATE TABLE workspace_invitations (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
  token_key_id text NOT NULL CHECK (token_key_id = 'workspace-invitation-hmac/v1'),
  role text NOT NULL CHECK (role IN ('administrator', 'operator', 'member', 'viewer')),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'revoked', 'expired')),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  accepted_by uuid REFERENCES users(id),
  accepted_at timestamptz,
  CHECK (expires_at > created_at),
  CHECK ((status = 'accepted') = (accepted_by IS NOT NULL AND accepted_at IS NOT NULL)),
  CHECK (accepted_at IS NULL OR accepted_at <= expires_at)
);

CREATE FUNCTION workspace_json_contains_secret_key(input_value jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
  entry record;
  normalized_key text;
BEGIN
  IF jsonb_typeof(input_value) = 'object' THEN
    FOR entry IN SELECT pair.key, pair.value AS child FROM jsonb_each(input_value) AS pair LOOP
      normalized_key := regexp_replace(lower(entry.key), '[^a-z0-9]', '', 'g');
      IF normalized_key IN ('token', 'invitetoken', 'accesstoken', 'refreshtoken', 'authorization', 'password', 'secret', 'credential') THEN
        RETURN true;
      END IF;
      IF workspace_json_contains_secret_key(entry.child) THEN
        RETURN true;
      END IF;
    END LOOP;
  ELSIF jsonb_typeof(input_value) = 'array' THEN
    FOR entry IN SELECT item.value AS child FROM jsonb_array_elements(input_value) AS item LOOP
      IF workspace_json_contains_secret_key(entry.child) THEN
        RETURN true;
      END IF;
    END LOOP;
  END IF;
  RETURN false;
END;
$$;

CREATE TABLE workspace_idempotency_receipts (
  principal_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  operation text NOT NULL,
  target_key text NOT NULL CHECK (char_length(target_key) BETWEEN 1 AND 256),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  response_status integer NOT NULL CHECK (response_status BETWEEN 200 AND 599),
  response_body jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (principal_id, workspace_id, operation, idempotency_key),
  CHECK (NOT workspace_json_contains_secret_key(response_body))
);

CREATE TABLE workspace_audit_events (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  actor_user_id uuid REFERENCES users(id),
  event_type text NOT NULL CHECK (char_length(event_type) BETWEEN 1 AND 96),
  subject_user_id uuid REFERENCES users(id),
  invitation_id uuid REFERENCES workspace_invitations(id),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (NOT workspace_json_contains_secret_key(payload))
);

CREATE INDEX workspace_memberships_user_active_idx ON workspace_memberships(user_id, workspace_id) WHERE status = 'active';
CREATE INDEX workspace_invitations_workspace_status_idx ON workspace_invitations(workspace_id, status, created_at);
CREATE INDEX workspace_audit_events_workspace_created_idx ON workspace_audit_events(workspace_id, created_at, id);

GRANT SELECT, INSERT, UPDATE ON TABLE workspaces TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE workspace_memberships TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE workspace_invitations TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE workspace_idempotency_receipts TO blazn_runtime;
GRANT SELECT, INSERT ON TABLE workspace_audit_events TO blazn_runtime;
REVOKE ALL ON TABLE workspaces, workspace_memberships, workspace_invitations, workspace_idempotency_receipts, workspace_audit_events FROM blazn_bootstrap;
