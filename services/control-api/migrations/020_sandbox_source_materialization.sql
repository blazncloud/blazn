-- Fence credential-free source materialization receipts to the exact active
-- create lease and persisted Sandbox -> Pod -> Workload observation.

CREATE FUNCTION sandbox_source_digest_field(p_value text)
RETURNS bytea
LANGUAGE sql IMMUTABLE STRICT
SET search_path = pg_catalog, public
RETURN pg_catalog.int8send(octet_length(convert_to(p_value,'UTF8'))::bigint)||convert_to(p_value,'UTF8');

CREATE FUNCTION sandbox_source_manifest_digest(
  p_names text[], p_urls text[], p_destinations text[], p_commits text[], p_writable boolean[])
RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT
SET search_path = pg_catalog, public
AS $$
DECLARE payload bytea := public.sandbox_source_digest_field('blazn.dev/sandbox-source-manifest/v1'); i integer;
BEGIN
  IF cardinality(p_names)<>cardinality(p_urls) OR cardinality(p_names)<>cardinality(p_destinations) OR
     cardinality(p_names)<>cardinality(p_commits) OR cardinality(p_names)<>cardinality(p_writable) THEN
    RETURN NULL;
  END IF;
  FOR i IN 1..coalesce(cardinality(p_names),0) LOOP
    payload := payload||public.sandbox_source_digest_field(p_names[i])||
      public.sandbox_source_digest_field(p_urls[i])||public.sandbox_source_digest_field(p_destinations[i])||
      public.sandbox_source_digest_field(p_commits[i])||public.sandbox_source_digest_field(p_writable[i]::text);
  END LOOP;
  RETURN encode(public.digest(payload,'sha256'),'hex');
END
$$;

CREATE FUNCTION sandbox_source_receipt_digest(p_manifest_digest text, p_sources jsonb)
RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT
SET search_path = pg_catalog, public
AS $$
DECLARE payload bytea := public.sandbox_source_digest_field('blazn.dev/sandbox-source-materialization/v1')||
  public.sandbox_source_digest_field('sha256:'||p_manifest_digest); item jsonb;
BEGIN
  IF jsonb_typeof(p_sources)<>'array' THEN RETURN NULL; END IF;
  FOR item IN SELECT value FROM jsonb_array_elements(p_sources) WITH ORDINALITY ordered(value,position) ORDER BY position LOOP
    payload := payload||public.sandbox_source_digest_field(item->>'name')||
      public.sandbox_source_digest_field(item->>'url')||public.sandbox_source_digest_field(item->>'destination')||
      public.sandbox_source_digest_field(item->>'commit')||public.sandbox_source_digest_field(item->>'tree')||
      public.sandbox_source_digest_field(item->>'contentDigest')||public.sandbox_source_digest_field(item->>'fileCount')||
      public.sandbox_source_digest_field(item->>'totalBytes')||public.sandbox_source_digest_field(item->>'writable');
  END LOOP;
  RETURN encode(public.digest(payload,'sha256'),'hex');
EXCEPTION WHEN null_value_not_allowed THEN RETURN NULL;
END
$$;

-- Required to make the composite receipt foreign key prove the exact bound
-- backend tuple rather than merely the Sandbox primary key.
ALTER TABLE sandbox_workload_admissions
  ADD CONSTRAINT sandbox_workload_admission_operation_backend_unique
  UNIQUE (operation_id,workspace_id,sandbox_id,backend_uid,backend_resource_version);

