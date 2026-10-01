# Blazn sandbox queue

`blazn-queue.yaml` holds the cluster-scoped Kueue objects for Blazn sandboxes:

- the `blazn-sandbox` ResourceFlavor, which selects `blazn.dev/sandbox-eligible=true` nodes and declares no tolerations;
- the `blazn-sandboxes` ClusterQueue, which admits only the `blazn-poc-sandboxes` namespace, has no cohort, and never preempts.

The phase 5 boundary owns the `blazn-sandboxes` LocalQueue that targets this ClusterQueue. The sandbox controller routes Pods to it when `BLAZN_SANDBOX_LOCAL_QUEUE=blazn-sandboxes`.

Kueue quota is static while Blazn nodes come and go. Because the flavor pins placement to sandbox-eligible nodes, quota beyond real node capacity only yields Pending Pods, never placement on Frontro nodes. Size the quota for the expected node fleet.

Apply before upgrading the boundary to a version that includes the dedicated LocalQueue:

```sh
kubectl apply --server-side --field-manager blazn-phase5-queue -f blazn-queue.yaml
```
