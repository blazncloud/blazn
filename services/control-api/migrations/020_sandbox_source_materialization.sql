-- Fence credential-free source materialization receipts to the exact active
-- create lease and persisted Sandbox -> Pod -> Workload observation.

DO $$ BEGIN
  IF EXISTS(
    SELECT 1 FROM sandbox_template_version_repositories parent
    JOIN sandbox_template_version_repositories child ON child.version_id=parent.version_id AND child.name<>parent.name
    WHERE left(child.destination,char_length(parent.destination)+1)=parent.destination||'/') THEN
    RAISE EXCEPTION 'existing Sandbox repository destinations overlap' USING ERRCODE='23514';
  END IF;
END $$;

CREATE FUNCTION sandbox_reject_nested_repository_destinations()
RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target_version uuid := coalesce(NEW.version_id,OLD.version_id);
BEGIN
  IF EXISTS(
    SELECT 1 FROM public.sandbox_template_version_repositories parent
    JOIN public.sandbox_template_version_repositories child
      ON child.version_id=parent.version_id AND child.name<>parent.name
    WHERE parent.version_id=target_version AND
      left(child.destination,char_length(parent.destination)+1)=parent.destination||'/') THEN
    RAISE EXCEPTION 'Sandbox repository destinations overlap' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER sandbox_repository_destinations_nonoverlapping
AFTER INSERT OR UPDATE OR DELETE ON sandbox_template_version_repositories
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION sandbox_reject_nested_repository_destinations();

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

CREATE TABLE sandbox_source_materialization_receipts (
  sandbox_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  operation_id uuid NOT NULL UNIQUE,
  operation_type text NOT NULL DEFAULT 'create' CHECK (operation_type='create'),
  backend_uid text NOT NULL,
  backend_resource_version text NOT NULL,
  observation_digest char(64) NOT NULL CHECK (observation_digest ~ '^[0-9a-f]{64}$'),
  manifest_digest char(64) NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
  receipt_digest char(64) NOT NULL CHECK (receipt_digest ~ '^[0-9a-f]{64}$'),
  receipt jsonb NOT NULL CHECK (jsonb_typeof(receipt)='object' AND NOT workspace_json_contains_secret_key(receipt)),
  bootstrap_observation jsonb NOT NULL CHECK (jsonb_typeof(bootstrap_observation)='object' AND NOT workspace_json_contains_secret_key(bootstrap_observation)),
  recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (sandbox_id,receipt_digest),
  FOREIGN KEY (sandbox_id,workspace_id) REFERENCES sandboxes(id,workspace_id) ON DELETE RESTRICT,
  FOREIGN KEY (operation_id,workspace_id,sandbox_id,operation_type)
    REFERENCES sandbox_operations(id,workspace_id,sandbox_id,type) ON DELETE RESTRICT
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
  p_receipt_digest text, p_receipt jsonb, p_bootstrap_observation jsonb)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; expected_manifest text; item record; previous_name text := NULL;
