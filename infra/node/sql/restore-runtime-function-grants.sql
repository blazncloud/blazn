-- Legacy control planes can reach role preparation before workspace migrations
-- have created this function. Restore the runtime grant when the function is
-- present and otherwise leave it to the owning migration.
DO $restore_runtime_function_grants$
BEGIN
  IF pg_catalog.to_regprocedure('public.workspace_json_contains_secret_key(jsonb)') IS NOT NULL THEN
    EXECUTE 'GRANT EXECUTE ON FUNCTION public.workspace_json_contains_secret_key(jsonb) TO blazn_runtime';
  END IF;
END
$restore_runtime_function_grants$;
