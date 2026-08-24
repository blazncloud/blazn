CREATE FUNCTION public.rotate_bootstrap_password(p_login text, p_password_salt text, p_password_hash text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
  identity_count bigint;
  target_user_id uuid;
BEGIN
  PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('blazn-initial-identity'));
  LOCK TABLE public.sessions, public.device_authorizations IN SHARE ROW EXCLUSIVE MODE;

  SELECT pg_catalog.count(*) INTO identity_count
  FROM public.users AS candidate
  WHERE candidate.email = p_login;
  IF identity_count <> 1 THEN
    RAISE EXCEPTION 'configured bootstrap identity must exist exactly once';
  END IF;

  SELECT candidate.id INTO target_user_id
  FROM public.users AS candidate
  WHERE candidate.email = p_login
  FOR UPDATE;

  UPDATE public.users
  SET password_salt = p_password_salt, password_hash = p_password_hash
  WHERE id = target_user_id;
  UPDATE public.sessions
  SET revoked_at = COALESCE(revoked_at, pg_catalog.now())
  WHERE user_id = target_user_id;
  UPDATE public.device_authorizations
  SET expires_at = LEAST(expires_at, pg_catalog.now()),
      consumed_at = COALESCE(consumed_at, pg_catalog.now())
  WHERE consumed_at IS NULL;
END;
$$;

ALTER FUNCTION public.rotate_bootstrap_password(text, text, text) OWNER TO blazn_migration;
REVOKE ALL ON FUNCTION public.rotate_bootstrap_password(text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.rotate_bootstrap_password(text, text, text) TO blazn_bootstrap;
