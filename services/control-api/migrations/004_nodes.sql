CREATE TABLE nodes (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
  kind text NOT NULL CHECK (kind IN ('personal', 'shared', 'managed')),
  owner_user_id uuid NOT NULL REFERENCES users(id),
  machine_fingerprint char(64) NOT NULL CHECK (machine_fingerprint ~ '^[0-9a-f]{64}$'),
  host_platform text NOT NULL CHECK (host_platform IN ('linux', 'macos')),
  host_architecture text NOT NULL CHECK (host_architecture IN ('amd64', 'arm64')),
  lifecycle_state text NOT NULL CHECK (lifecycle_state IN ('pending', 'installing', 'verifying', 'active', 'paused', 'draining', 'offline', 'quarantined', 'removed')),
  trust_state text NOT NULL CHECK (trust_state IN ('unverified', 'verifying', 'verified', 'rotating', 'revoked')),
  agent_eligible boolean NOT NULL DEFAULT false,
  service_version text NOT NULL,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  last_heartbeat_at timestamptz,
  offline_after timestamptz,
  current_identity_generation bigint,
  current_identity_status text CHECK (current_identity_status IS NULL OR current_identity_status = 'active'),
  current_capability_version bigint,
  kubernetes_cluster_id text CHECK (kubernetes_cluster_id IS NULL OR char_length(kubernetes_cluster_id) BETWEEN 1 AND 128),
  kubernetes_node_name text CHECK (kubernetes_node_name IS NULL OR char_length(kubernetes_node_name) BETWEEN 1 AND 253),
  kubernetes_node_uid text CHECK (kubernetes_node_uid IS NULL OR char_length(kubernetes_node_uid) BETWEEN 1 AND 128),
  kubernetes_resource_version text CHECK (kubernetes_resource_version IS NULL OR char_length(kubernetes_resource_version) BETWEEN 1 AND 128),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, workspace_id),
  UNIQUE (id, workspace_id, kubernetes_node_uid),
  CHECK ((kubernetes_cluster_id IS NOT NULL) = (kubernetes_node_name IS NOT NULL)
    AND (kubernetes_cluster_id IS NOT NULL) = (kubernetes_node_uid IS NOT NULL)
    AND (kubernetes_cluster_id IS NOT NULL) = (kubernetes_resource_version IS NOT NULL)),
  CHECK ((current_identity_generation IS NOT NULL) = (current_identity_status IS NOT NULL)),
  CHECK (NOT agent_eligible OR (lifecycle_state = 'active' AND trust_state = 'verified' AND current_identity_status = 'active'
    AND kubernetes_node_uid IS NOT NULL AND current_identity_generation IS NOT NULL
    AND current_capability_version IS NOT NULL))
);

CREATE UNIQUE INDEX nodes_active_machine_idx ON nodes(workspace_id, machine_fingerprint) WHERE lifecycle_state <> 'removed';
CREATE UNIQUE INDEX nodes_kubernetes_binding_idx ON nodes(kubernetes_cluster_id, kubernetes_node_uid) WHERE kubernetes_node_uid IS NOT NULL;