CREATE TABLE sandbox_source_materialization_receipts (
  sandbox_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  operation_id uuid NOT NULL UNIQUE,
  backend_uid text NOT NULL,
  backend_resource_version text NOT NULL,
  observation_digest char(64) NOT NULL CHECK (observation_digest ~ '^[0-9a-f]{64}$'),
  manifest_digest char(64) NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
  receipt_digest char(64) NOT NULL CHECK (receipt_digest ~ '^[0-9a-f]{64}$'),
  receipt jsonb NOT NULL CHECK (jsonb_typeof(receipt)='object' AND NOT workspace_json_contains_secret_key(receipt)),
  recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (sandbox_id,receipt_digest),
  FOREIGN KEY (sandbox_id,workspace_id) REFERENCES sandboxes(id,workspace_id) ON DELETE RESTRICT,
  FOREIGN KEY (operation_id,workspace_id,sandbox_id,backend_uid,backend_resource_version)
    REFERENCES sandbox_workload_admissions(operation_id,workspace_id,sandbox_id,backend_uid,backend_resource_version) ON DELETE RESTRICT
);

CREATE FUNCTION sandbox_source_receipts_immutable()
RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$ BEGIN RAISE EXCEPTION 'sandbox source materialization receipts are immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER sandbox_source_receipts_immutable
BEFORE UPDATE OR DELETE ON sandbox_source_materialization_receipts
FOR EACH ROW EXECUTE FUNCTION sandbox_source_receipts_immutable();

CREATE FUNCTION sandbox_controller_record_source_materialization_v1(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_expected_backend_uid text, p_expected_backend_resource_version text,
  p_expected_observation_digest text, p_manifest_digest text,
  p_receipt_digest text, p_receipt jsonb)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; expected_manifest text; item record; previous_name text := NULL;
