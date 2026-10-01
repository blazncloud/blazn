-- The sandbox controller records which Blazn node runs an Agent Run's
-- sandbox. It sees the scheduled Pod's node name but has no Kubernetes
-- permission to read Node objects, and it should not need one: the node's
-- identity is already in Blazn's registry, recorded at activation and kept
-- current by heartbeats. This wrapper resolves the UID of the single active,
-- verified Blazn node with that cluster binding and name in the operation's
-- workspace, then records the observation through the existing lease- and
-- admission-fenced function, which re-checks every registry condition.
CREATE FUNCTION sandbox_controller_record_agent_node_placement(
  p_operation_id uuid,p_worker_id text,p_lease_token uuid,p_admission_observation_digest text,
  p_pod_uid text,p_pod_resource_version text,p_kubernetes_cluster_id text,p_kubernetes_node_name text)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE node_uids text[];
BEGIN
  IF p_operation_id IS NULL OR char_length(p_kubernetes_cluster_id) NOT BETWEEN 1 AND 128 OR
     char_length(p_kubernetes_node_name) NOT BETWEEN 1 AND 253 THEN RETURN false; END IF;
  SELECT array_agg(node.kubernetes_node_uid) INTO node_uids
    FROM public.sandbox_operations operation
    JOIN public.nodes node ON node.workspace_id=operation.workspace_id
    WHERE operation.id=p_operation_id AND operation.status='running'
      AND node.kubernetes_cluster_id=p_kubernetes_cluster_id AND node.kubernetes_node_name=p_kubernetes_node_name
      AND node.kubernetes_node_uid IS NOT NULL AND node.lifecycle_state='active' AND node.trust_state='verified'
      AND node.agent_eligible AND node.current_identity_status='active' AND node.current_capability_version IS NOT NULL;
  -- Zero or several candidates means the registry cannot vouch for one node.
  IF node_uids IS NULL OR cardinality(node_uids)<>1 THEN RETURN false; END IF;
  RETURN public.sandbox_controller_record_agent_node_observation(p_operation_id,p_worker_id,p_lease_token,
    p_admission_observation_digest,p_pod_uid,p_pod_resource_version,p_kubernetes_cluster_id,p_kubernetes_node_name,node_uids[1]);
END $$;

REVOKE ALL ON FUNCTION sandbox_controller_record_agent_node_placement(uuid,text,uuid,text,text,text,text,text)
  FROM PUBLIC,blazn_runtime,blazn_bootstrap,blazn_node_broker,blazn_sandbox_controller,blazn_development_controller,blazn_agent_run_controller;
GRANT EXECUTE ON FUNCTION sandbox_controller_record_agent_node_placement(uuid,text,uuid,text,text,text,text,text) TO blazn_sandbox_controller;