CREATE TABLE node_enrollments (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  requested_name text NOT NULL CHECK (char_length(requested_name) BETWEEN 1 AND 128),
  mode text NOT NULL CHECK (mode IN ('fresh', 'adopt')),
  expected_platform text NOT NULL CHECK (expected_platform IN ('linux', 'macos')),
  expected_architecture text CHECK (expected_architecture IN ('amd64', 'arm64')),
  token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
  token_key_id text NOT NULL CHECK (token_key_id = 'node-enrollment/v1'),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  created_by uuid NOT NULL REFERENCES users(id),
  expires_at timestamptz NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'exchanged', 'consumed', 'expired', 'revoked')),
  machine_binding char(64) CHECK (machine_binding ~ '^[0-9a-f]{64}$'),
  node_public_key text CHECK (node_public_key ~ '^[A-Za-z0-9_-]{43}$'),
  node_public_key_fingerprint char(64) CHECK (node_public_key_fingerprint ~ '^[0-9a-f]{64}$'),
  consumed_by_node_id uuid,
  exchanged_at timestamptz,
  consumed_at timestamptz,
  revoked_at timestamptz,
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (expires_at > created_at),
  UNIQUE (id, workspace_id),
  UNIQUE (created_by, idempotency_key),
  CHECK ((status = 'consumed') = (consumed_by_node_id IS NOT NULL)),
  CHECK ((machine_binding IS NOT NULL) = (node_public_key IS NOT NULL)
    AND (machine_binding IS NOT NULL) = (node_public_key_fingerprint IS NOT NULL)),
  CHECK (status NOT IN ('exchanged', 'consumed') OR machine_binding IS NOT NULL),
  CHECK (status <> 'pending' OR machine_binding IS NULL),
  CHECK (status NOT IN ('exchanged', 'consumed') OR exchanged_at IS NOT NULL),
  CHECK (status <> 'pending' OR exchanged_at IS NULL),
  CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
  FOREIGN KEY (consumed_by_node_id, workspace_id) REFERENCES nodes(id, workspace_id)
);

CREATE TABLE node_identities (
  id uuid PRIMARY KEY,
  node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  public_key_fingerprint char(64) NOT NULL CHECK (public_key_fingerprint ~ '^[0-9a-f]{64}$'),
  public_key text NOT NULL CHECK (public_key ~ '^[A-Za-z0-9_-]{43}$'),
  signing_key_id text NOT NULL,
  generation bigint NOT NULL CHECK (generation > 0),
  status text NOT NULL CHECK (status IN ('active', 'rotating', 'revoked', 'expired')),
  issued_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  rotated_at timestamptz,
  revoked_at timestamptz,
  UNIQUE (node_id, generation),
  UNIQUE (node_id, generation, status),
  UNIQUE (node_id, generation, public_key_fingerprint, signing_key_id),
  CHECK (expires_at > issued_at),
  CHECK (rotated_at IS NULL OR rotated_at >= issued_at),
  CHECK (revoked_at IS NULL OR revoked_at >= issued_at)
);

CREATE UNIQUE INDEX node_identities_one_active_idx ON node_identities(node_id) WHERE status = 'active';

ALTER TABLE nodes ADD CONSTRAINT nodes_current_identity_fk
  FOREIGN KEY (id, current_identity_generation, current_identity_status)
  REFERENCES node_identities(node_id, generation, status) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE node_capability_versions (
  id uuid PRIMARY KEY,
  node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  version bigint NOT NULL CHECK (version > 0),
  digest char(64) NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
  payload jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(payload)),
  observed_at timestamptz NOT NULL,
  UNIQUE (node_id, version),
  UNIQUE (node_id, digest)
);

ALTER TABLE nodes ADD CONSTRAINT nodes_current_capability_fk
  FOREIGN KEY (id, current_capability_version) REFERENCES node_capability_versions(node_id, version) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE node_heartbeat_state (
  node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  identity_generation bigint NOT NULL CHECK (identity_generation > 0),
  boot_id text NOT NULL CHECK (char_length(boot_id) BETWEEN 1 AND 128),
  sequence bigint NOT NULL CHECK (sequence >= 0),
  sent_at timestamptz NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  capability_digest char(64) CHECK (capability_digest ~ '^[0-9a-f]{64}$'),
  health jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(health))
);

CREATE TABLE node_install_plans (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  node_id uuid NOT NULL,
  enrollment_id uuid NOT NULL UNIQUE,
  approved_by uuid NOT NULL REFERENCES users(id),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  plan_digest char(64) NOT NULL UNIQUE CHECK (plan_digest ~ '^[0-9a-f]{64}$'),
  signing_key_id text NOT NULL,
  signature text NOT NULL CHECK (signature ~ '^[A-Za-z0-9_-]{86}$'),
  canonical_plan jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(canonical_plan)),
  issued_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  status text NOT NULL CHECK (status IN ('issued', 'accepted', 'expired', 'revoked')),
  accepted_at timestamptz,
  revoked_at timestamptz,
  UNIQUE (id, workspace_id),
  UNIQUE (id, workspace_id, node_id),
  UNIQUE (id, workspace_id, enrollment_id, node_id),
  UNIQUE (approved_by, idempotency_key),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (enrollment_id, workspace_id) REFERENCES node_enrollments(id, workspace_id),
  CHECK (expires_at > issued_at),
  CHECK (status <> 'accepted' OR accepted_at IS NOT NULL),
  CHECK (status <> 'issued' OR accepted_at IS NULL),
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);