BEGIN
  SELECT o.workspace_id,o.sandbox_id,o.type
  INTO target
  FROM public.sandbox_operations o
  JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
  JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
  WHERE o.id=p_operation_id AND o.status='running' AND o.type='create' AND j.completed_at IS NULL
    AND j.lease_owner=p_worker_id AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp()
  FOR UPDATE OF o,j,s;
  IF NOT FOUND OR p_expected_backend_uid IS NULL OR p_expected_backend_resource_version IS NULL OR
     p_expected_observation_digest IS NULL OR p_manifest_digest IS NULL OR p_receipt_digest IS NULL OR
     p_expected_backend_uid !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR
     p_expected_backend_resource_version !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR
     p_expected_observation_digest !~ '^[0-9a-f]{64}$' OR p_manifest_digest !~ '^[0-9a-f]{64}$' OR
     p_receipt_digest !~ '^[0-9a-f]{64}$' THEN RETURN false; END IF;

  IF jsonb_typeof(p_bootstrap_observation)<>'object' OR jsonb_strip_nulls(p_bootstrap_observation)<>p_bootstrap_observation OR
     jsonb_typeof(p_bootstrap_observation->'sandbox')<>'object' OR jsonb_typeof(p_bootstrap_observation->'pod')<>'object' OR
     jsonb_typeof(p_bootstrap_observation->'workload')<>'object' OR jsonb_typeof(p_bootstrap_observation#>'{workload,owner}')<>'object' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,condition}')<>'object' THEN RETURN false; END IF;
  IF
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_bootstrap_observation) key)<>
       ARRAY['digest','pod','sandbox','workload']::text[] OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_bootstrap_observation->'sandbox') key)<>
       ARRAY['apiVersion','kind','name','namespace','resourceVersion','uid']::text[] OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_bootstrap_observation->'pod') key)<>
       ARRAY['apiVersion','kind','name','namespace','resourceVersion','uid']::text[] OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_bootstrap_observation->'workload') key)<>
       ARRAY['admitted','apiVersion','clusterQueue','condition','digest','name','namespace','owner','resourceVersion','sandboxId','uid','workspaceId']::text[] OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_bootstrap_observation#>'{workload,owner}') key)<>
       ARRAY['apiVersion','controller','kind','name','uid']::text[] OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_bootstrap_observation#>'{workload,condition}') key)<>
       ARRAY['status','type']::text[] OR
     jsonb_typeof(p_bootstrap_observation#>'{sandbox,apiVersion}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{sandbox,kind}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{sandbox,namespace}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{sandbox,name}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{sandbox,uid}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{sandbox,resourceVersion}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{pod,apiVersion}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{pod,kind}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{pod,namespace}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{pod,name}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{pod,uid}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{pod,resourceVersion}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,apiVersion}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,namespace}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,name}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,uid}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,resourceVersion}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,clusterQueue}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,workspaceId}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,sandboxId}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,digest}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,admitted}')<>'boolean' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,owner,apiVersion}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,owner,kind}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,owner,name}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,owner,uid}')<>'string' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,owner,controller}')<>'boolean' OR
     jsonb_typeof(p_bootstrap_observation#>'{workload,condition,type}')<>'string' OR jsonb_typeof(p_bootstrap_observation#>'{workload,condition,status}')<>'string' OR
     p_bootstrap_observation#>>'{sandbox,apiVersion}'<>'agents.x-k8s.io/v1beta1' OR
     p_bootstrap_observation#>>'{sandbox,kind}'<>'Sandbox' OR
     p_bootstrap_observation#>>'{sandbox,namespace}'<>'blazn-poc-sandboxes' OR
     p_bootstrap_observation#>>'{sandbox,name}'<>target.sandbox_id::text OR
     p_bootstrap_observation#>>'{sandbox,uid}'<>p_expected_backend_uid OR
     p_bootstrap_observation#>>'{sandbox,resourceVersion}'<>p_expected_backend_resource_version OR
     p_bootstrap_observation#>>'{pod,apiVersion}'<>'v1' OR p_bootstrap_observation#>>'{pod,kind}'<>'Pod' OR
     p_bootstrap_observation#>>'{pod,namespace}'<>'blazn-poc-sandboxes' OR
     p_bootstrap_observation#>>'{workload,apiVersion}'<>'kueue.x-k8s.io/v1beta1' OR
     p_bootstrap_observation#>>'{workload,namespace}'<>'blazn-poc-sandboxes' OR
     p_bootstrap_observation#>>'{workload,owner,apiVersion}'<>'agents.x-k8s.io/v1beta1' OR
     p_bootstrap_observation#>>'{workload,owner,kind}'<>'Sandbox' OR
     p_bootstrap_observation#>>'{workload,owner,name}'<>target.sandbox_id::text OR
     p_bootstrap_observation#>>'{workload,owner,uid}'<>p_expected_backend_uid OR
     (p_bootstrap_observation#>>'{workload,owner,controller}')::boolean IS NOT TRUE OR
     p_bootstrap_observation#>>'{workload,workspaceId}'<>target.workspace_id::text OR
     p_bootstrap_observation#>>'{workload,sandboxId}'<>target.sandbox_id::text OR
     (p_bootstrap_observation#>>'{workload,admitted}')::boolean IS NOT TRUE OR
     p_bootstrap_observation#>>'{workload,condition,type}'<>'Admitted' OR
     p_bootstrap_observation#>>'{workload,condition,status}'<>'True' OR
     p_bootstrap_observation#>>'{workload,digest}' !~ '^sha256:[0-9a-f]{64}$' OR
     substring(p_bootstrap_observation#>>'{workload,digest}' from 8)<>public.sandbox_workload_admission_digest(
       p_bootstrap_observation#>>'{workload,apiVersion}',p_bootstrap_observation#>>'{workload,namespace}',
       p_bootstrap_observation#>>'{workload,name}',p_bootstrap_observation#>>'{workload,uid}',
       p_bootstrap_observation#>>'{workload,resourceVersion}',p_bootstrap_observation#>>'{workload,clusterQueue}',
       p_bootstrap_observation#>>'{workload,owner,apiVersion}',p_bootstrap_observation#>>'{workload,owner,kind}',
       p_bootstrap_observation#>>'{workload,owner,name}',p_bootstrap_observation#>>'{workload,owner,uid}',
       (p_bootstrap_observation#>>'{workload,owner,controller}')::boolean,
       p_bootstrap_observation#>>'{workload,workspaceId}',p_bootstrap_observation#>>'{workload,sandboxId}',
       (p_bootstrap_observation#>>'{workload,admitted}')::boolean,p_bootstrap_observation#>>'{workload,condition,type}',
       p_bootstrap_observation#>>'{workload,condition,status}') OR
     p_bootstrap_observation->>'digest'<>'sha256:'||p_expected_observation_digest OR
     p_expected_observation_digest<>public.sandbox_admission_observation_digest(
       p_expected_backend_uid,p_expected_backend_resource_version,p_bootstrap_observation#>>'{pod,apiVersion}',
       p_bootstrap_observation#>>'{pod,kind}',p_bootstrap_observation#>>'{pod,namespace}',
       p_bootstrap_observation#>>'{pod,name}',p_bootstrap_observation#>>'{pod,uid}',
       p_bootstrap_observation#>>'{pod,resourceVersion}',p_bootstrap_observation#>>'{workload,apiVersion}',
       p_bootstrap_observation#>>'{workload,namespace}',p_bootstrap_observation#>>'{workload,name}',
       p_bootstrap_observation#>>'{workload,uid}',p_bootstrap_observation#>>'{workload,resourceVersion}',
       p_bootstrap_observation#>>'{workload,clusterQueue}',p_bootstrap_observation#>>'{workload,owner,apiVersion}',
       p_bootstrap_observation#>>'{workload,owner,kind}',p_bootstrap_observation#>>'{workload,owner,name}',
       p_bootstrap_observation#>>'{workload,owner,uid}',(p_bootstrap_observation#>>'{workload,owner,controller}')::boolean,
       p_bootstrap_observation#>>'{workload,workspaceId}',p_bootstrap_observation#>>'{workload,sandboxId}',
       (p_bootstrap_observation#>>'{workload,admitted}')::boolean,p_bootstrap_observation#>>'{workload,condition,type}',
       p_bootstrap_observation#>>'{workload,condition,status}',substring(p_bootstrap_observation#>>'{workload,digest}' from 8))
  THEN RETURN false; END IF;

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
  IF jsonb_typeof(p_receipt)<>'object' OR jsonb_strip_nulls(p_receipt)<>p_receipt OR
     jsonb_typeof(p_receipt->'sources')<>'array' THEN RETURN false; END IF;
  IF expected_manifest IS NULL OR expected_manifest<>p_manifest_digest OR
     (SELECT array_agg(key ORDER BY key) FROM jsonb_object_keys(p_receipt) key)<>
       ARRAY['digest','manifestDigest','schemaVersion','sources']::text[] OR
     p_receipt->>'schemaVersion'<>'blazn.dev/sandbox-source-materialization/v1' OR
     p_receipt->>'manifestDigest'<>'sha256:'||p_manifest_digest OR
     p_receipt->>'digest'<>'sha256:'||p_receipt_digest OR
     jsonb_array_length(p_receipt->'sources')<>(SELECT count(*) FROM public.sandbox_sources s WHERE s.sandbox_id=target.sandbox_id) OR
     public.sandbox_source_receipt_digest(p_manifest_digest,p_receipt->'sources')<>p_receipt_digest THEN RETURN false; END IF;

  FOR item IN
    SELECT value,position FROM jsonb_array_elements(p_receipt->'sources') WITH ORDINALITY listed(value,position) ORDER BY position
  LOOP
    IF jsonb_typeof(item.value)<>'object' OR jsonb_strip_nulls(item.value)<>item.value THEN RETURN false; END IF;
    IF
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
    observation_digest,manifest_digest,receipt_digest,receipt,bootstrap_observation)
  VALUES(target.sandbox_id,target.workspace_id,p_operation_id,p_expected_backend_uid,
    p_expected_backend_resource_version,p_expected_observation_digest,p_manifest_digest,p_receipt_digest,p_receipt,p_bootstrap_observation)
  ON CONFLICT (sandbox_id) DO NOTHING;
  RETURN EXISTS(SELECT 1 FROM public.sandbox_source_materialization_receipts r
    WHERE r.sandbox_id=target.sandbox_id AND r.workspace_id=target.workspace_id AND r.operation_id=p_operation_id AND
      r.backend_uid=p_expected_backend_uid AND r.backend_resource_version=p_expected_backend_resource_version AND
      r.observation_digest=p_expected_observation_digest AND r.manifest_digest=p_manifest_digest AND
      r.receipt_digest=p_receipt_digest AND r.receipt=p_receipt AND r.bootstrap_observation=p_bootstrap_observation);
END
$$;

CREATE FUNCTION sandbox_controller_bind_backend_v4(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_backend_uid text, p_backend_resource_version text,
  p_sandbox_api_version text, p_sandbox_kind text, p_sandbox_namespace text,
  p_sandbox_name text, p_sandbox_uid text, p_sandbox_resource_version text,
  p_pod_api_version text, p_pod_kind text, p_pod_namespace text,
  p_pod_name text, p_pod_uid text, p_pod_resource_version text,
  p_workload_api_version text, p_workload_namespace text, p_workload_name text,
  p_workload_uid text, p_workload_resource_version text, p_admitted_cluster_queue text,
  p_owner_api_version text, p_owner_kind text, p_owner_name text, p_owner_uid text,
  p_owner_controller boolean, p_workspace_label text, p_sandbox_label text,
  p_admitted boolean, p_condition_type text, p_condition_status text,
  p_workload_digest text, p_observation_digest text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE source_count integer; bootstrap jsonb;
BEGIN
  SELECT count(*) INTO source_count FROM public.sandbox_sources s
    JOIN public.sandbox_operations o ON o.sandbox_id=s.sandbox_id AND o.workspace_id=s.workspace_id
    WHERE o.id=p_operation_id;
  IF source_count>0 THEN
    SELECT r.bootstrap_observation INTO bootstrap FROM public.sandbox_source_materialization_receipts r
      WHERE r.operation_id=p_operation_id AND r.sandbox_id=p_sandbox_name::uuid AND
        r.workspace_id=p_workspace_label::uuid AND r.backend_uid=p_backend_uid;
    IF bootstrap IS NULL OR
       bootstrap#>>'{sandbox,uid}'<>p_backend_uid OR
       bootstrap#>>'{pod,apiVersion}'<>p_pod_api_version OR bootstrap#>>'{pod,kind}'<>p_pod_kind OR
       bootstrap#>>'{pod,namespace}'<>p_pod_namespace OR bootstrap#>>'{pod,name}'<>p_pod_name OR
       bootstrap#>>'{pod,uid}'<>p_pod_uid OR bootstrap#>>'{pod,resourceVersion}'<>p_pod_resource_version OR
       bootstrap#>>'{workload,apiVersion}'<>p_workload_api_version OR bootstrap#>>'{workload,namespace}'<>p_workload_namespace OR
       bootstrap#>>'{workload,name}'<>p_workload_name OR bootstrap#>>'{workload,uid}'<>p_workload_uid OR
       bootstrap#>>'{workload,resourceVersion}'<>p_workload_resource_version OR
       bootstrap#>>'{workload,clusterQueue}'<>p_admitted_cluster_queue OR
       bootstrap#>>'{workload,owner,apiVersion}'<>p_owner_api_version OR bootstrap#>>'{workload,owner,kind}'<>p_owner_kind OR
       bootstrap#>>'{workload,owner,name}'<>p_owner_name OR bootstrap#>>'{workload,owner,uid}'<>p_owner_uid OR
       (bootstrap#>>'{workload,owner,controller}')::boolean<>p_owner_controller OR
       bootstrap#>>'{workload,workspaceId}'<>p_workspace_label OR bootstrap#>>'{workload,sandboxId}'<>p_sandbox_label OR
       (bootstrap#>>'{workload,admitted}')::boolean<>p_admitted OR
       bootstrap#>>'{workload,condition,type}'<>p_condition_type OR bootstrap#>>'{workload,condition,status}'<>p_condition_status OR
       substring(bootstrap#>>'{workload,digest}' from 8)<>p_workload_digest THEN RETURN false; END IF;
  END IF;
  RETURN public.sandbox_controller_bind_backend_v3(
    p_operation_id,p_worker_id,p_lease_token,p_backend_uid,p_backend_resource_version,
    p_sandbox_api_version,p_sandbox_kind,p_sandbox_namespace,p_sandbox_name,p_sandbox_uid,p_sandbox_resource_version,
    p_pod_api_version,p_pod_kind,p_pod_namespace,p_pod_name,p_pod_uid,p_pod_resource_version,
    p_workload_api_version,p_workload_namespace,p_workload_name,p_workload_uid,p_workload_resource_version,
    p_admitted_cluster_queue,p_owner_api_version,p_owner_kind,p_owner_name,p_owner_uid,p_owner_controller,
    p_workspace_label,p_sandbox_label,p_admitted,p_condition_type,p_condition_status,p_workload_digest,p_observation_digest);
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
  pod_resource_version text, observation_digest text, source_materialization_receipt jsonb,
  source_bootstrap_observation jsonb)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
  SELECT claimed.*,receipt.receipt,receipt.bootstrap_observation
  FROM public.sandbox_controller_claim_v3(p_worker_id,p_lease_seconds) claimed
  LEFT JOIN public.sandbox_source_materialization_receipts receipt
    ON receipt.sandbox_id=claimed.sandbox_id AND receipt.workspace_id=claimed.workspace_id AND
       receipt.operation_id=claimed.operation_id
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
      WHERE r.sandbox_id=o.sandbox_id AND r.operation_id=o.id) source_receipt
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
  sandbox_reject_nested_repository_destinations(),
  sandbox_source_digest_field(text),sandbox_source_manifest_digest(text[],text[],text[],text[],boolean[]),
  sandbox_source_receipt_digest(text,jsonb),sandbox_source_receipts_immutable(),
  sandbox_controller_record_source_materialization_v1(uuid,text,uuid,text,text,text,text,text,jsonb,jsonb),
  sandbox_controller_bind_backend_v4(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text),
  sandbox_controller_claim_v4(text,integer),
  sandbox_controller_complete_v4(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE ALL ON FUNCTION
  sandbox_controller_claim_v3(text,integer),
  sandbox_controller_bind_backend_v3(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text),
  sandbox_controller_complete_v3(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  FROM blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION
  sandbox_controller_record_source_materialization_v1(uuid,text,uuid,text,text,text,text,text,jsonb,jsonb),
  sandbox_controller_bind_backend_v4(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text),
  sandbox_controller_claim_v4(text,integer),
  sandbox_controller_complete_v4(uuid,text,uuid,text,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  TO blazn_sandbox_controller;
