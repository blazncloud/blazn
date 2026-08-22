CREATE TABLE IF NOT EXISTS schema_migrations (
  version text PRIMARY KEY,
  checksum text NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY,
  email text NOT NULL UNIQUE,
  display_name text NOT NULL,
  password_salt text NOT NULL,
  password_hash text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS devices (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL,
  platform text NOT NULL,
  public_key text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
  ,UNIQUE (id, user_id)
);

CREATE TABLE IF NOT EXISTS device_authorizations (
  id uuid PRIMARY KEY,
  device_code_hash text NOT NULL UNIQUE,
  user_code text NOT NULL UNIQUE,
  device_name text NOT NULL,
  platform text NOT NULL,
  public_key text NOT NULL,
  challenge text NOT NULL,
  expires_at timestamptz NOT NULL,
  approved_user_id uuid REFERENCES users(id),
  consumed_at timestamptz,
  last_polled_at timestamptz,
  poll_count integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id uuid NOT NULL,
  token_hash text NOT NULL UNIQUE,
  refresh_token_hash text NOT NULL UNIQUE,
  refresh_version integer NOT NULL DEFAULT 1,
  access_expires_at timestamptz NOT NULL,
  refresh_expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
  ,FOREIGN KEY (device_id, user_id) REFERENCES devices(id, user_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS sessions_user_device_idx ON sessions(user_id, device_id);
CREATE INDEX IF NOT EXISTS device_authorizations_expiry_idx ON device_authorizations(expires_at);

CREATE TABLE IF NOT EXISTS auth_rate_limits (
  key text PRIMARY KEY,
  window_start timestamptz NOT NULL,
  count integer NOT NULL CHECK (count > 0)
);

-- The deployment creates these NOLOGIN/LOGIN roles before migrations. Keep the
-- internet-facing runtime and the one-shot bootstrap on explicit table grants;
-- future migrations must grant each new operation deliberately.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM blazn_runtime, blazn_bootstrap;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public
  REVOKE ALL ON TABLES FROM blazn_runtime, blazn_bootstrap;

GRANT SELECT, INSERT ON TABLE users TO blazn_bootstrap;

REVOKE INSERT, UPDATE, DELETE ON TABLE users FROM blazn_runtime;
GRANT SELECT ON TABLE users TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE devices TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE device_authorizations TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE sessions TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE auth_rate_limits TO blazn_runtime;