BEGIN
  SELECT o.workspace_id,o.sandbox_id,o.type,
    a.backend_uid,a.backend_resource_version,a.observation_digest::text
  INTO target
  FROM public.sandbox_operations o
  JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  JOIN public.sandbox_workload_admissions a
    ON a.sandbox_id=o.sandbox_id AND a.workspace_id=o.workspace_id AND a.operation_id=o.id
  WHERE o.id=p_operation_id AND o.status='running' AND o.type='create' AND j.completed_at IS NULL
    AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j,a;
  IF NOT FOUND OR target.backend_uid<>p_expected_backend_uid OR
     target.backend_resource_version<>p_expected_backend_resource_version OR
     target.observation_digest IS NULL OR target.observation_digest<>p_expected_observation_digest OR
     p_manifest_digest !~ '^[0-9a-f]{64}$' OR p_receipt_digest !~ '^[0-9a-f]{64}$' THEN RETURN false; END IF;

  SELECT public.sandbox_source_manifest_digest(
    coalesce(array_agg(r.name ORDER BY r.name),'{}'::text[]),
    coalesce(array_agg(r.url ORDER BY r.name),'{}'::text[]),
    coalesce(array_agg(r.destination ORDER BY r.name),'{}'::text[]),
    coalesce(array_agg(s.commit ORDER BY r.name),'{}'::text[]),
    coalesce(array_agg(r.writable ORDER BY r.name),'{}'::boolean[]))
  INTO expected_manifest
  FROM public.sandbox_sources s JOIN public.sandbox_template_version_repositories r
    ON r.version_id=s.template_version_id AND r.workspace_id=s.workspace_id AND r.name=s.repository_name
  WHERE s.sandbox_id=target.sandbox_id AND s.workspace_id=target.workspace_id;
  IF expected_manifest IS NULL OR expected_manifest<>p_manifest_digest OR
     jsonb_typeof(p_receipt)<>'object' OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_receipt) key)<>
       ARRAY['digest','manifestDigest','schemaVersion','sources']::text[] OR
     p_receipt->>'schemaVersion'<>'blazn.dev/sandbox-source-materialization/v1' OR
     p_receipt->>'manifestDigest'<>'sha256:'||p_manifest_digest OR
     p_receipt->>'digest'<>'sha256:'||p_receipt_digest OR jsonb_typeof(p_receipt->'sources')<>'array' OR
     jsonb_array_length(p_receipt->'sources')<>(SELECT count(*) FROM public.sandbox_sources s WHERE s.sandbox_id=target.sandbox_id) OR
     public.sandbox_source_receipt_digest(p_manifest_digest,p_receipt->'sources')<>p_receipt_digest THEN RETURN false; END IF;

  FOR item IN
    SELECT value,position FROM jsonb_array_elements(p_receipt->'sources') WITH ORDINALITY listed(value,position) ORDER BY position
  LOOP
    IF jsonb_typeof(item.value)<>'object' OR
       (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(item.value) key)<>
         ARRAY['commit','contentDigest','destination','fileCount','name','totalBytes','tree','url','writable']::text[] OR
       item.value->>'name' !~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' OR
       previous_name IS NOT NULL AND previous_name>=item.value->>'name' OR
       item.value->>'tree' !~ '^[0-9a-f]{40}([0-9a-f]{24})?$' OR
       item.value->>'contentDigest' !~ '^sha256:[0-9a-f]{64}$' OR
       jsonb_typeof(item.value->'fileCount')<>'number' OR item.value->>'fileCount' !~ '^(0|[1-9][0-9]{0,5})$' OR
       (item.value->>'fileCount')::integer>100000 OR
       jsonb_typeof(item.value->'totalBytes')<>'number' OR item.value->>'totalBytes' !~ '^(0|[1-9][0-9]{0,8})$' OR
       (item.value->>'totalBytes')::bigint>268435456 OR jsonb_typeof(item.value->'writable')<>'boolean' OR
       NOT EXISTS(
         SELECT 1 FROM public.sandbox_sources s JOIN public.sandbox_template_version_repositories r
           ON r.version_id=s.template_version_id AND r.workspace_id=s.workspace_id AND r.name=s.repository_name
         WHERE s.sandbox_id=target.sandbox_id AND s.workspace_id=target.workspace_id AND
           r.name=item.value->>'name' AND r.url=item.value->>'url' AND r.destination=item.value->>'destination' AND
           s.commit=item.value->>'commit' AND r.writable=(item.value->>'writable')::boolean)
    THEN RETURN false; END IF;
    previous_name := item.value->>'name';
  END LOOP;

  INSERT INTO public.sandbox_source_materialization_receipts(
    sandbox_id,workspace_id,operation_id,backend_uid,backend_resource_version,
    observation_digest,manifest_digest,receipt_digest,receipt)
  VALUES(target.sandbox_id,target.workspace_id,p_operation_id,p_expected_backend_uid,
    p_expected_backend_resource_version,p_expected_observation_digest,p_manifest_digest,p_receipt_digest,p_receipt)
  ON CONFLICT (sandbox_id) DO NOTHING;
  RETURN EXISTS(SELECT 1 FROM public.sandbox_source_materialization_receipts r
    WHERE r.sandbox_id=target.sandbox_id AND r.workspace_id=target.workspace_id AND r.operation_id=p_operation_id AND
      r.backend_uid=p_expected_backend_uid AND r.backend_resource_version=p_expected_backend_resource_version AND
      r.observation_digest=p_expected_observation_digest AND r.manifest_digest=p_manifest_digest AND
      r.receipt_digest=p_receipt_digest AND r.receipt=p_receipt);
END
$$;

