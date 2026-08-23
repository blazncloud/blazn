-- Social identities are verified by the isolated OIDC provider. Password
-- columns remain populated for legacy provisioned users and are null for
-- provider-only accounts.
ALTER TABLE users ALTER COLUMN password_salt DROP NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at timestamptz;

ALTER TABLE device_authorizations
  ADD COLUMN IF NOT EXISTS approved_identity_provider text CHECK (approved_identity_provider IS NULL OR approved_identity_provider = 'zitadel'),
  ADD COLUMN IF NOT EXISTS approved_identity_release text CHECK (approved_identity_release IS NULL OR approved_identity_release ~ '^v?[0-9]+\.[0-9]+\.[0-9]+$'),
  ADD COLUMN IF NOT EXISTS approved_identity_policy_digest text CHECK (approved_identity_policy_digest IS NULL OR approved_identity_policy_digest ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN IF NOT EXISTS approved_identity_acr text,
  ADD COLUMN IF NOT EXISTS approved_identity_amr text[],
  ADD CONSTRAINT device_authorization_assurance_complete CHECK (
    (approved_identity_provider IS NULL AND approved_identity_release IS NULL AND approved_identity_policy_digest IS NULL AND approved_identity_acr IS NULL AND approved_identity_amr IS NULL) OR
    (approved_identity_provider IS NOT NULL AND approved_identity_release IS NOT NULL AND approved_identity_policy_digest IS NOT NULL AND approved_identity_acr IS NOT NULL AND approved_identity_amr IS NOT NULL AND cardinality(approved_identity_amr) >= 2));
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
GRANT UPDATE (approved_identity_provider, approved_identity_release, approved_identity_policy_digest, approved_identity_acr, approved_identity_amr) ON TABLE device_authorizations TO blazn_runtime;
REVOKE UPDATE, DELETE ON TABLE users FROM blazn_runtime;
