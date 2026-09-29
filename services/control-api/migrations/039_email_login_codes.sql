-- Passwordless email sign-in and sign-up. A short numeric code is emailed to
-- the address entered on the activation page and is bound to exactly one
-- pending device authorization. Only a keyed digest of the code is stored.
CREATE TABLE email_login_codes (
  id uuid PRIMARY KEY,
  device_authorization_id uuid NOT NULL REFERENCES device_authorizations(id) ON DELETE CASCADE,
  email text NOT NULL CHECK (length(email) BETWEEN 3 AND 254 AND email = lower(email)),
  code_hash text NOT NULL CHECK (code_hash ~ '^[0-9a-f]{64}$'),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz,
  CHECK (expires_at > created_at)
);

CREATE INDEX email_login_codes_authorization_idx ON email_login_codes(device_authorization_id, email);
CREATE INDEX email_login_codes_expiry_idx ON email_login_codes(expires_at);

REVOKE ALL ON TABLE email_login_codes FROM PUBLIC, blazn_runtime, blazn_bootstrap;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE email_login_codes TO blazn_runtime;