CREATE TABLE node_install_receipts (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  node_id uuid NOT NULL,
  plan_id uuid NOT NULL,
  receipt_digest char(64) NOT NULL UNIQUE CHECK (receipt_digest ~ '^[0-9a-f]{64}$'),
  signer_kind text NOT NULL CHECK (signer_kind = 'node_identity'),
  identity_generation bigint NOT NULL CHECK (identity_generation > 0),
  signer_fingerprint char(64) NOT NULL CHECK (signer_fingerprint ~ '^[0-9a-f]{64}$'),
  signing_key_id text NOT NULL,
  signature text NOT NULL CHECK (signature ~ '^[A-Za-z0-9_-]{86}$'),
  payload jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(payload)),
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (plan_id, workspace_id, node_id) REFERENCES node_install_plans(id, workspace_id, node_id),
  FOREIGN KEY (node_id, identity_generation, signer_fingerprint, signing_key_id)
    REFERENCES node_identities(node_id, generation, public_key_fingerprint, signing_key_id)
);

CREATE TABLE node_operation_receipts (
  id uuid PRIMARY KEY,
  operation_id uuid NOT NULL UNIQUE,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  node_id uuid NOT NULL,
  operation_type text NOT NULL CHECK (operation_type IN ('pause', 'resume', 'label', 'cordon', 'uncordon', 'rotate_identity', 'repair', 'update', 'drain', 'remove')),
  receipt_digest char(64) NOT NULL UNIQUE CHECK (receipt_digest ~ '^[0-9a-f]{64}$'),
  signer_kind text NOT NULL CHECK (signer_kind IN ('node_identity', 'control_plane')),
  identity_generation bigint CHECK (identity_generation > 0),
  signer_fingerprint char(64) NOT NULL CHECK (signer_fingerprint ~ '^[0-9a-f]{64}$'),
  signing_key_id text NOT NULL,
  signature text NOT NULL CHECK (signature ~ '^[A-Za-z0-9_-]{86}$'),
  payload jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(payload)),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, operation_id, workspace_id, node_id, operation_type),
  CHECK ((signer_kind = 'node_identity') = (identity_generation IS NOT NULL)),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (node_id, identity_generation, signer_fingerprint, signing_key_id)
    REFERENCES node_identities(node_id, generation, public_key_fingerprint, signing_key_id)
);

CREATE TABLE node_operations (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  node_id uuid NOT NULL,
  type text NOT NULL CHECK (type IN ('pause', 'resume', 'label', 'cordon', 'uncordon', 'rotate_identity', 'repair', 'update', 'drain', 'remove')),
  status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled', 'partial', 'recovery_required')),
  expected_node_version bigint NOT NULL CHECK (expected_node_version > 0),
  requested_by uuid NOT NULL REFERENCES users(id),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  parameters jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (NOT workspace_json_contains_secret_key(parameters)),
  result jsonb CHECK (result IS NULL OR NOT workspace_json_contains_secret_key(result)),
  error jsonb CHECK (error IS NULL OR NOT workspace_json_contains_secret_key(error)),
  receipt_id uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  completed_at timestamptz,
  UNIQUE (requested_by, type, idempotency_key),
  UNIQUE (id, workspace_id, node_id, type),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (receipt_id, id, workspace_id, node_id, type)
    REFERENCES node_operation_receipts(id, operation_id, workspace_id, node_id, operation_type)
    DEFERRABLE INITIALLY DEFERRED,
  CHECK ((status IN ('succeeded', 'failed', 'cancelled', 'partial', 'recovery_required')) = (completed_at IS NOT NULL)),
  CHECK ((status IN ('succeeded', 'failed', 'cancelled', 'partial', 'recovery_required')) = (receipt_id IS NOT NULL))
);

