-- Social identities are verified by the isolated OIDC provider. Password
-- columns remain populated for legacy provisioned users and are null for
-- provider-only accounts.
ALTER TABLE users ALTER COLUMN password_salt DROP NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at timestamptz;

CREATE TABLE user_identities (
  issuer text NOT NULL CHECK (issuer ~ '^https://'),
  subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_login_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issuer, subject),
  UNIQUE (user_id, issuer)
);

REVOKE ALL ON TABLE user_identities FROM PUBLIC, blazn_runtime, blazn_bootstrap;
GRANT SELECT, INSERT, UPDATE ON TABLE user_identities TO blazn_runtime;
GRANT INSERT (id, email, display_name, email_verified_at) ON TABLE users TO blazn_runtime;
REVOKE UPDATE, DELETE ON TABLE users FROM blazn_runtime;
