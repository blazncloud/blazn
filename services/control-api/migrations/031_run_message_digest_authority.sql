CREATE FUNCTION run_message_digest_matches(input_content text, input_digest text) RETURNS boolean
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
  SELECT input_digest = 'sha256:' || encode(public.digest(convert_to(input_content, 'UTF8'), 'sha256'), 'hex');
$$;

REVOKE ALL ON FUNCTION run_message_digest_matches(text, text) FROM PUBLIC, blazn_bootstrap;
GRANT EXECUTE ON FUNCTION run_message_digest_matches(text, text) TO blazn_runtime;

ALTER TABLE run_messages DROP CONSTRAINT run_messages_check;
ALTER TABLE run_messages ADD CONSTRAINT run_messages_content_digest_matches
  CHECK (run_message_digest_matches(content, content_digest));
