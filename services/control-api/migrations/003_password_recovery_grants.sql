GRANT UPDATE (password_salt, password_hash) ON TABLE users TO blazn_bootstrap;
GRANT REFERENCES ON TABLE sessions, device_authorizations TO blazn_bootstrap;
GRANT SELECT (user_id, revoked_at), UPDATE (revoked_at) ON TABLE sessions TO blazn_bootstrap;
GRANT SELECT (expires_at, consumed_at), UPDATE (expires_at, consumed_at) ON TABLE device_authorizations TO blazn_bootstrap;
