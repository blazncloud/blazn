DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM node_enrollments) THEN
    RAISE EXCEPTION '006_node_plan_signing_trust requires zero pre-contract node enrollments; revoke and recreate them so their signing trust can be pinned';
  END IF;
END
$$;

ALTER TABLE node_enrollments
  ADD COLUMN plan_signing_key_id text NOT NULL
    CHECK (char_length(plan_signing_key_id) BETWEEN 1 AND 128),
  ADD COLUMN plan_signing_public_key char(43) NOT NULL
    CHECK (plan_signing_public_key ~ '^[A-Za-z0-9_-]{43}$'),
  ADD COLUMN plan_signing_key_fingerprint char(64) NOT NULL
    CHECK (plan_signing_key_fingerprint ~ '^[0-9a-f]{64}$'),
  ADD CONSTRAINT node_enrollments_plan_signing_key_id_unique
    UNIQUE (id, plan_signing_key_id),
  ADD CONSTRAINT node_enrollments_plan_signing_key_tuple_unique
    UNIQUE (id, plan_signing_key_id, plan_signing_key_fingerprint);

ALTER TABLE node_install_plans
  ADD CONSTRAINT node_install_plans_pinned_signing_key_fk
    FOREIGN KEY (enrollment_id, signing_key_id)
    REFERENCES node_enrollments(id, plan_signing_key_id);

COMMENT ON COLUMN node_enrollments.plan_signing_key_id IS
  'Immutable plan-signing key selected when the enrollment is first created.';
COMMENT ON COLUMN node_enrollments.plan_signing_public_key IS
  'Immutable raw Ed25519 public key encoded as unpadded base64url.';
COMMENT ON COLUMN node_enrollments.plan_signing_key_fingerprint IS
  'Immutable lowercase SHA-256 of the raw plan-signing public key.';

REVOKE UPDATE ON TABLE node_enrollments FROM blazn_runtime;
GRANT UPDATE (
  status,
  machine_binding,
  node_public_key,
  node_public_key_fingerprint,
  consumed_by_node_id,
  exchanged_at,
  consumed_at,
  revoked_at,
  version
) ON TABLE node_enrollments TO blazn_runtime;
