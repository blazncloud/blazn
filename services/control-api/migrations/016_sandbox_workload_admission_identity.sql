-- Persist the exact observed Kueue admission and replace scalar controller
-- bind/complete authority with digest-bound typed entrypoints.

CREATE FUNCTION sandbox_workload_admission_digest(
  p_api_version text, p_namespace text, p_name text, p_uid text, p_resource_version text,
  p_cluster_queue text, p_owner_api_version text, p_owner_kind text, p_owner_name text,
  p_owner_uid text, p_owner_controller boolean, p_workspace_label text, p_sandbox_label text,
  p_admitted boolean, p_condition_type text, p_condition_status text)
RETURNS text
LANGUAGE sql IMMUTABLE STRICT
SET search_path = pg_catalog, public
RETURN encode(public.digest(convert_to(
  'sandbox-workload-admission-v1'||E'\n'||p_api_version||E'\n'||p_namespace||E'\n'||p_name||E'\n'||
  p_uid||E'\n'||p_resource_version||E'\n'||p_cluster_queue||E'\n'||p_owner_api_version||E'\n'||
  p_owner_kind||E'\n'||p_owner_name||E'\n'||p_owner_uid||E'\n'||p_owner_controller::text||E'\n'||
  p_workspace_label||E'\n'||p_sandbox_label||E'\n'||p_admitted::text||E'\n'||p_condition_type||E'\n'||
  p_condition_status,'UTF8'),'sha256'),'hex');