ALTER TABLE node_operation_receipts ADD CONSTRAINT node_operation_receipt_operation_fk
  FOREIGN KEY (operation_id, workspace_id, node_id, operation_type)
  REFERENCES node_operations(id, workspace_id, node_id, type)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE node_operation_events (
  id uuid PRIMARY KEY,
  operation_id uuid NOT NULL REFERENCES node_operations(id) ON DELETE CASCADE,
  sequence bigint NOT NULL CHECK (sequence >= 0),
  type text NOT NULL,
  payload jsonb NOT NULL CHECK (NOT workspace_json_contains_secret_key(payload)),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (operation_id, sequence)
);

CREATE TABLE node_join_issuances (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  enrollment_id uuid NOT NULL UNIQUE,
  plan_id uuid NOT NULL UNIQUE,
  node_id uuid NOT NULL,
  node_public_key_fingerprint char(64) NOT NULL CHECK (node_public_key_fingerprint ~ '^[0-9a-f]{64}$'),
  machine_fingerprint char(64) NOT NULL CHECK (machine_fingerprint ~ '^[0-9a-f]{64}$'),
  credential_hash char(64) NOT NULL UNIQUE CHECK (credential_hash ~ '^[0-9a-f]{64}$'),
  credential_ciphertext bytea NOT NULL CHECK (octet_length(credential_ciphertext) >= 29),
  credential_key_id text NOT NULL CHECK (credential_key_id = 'node-join-credential/v1'),
  idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 128),
  request_digest char(64) NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  issued_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  revoked_at timestamptz,
  joined_node_uid text,
  CHECK (expires_at > issued_at),
  CHECK (NOT (consumed_at IS NOT NULL AND revoked_at IS NOT NULL)),
  CHECK ((consumed_at IS NOT NULL) = (joined_node_uid IS NOT NULL)),
  UNIQUE (node_id, idempotency_key),
  FOREIGN KEY (enrollment_id, workspace_id) REFERENCES node_enrollments(id, workspace_id),
  FOREIGN KEY (plan_id, workspace_id, enrollment_id, node_id)
    REFERENCES node_install_plans(id, workspace_id, enrollment_id, node_id),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (node_id, workspace_id, joined_node_uid)
    REFERENCES nodes(id, workspace_id, kubernetes_node_uid)
);

CREATE TABLE node_audit_events (
  id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  node_id uuid,
  actor_user_id uuid REFERENCES users(id),
  event_type text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (NOT workspace_json_contains_secret_key(payload)),
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (node_id, workspace_id) REFERENCES nodes(id, workspace_id)
);

GRANT SELECT, INSERT, UPDATE ON TABLE nodes, node_enrollments, node_identities, node_capability_versions, node_heartbeat_state, node_install_plans, node_install_receipts, node_operation_receipts, node_operations, node_operation_events, node_audit_events TO blazn_runtime;
GRANT SELECT (id, workspace_id, enrollment_id, plan_id, node_id, node_public_key_fingerprint,
  machine_fingerprint, idempotency_key, request_digest, issued_at, expires_at,
  consumed_at, revoked_at, joined_node_uid) ON TABLE node_join_issuances TO blazn_runtime;
GRANT UPDATE (consumed_at, joined_node_uid) ON TABLE node_join_issuances TO blazn_runtime;
GRANT SELECT ON TABLE nodes, node_enrollments, node_install_plans TO blazn_node_broker;
GRANT SELECT, INSERT, UPDATE ON TABLE node_join_issuances TO blazn_node_broker;
REVOKE ALL ON TABLE nodes, node_enrollments, node_identities, node_capability_versions, node_heartbeat_state, node_install_plans, node_install_receipts, node_operation_receipts, node_operations, node_operation_events, node_join_issuances, node_audit_events FROM blazn_bootstrap;
