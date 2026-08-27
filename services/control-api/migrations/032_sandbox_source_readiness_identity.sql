BEGIN;

-- Source bootstrap freezes object identity, but Pod, Workload, and Sandbox
-- resourceVersions legitimately advance while the init gate is released and
-- readiness is reported. Bind the current self-validating observation only
-- when every stable UID and ownership field still matches the bootstrap
-- receipt; do not require the transient resourceVersions to remain equal.
CREATE OR REPLACE FUNCTION sandbox_controller_bind_backend_v4(
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
       bootstrap#>>'{pod,uid}'<>p_pod_uid OR
       bootstrap#>>'{workload,apiVersion}'<>p_workload_api_version OR bootstrap#>>'{workload,namespace}'<>p_workload_namespace OR
       bootstrap#>>'{workload,name}'<>p_workload_name OR bootstrap#>>'{workload,uid}'<>p_workload_uid OR
       bootstrap#>>'{workload,clusterQueue}'<>p_admitted_cluster_queue OR
       bootstrap#>>'{workload,owner,apiVersion}'<>p_owner_api_version OR bootstrap#>>'{workload,owner,kind}'<>p_owner_kind OR
       bootstrap#>>'{workload,owner,name}'<>p_owner_name OR bootstrap#>>'{workload,owner,uid}'<>p_owner_uid OR
       (bootstrap#>>'{workload,owner,controller}')::boolean<>p_owner_controller OR
       bootstrap#>>'{workload,workspaceId}'<>p_workspace_label OR bootstrap#>>'{workload,sandboxId}'<>p_sandbox_label OR
       (bootstrap#>>'{workload,admitted}')::boolean<>p_admitted OR
       bootstrap#>>'{workload,condition,type}'<>p_condition_type OR bootstrap#>>'{workload,condition,status}'<>p_condition_status
    THEN RETURN false; END IF;
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

REVOKE ALL ON FUNCTION
  sandbox_controller_bind_backend_v4(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text)
  FROM PUBLIC, blazn_runtime, blazn_bootstrap, blazn_node_broker, blazn_sandbox_controller;
GRANT EXECUTE ON FUNCTION
  sandbox_controller_bind_backend_v4(uuid,text,uuid,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,text,boolean,text,text,boolean,text,text,text,text)
  TO blazn_sandbox_controller;

COMMIT;