CREATE TABLE sandbox_workload_admissions (
  sandbox_id uuid PRIMARY KEY,
  workspace_id uuid NOT NULL,
  operation_id uuid NOT NULL UNIQUE,
  operation_type text NOT NULL DEFAULT 'create' CHECK (operation_type='create'),
  backend_uid text NOT NULL CHECK (backend_uid ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
  backend_resource_version text NOT NULL CHECK (backend_resource_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
  api_version text NOT NULL CHECK (api_version='kueue.x-k8s.io/v1beta1'),
  namespace text NOT NULL CHECK (namespace='blazn-poc-sandboxes'),
  workload_name text NOT NULL CHECK (char_length(workload_name)<=253 AND workload_name ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$'),
  workload_uid text NOT NULL CHECK (workload_uid ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
  workload_resource_version text NOT NULL CHECK (workload_resource_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
  admitted_cluster_queue text NOT NULL CHECK (char_length(admitted_cluster_queue)<=253 AND admitted_cluster_queue ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$'),
  owner_api_version text NOT NULL CHECK (owner_api_version='agents.x-k8s.io/v1beta1'),
  owner_kind text NOT NULL CHECK (owner_kind='Sandbox'),
  owner_name text NOT NULL,
  owner_uid text NOT NULL,
  owner_controller boolean NOT NULL CHECK (owner_controller),
  workspace_label text NOT NULL,
  sandbox_label text NOT NULL,
  admitted boolean NOT NULL CHECK (admitted),
  condition_type text NOT NULL CHECK (condition_type='Admitted'),
  condition_status text NOT NULL CHECK (condition_status='True'),
  admission_digest char(64) NOT NULL,
  observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (sandbox_id, admission_digest),
  UNIQUE (namespace, workload_uid),
  FOREIGN KEY (sandbox_id,workspace_id) REFERENCES sandboxes(id,workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (operation_id,workspace_id,sandbox_id,operation_type)
    REFERENCES sandbox_operations(id,workspace_id,sandbox_id,type) ON DELETE RESTRICT,
  CHECK (owner_uid=backend_uid),
  CHECK (owner_name=sandbox_id::text AND workspace_label=workspace_id::text AND sandbox_label=sandbox_id::text),
  CHECK (admission_digest=sandbox_workload_admission_digest(api_version,namespace,workload_name,workload_uid,
    workload_resource_version,admitted_cluster_queue,owner_api_version,owner_kind,owner_name,owner_uid,
    owner_controller,workspace_label,sandbox_label,admitted,condition_type,condition_status))
);

ALTER TABLE sandbox_operation_terminal_receipts ADD COLUMN admission_digest char(64);
ALTER TABLE sandbox_operation_terminal_receipts ADD CONSTRAINT sandbox_terminal_admission_fk
  FOREIGN KEY (sandbox_id,admission_digest) REFERENCES sandbox_workload_admissions(sandbox_id,admission_digest);

CREATE OR REPLACE FUNCTION sandbox_enforce_terminal_receipt() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE current_backend_uid text; current_backend_resource_version text;
BEGIN
  IF TG_OP='UPDATE' AND OLD.admission_digest IS NULL AND NEW.admission_digest IS NOT NULL AND
     (to_jsonb(OLD)-'admission_digest')=(to_jsonb(NEW)-'admission_digest') THEN
    RETURN NEW;
  END IF;
  IF TG_OP<>'INSERT' THEN
    RAISE EXCEPTION 'sandbox operation terminal receipts are immutable' USING ERRCODE='55000';
  END IF;
  IF NEW.operation_type='create' AND NEW.status='succeeded' THEN
    SELECT backend_uid,backend_resource_version INTO current_backend_uid,current_backend_resource_version
      FROM public.sandboxes WHERE id=NEW.sandbox_id AND workspace_id=NEW.workspace_id;
    IF current_backend_uid IS NULL OR (NEW.backend_uid,NEW.backend_resource_version) IS DISTINCT FROM
       (current_backend_uid,current_backend_resource_version) THEN
      RAISE EXCEPTION 'sandbox create receipt backend identity mismatch' USING ERRCODE='23514';
    END IF;
  END IF;
  RETURN NEW;
END
$$;

CREATE FUNCTION sandbox_enforce_successful_create_admission() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE persisted_digest char(64);
BEGIN
  SELECT admission_digest INTO persisted_digest FROM public.sandbox_operation_terminal_receipts WHERE id=NEW.id;
  IF NEW.operation_type='create' AND NEW.status='succeeded' AND persisted_digest IS NULL THEN
    RAISE EXCEPTION 'successful create requires digest-bound Workload admission identity' USING ERRCODE='23514';
  END IF;
  RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER sandbox_successful_create_admission
AFTER INSERT OR UPDATE ON sandbox_operation_terminal_receipts DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION sandbox_enforce_successful_create_admission();

CREATE FUNCTION sandbox_controller_bind_backend_v2(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid,
  p_backend_uid text, p_backend_resource_version text,
  p_api_version text, p_namespace text, p_workload_name text, p_workload_uid text,
  p_workload_resource_version text, p_admitted_cluster_queue text,
  p_owner_api_version text, p_owner_kind text, p_owner_name text, p_owner_uid text,
  p_owner_controller boolean, p_workspace_label text, p_sandbox_label text,
  p_admitted boolean, p_condition_type text, p_condition_status text, p_admission_digest text)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; bound boolean; expected_digest text;
BEGIN
  SELECT o.workspace_id,o.sandbox_id,o.type INTO target
    FROM public.sandbox_operations o JOIN public.sandbox_reconcile_jobs j ON j.operation_id=o.id
    WHERE o.id=p_operation_id AND j.completed_at IS NULL AND j.lease_owner=p_worker_id
      AND j.lease_token=p_lease_token AND j.lease_expires_at>clock_timestamp();
  IF NOT FOUND OR target.type<>'create' THEN RETURN false; END IF;
  IF p_api_version<>'kueue.x-k8s.io/v1beta1' OR p_namespace<>'blazn-poc-sandboxes' OR
     p_owner_api_version<>'agents.x-k8s.io/v1beta1' OR p_owner_kind<>'Sandbox' OR
     p_owner_name<>target.sandbox_id::text OR p_owner_uid<>p_backend_uid OR p_owner_controller IS NOT TRUE OR
     p_workspace_label<>target.workspace_id::text OR p_sandbox_label<>target.sandbox_id::text OR
     p_admitted IS NOT TRUE OR p_condition_type<>'Admitted' OR p_condition_status<>'True' OR
     char_length(p_workload_name)>253 OR p_workload_name !~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$' OR
     char_length(p_admitted_cluster_queue)>253 OR p_admitted_cluster_queue !~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$' THEN
    RETURN false;
  END IF;
  expected_digest := public.sandbox_workload_admission_digest(p_api_version,p_namespace,p_workload_name,
    p_workload_uid,p_workload_resource_version,p_admitted_cluster_queue,p_owner_api_version,p_owner_kind,
    p_owner_name,p_owner_uid,p_owner_controller,p_workspace_label,p_sandbox_label,p_admitted,
    p_condition_type,p_condition_status);
  IF p_admission_digest IS NULL OR p_admission_digest<>expected_digest THEN RETURN false; END IF;
  bound := public.sandbox_controller_bind_backend(p_operation_id,p_worker_id,p_lease_token,
    p_backend_uid,p_backend_resource_version,p_workload_uid);
  IF NOT bound THEN RETURN false; END IF;
  INSERT INTO public.sandbox_workload_admissions(sandbox_id,workspace_id,operation_id,backend_uid,
    backend_resource_version,api_version,namespace,workload_name,workload_uid,workload_resource_version,
    admitted_cluster_queue,owner_api_version,owner_kind,owner_name,owner_uid,owner_controller,
    workspace_label,sandbox_label,admitted,condition_type,condition_status,admission_digest)
  VALUES(target.sandbox_id,target.workspace_id,p_operation_id,p_backend_uid,p_backend_resource_version,
    p_api_version,p_namespace,p_workload_name,p_workload_uid,p_workload_resource_version,p_admitted_cluster_queue,
    p_owner_api_version,p_owner_kind,p_owner_name,p_owner_uid,p_owner_controller,p_workspace_label,p_sandbox_label,
    p_admitted,p_condition_type,p_condition_status,p_admission_digest)
  ON CONFLICT (sandbox_id) DO NOTHING;
  RETURN EXISTS(SELECT 1 FROM public.sandbox_workload_admissions a
    WHERE a.sandbox_id=target.sandbox_id AND a.workspace_id=target.workspace_id AND
      a.operation_id=p_operation_id AND a.backend_uid=p_backend_uid AND
      a.backend_resource_version=p_backend_resource_version AND a.admission_digest=p_admission_digest);
END
$$;

CREATE FUNCTION sandbox_controller_complete_v2(
  p_operation_id uuid, p_worker_id text, p_lease_token uuid, p_status text,
  p_expected_backend_uid text, p_expected_backend_resource_version text, p_expected_admission_digest text,
  p_cleanup_complete boolean, p_artifact_export_complete boolean, p_grants_revoked boolean,
  p_backend_destroyed boolean, p_artifact_ids uuid[], p_warning_codes text[],
  p_error_code text, p_safe_message text, p_request_id uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
DECLARE target record; completed boolean;
BEGIN
  SELECT o.type,s.backend_uid,s.backend_resource_version,s.admission_id,a.admission_digest,a.workload_uid
    INTO target FROM public.sandbox_operations o
    JOIN public.sandboxes s ON s.id=o.sandbox_id AND s.workspace_id=o.workspace_id
    LEFT JOIN public.sandbox_workload_admissions a ON a.sandbox_id=s.id AND a.workspace_id=s.workspace_id
    WHERE o.id=p_operation_id;
  IF NOT FOUND THEN RETURN false; END IF;
  IF target.backend_uid IS NOT NULL OR (target.type='create' AND p_status='succeeded') THEN
    IF target.admission_digest IS NULL OR p_expected_admission_digest IS NULL OR
       target.admission_digest<>p_expected_admission_digest OR target.workload_uid<>target.admission_id THEN
      RETURN false;
    END IF;
  ELSIF p_expected_admission_digest IS NOT NULL THEN RETURN false;
  END IF;
  completed := public.sandbox_controller_complete(p_operation_id,p_worker_id,p_lease_token,p_status,
    p_expected_backend_uid,p_expected_backend_resource_version,target.admission_id,p_cleanup_complete,
    p_artifact_export_complete,p_grants_revoked,p_backend_destroyed,p_artifact_ids,p_warning_codes,
    p_error_code,p_safe_message,p_request_id);
  IF NOT completed THEN RETURN false; END IF;
  UPDATE public.sandbox_operation_terminal_receipts r SET admission_digest=target.admission_digest
    WHERE r.operation_id=p_operation_id AND target.admission_digest IS NOT NULL;
  RETURN true;
END
$$;

-- Re-project claims with the persisted admission tuple. The old v2 signature is
-- removed atomically by this migration and remains controller-only.
REVOKE ALL ON FUNCTION sandbox_controller_claim_v2(text,integer) FROM blazn_sandbox_controller;
DROP FUNCTION sandbox_controller_claim_v2(text,integer);
CREATE FUNCTION sandbox_controller_claim_v2(p_worker_id text, p_lease_seconds integer)
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
  workspace_label text, sandbox_label text, admitted boolean, condition_type text, condition_status text)
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
AS $$
  SELECT claimed.operation_id,claimed.workspace_id,claimed.sandbox_id,s.requested_by,
    claimed.operation_type,claimed.expected_sandbox_version,claimed.lease_token,claimed.lease_expires_at,
    claimed.attempt,claimed.allocation_mode,claimed.desired_state,claimed.architecture,claimed.template_version_id,
    claimed.template_digest,claimed.variant_name,claimed.image_index_digest,claimed.image_child_digest,
    claimed.placement_profile,claimed.command,claimed.request_cpu,claimed.request_memory,claimed.request_ephemeral_storage,
    claimed.limit_cpu,claimed.limit_memory,claimed.limit_ephemeral_storage,claimed.queue_name,claimed.admission_id,
    claimed.backend_uid,claimed.backend_resource_version,claimed.expires_at,
    claimed.source_names,claimed.source_urls,claimed.source_destinations,claimed.source_writable,claimed.source_commits,
    coalesce(artifacts.names,'{}'::text[]),coalesce(artifacts.paths,'{}'::text[]),
    coalesce(artifacts.media_types,'{}'::text[]),coalesce(artifacts.required,'{}'::boolean[]),
    a.admission_digest::text,a.api_version,a.namespace,a.workload_name,a.workload_uid,a.workload_resource_version,
    a.admitted_cluster_queue,a.owner_api_version,a.owner_kind,a.owner_name,a.owner_uid,a.owner_controller,
    a.workspace_label,a.sandbox_label,a.admitted,a.condition_type,a.condition_status
  FROM public.sandbox_controller_claim(p_worker_id,p_lease_seconds) claimed
  JOIN public.sandboxes s ON s.id=claimed.sandbox_id AND s.workspace_id=claimed.workspace_id
  LEFT JOIN public.sandbox_workload_admissions a ON a.sandbox_id=s.id AND a.workspace_id=s.workspace_id
  LEFT JOIN LATERAL (
    SELECT array_agg(entry.name ORDER BY entry.name) names,array_agg(entry.path ORDER BY entry.name) paths,
      array_agg(entry.media_type ORDER BY entry.name) media_types,array_agg(entry.required ORDER BY entry.name) required
    FROM public.sandbox_artifact_contract_entries entry
    WHERE entry.sandbox_id=claimed.sandbox_id AND entry.workspace_id=claimed.workspace_id
  ) artifacts ON true
$$;

REVOKE ALL ON TABLE sandbox_workload_admissions FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
REVOKE UPDATE (backend_uid,backend_resource_version,queue_name,admission_id,conditions,stopped_at,deleted_at)
  ON TABLE sandboxes FROM blazn_runtime;
REVOKE INSERT ON TABLE sandbox_operation_terminal_receipts FROM blazn_runtime;
REVOKE UPDATE (status,terminal_receipt_id,started_at,completed_at) ON TABLE sandbox_operations FROM blazn_runtime;
REVOKE ALL ON FUNCTION sandbox_workload_admission_digest(text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text),
  sandbox_controller_bind_backend(uuid,text,uuid,text,text,text),
  sandbox_controller_bind_backend_v2(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text),
  sandbox_controller_complete(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid),
  sandbox_controller_complete_v2(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid),
  sandbox_controller_claim_v2(text,integer)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION sandbox_controller_claim_v2(text,integer),
  sandbox_controller_bind_backend_v2(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text),
  sandbox_controller_complete_v2(uuid,text,uuid,text,text,text,text,boolean,boolean,boolean,boolean,uuid[],text[],text,text,uuid)
  TO blazn_sandbox_controller;
