-- Trigger functions cannot be invoked directly, but retaining the default
-- PUBLIC EXECUTE grant needlessly broadens their ACL and complicates effective
-- privilege verification for controller roles.
REVOKE EXECUTE ON FUNCTION sandbox_enforce_successful_create_admission() FROM PUBLIC;