CREATE FUNCTION sandbox_controller_claim_v4(p_worker_id text, p_lease_seconds integer)
RETURNS TABLE(
  operation_id uuid, workspace_id uuid, sandbox_id uuid, requested_by uuid,
  operation_type text, expected_sandbox_version bigint, lease_token uuid, lease_expires_at timestamptz,
  attempt integer, allocation_mode text, desired_state text, architecture text, template_version_id uuid,
  template_digest text, variant_name text, image_index_digest text, image_child_digest text,
  placement_profile text, command text[], request_cpu text, request_memory text, request_ephemeral_storage text,
  limit_cpu text, limit_memory text, limit_ephemeral_storage text, queue_name text, admission_id text,
  backend_uid text, backend_resource_version text, expires_at timestamptz,
  source_names text[], source_urls text[], source_destinations text[], source_writable boolean[], source_commits text[],
  artifact_names text[], artifact_paths text[], artifact_media_types text[], artifact_required boolean[],
  admission_digest text, workload_api_version text, workload_namespace text, workload_name text,
  workload_uid text, workload_resource_version text, admitted_cluster_queue text,
  owner_api_version text, owner_kind text, owner_name text, owner_uid text, owner_controller boolean,
  workspace_label text, sandbox_label text, admitted boolean, condition_type text, condition_status text,
  pod_api_version text, pod_kind text, pod_namespace text, pod_name text, pod_uid text,
  pod_resource_version text, observation_digest text, source_materialization_receipt jsonb)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
  SELECT claimed.*,receipt.receipt
  FROM public.sandbox_controller_claim_v3(p_worker_id,p_lease_seconds) claimed
  LEFT JOIN public.sandbox_source_materialization_receipts receipt
    ON receipt.sandbox_id=claimed.sandbox_id AND receipt.workspace_id=claimed.workspace_id AND
       receipt.operation_id=claimed.operation_id AND receipt.backend_uid=claimed.backend_uid AND
       receipt.backend_resource_version=claimed.backend_resource_version AND
       receipt.observation_digest=claimed.observation_digest
$$;

CREATE FUNCTION sandbox_controller_complete_v4(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid, p_status text,
  p_expected_backend_uid text, p_expected_backend_resource_version text,
  p_expected_workload_digest text, p_expected_observation_digest text,
  p_cleanup_complete boolean, p_artifact_export_complete boolean, p_grants_revoked boolean,
  p_backend_destroyed boolean, p_artifact_ids uuid[], p_warning_codes text[],
  p_error_code text, p_safe_message text, p_request_id uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record;
BEGIN
  SELECT o.type,o.sandbox_id,
    (SELECT count(*) FROM public.sandbox_sources s WHERE s.sandbox_id=o.sandbox_id) source_count,
    EXISTS(SELECT 1 FROM public.sandbox_source_materialization_receipts r
      WHERE r.sandbox_id=o.sandbox_id AND r.operation_id=o.id AND r.observation_digest=p_expected_observation_digest) source_receipt
  INTO target
  FROM public.sandbox_operations o JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  WHERE o.id=p_operation_id AND o.status='running' AND j.completed_at IS NULL
    AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j;
  IF NOT FOUND OR (p_status='succeeded' AND target.type='create' AND target.source_count>0 AND NOT target.source_receipt) THEN RETURN false; END IF;
  RETURN public.sandbox_controller_complete_v3(
    p_operation_id,p_worker_id,p_lease_token,p_status,p_expected_backend_uid,
    p_expected_backend_resource_version,p_expected_workload_digest,p_expected_observation_digest,
    p_cleanup_complete,p_artifact_export_complete,p_grants_revoked,p_backend_destroyed,
    p_artifact_ids,p_warning_codes,p_error_code,p_safe_message,p_request_id);
END
$$;

REVOKE ALL ON TABLE sandbox_source_materialization_receipts
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION
  sandbox_source_digest_field(text),sandbox_source_manifest_digest(text[],text[],text[],text[],boolean[]),
  sandbox_source_receipt_digest(text,jsonb),sandbox_source_receipts_immutable(),
  sandbox_controller_record_source_materialization_v1(uuid,text,uuid,text,text,text,text,text,jsonb),
  sandbox_controller_claim_v4(text,integer),
  sandbox_controller_complete_v4(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION
  sandbox_controller_claim_v3(text,integer),
  sandbox_controller_complete_v3(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  FROM blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION
  sandbox_controller_record_source_materialization_v1(uuid,text,uuid,text,text,text,text,text,jsonb),
  sandbox_controller_claim_v4(text,integer),
  sandbox_controller_complete_v4(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  TO blazn_sandbox_controller;
