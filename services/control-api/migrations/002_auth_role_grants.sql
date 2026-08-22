-- The deployment creates these LOGIN roles before migrations. Keep the
-- internet-facing runtime and the one-shot bootstrap on explicit table grants;
-- every future migration must grant each new operation deliberately.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM blazn_runtime, blazn_bootstrap;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public
  REVOKE ALL ON TABLES FROM blazn_runtime, blazn_bootstrap;
ALTER DEFAULT PRIVILEGES FOR ROLE blazn_migration IN SCHEMA public
  REVOKE ALL ON SEQUENCES FROM blazn_runtime, blazn_bootstrap;

GRANT SELECT, INSERT ON TABLE users TO blazn_bootstrap;

REVOKE INSERT, UPDATE, DELETE ON TABLE users FROM blazn_runtime;
GRANT SELECT ON TABLE users TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE devices TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE device_authorizations TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE ON TABLE sessions TO blazn_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE auth_rate_limits TO blazn_runtime;
