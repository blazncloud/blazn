DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blazn_node_broker') THEN
    RAISE EXCEPTION 'required role blazn_node_broker is absent';
  END IF;
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO blazn_node_broker', current_database());
END
$$;

REVOKE CREATE ON SCHEMA public FROM blazn_node_broker;
