#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

action=${1:-inspect}
qual_require_target
qual_refuse_shared_cluster
node=${BLAZN_QUALIFICATION_KUBE_NODE:-}
expected_uid=${BLAZN_QUALIFICATION_EXPECTED_NODE_UID:-}
expected_rv=${BLAZN_QUALIFICATION_EXPECTED_RESOURCE_VERSION:-}
[ -n "$node" ] && [ -n "$expected_uid" ] && [ -n "$expected_rv" ] || qual_die 'exact Kubernetes node name, UID, and resourceVersion are required'

node_json=$(kubectl --context "$BLAZN_QUALIFICATION_KUBE_CONTEXT" get node "$node" -o json)
uid=$(jq -r '.metadata.uid' <<<"$node_json")
rv=$(jq -r '.metadata.resourceVersion' <<<"$node_json")
[ "$uid" = "$expected_uid" ] || qual_die 'Kubernetes Node UID differs from approval'
[ "$rv" = "$expected_rv" ] || qual_die 'Kubernetes Node resourceVersion differs from approval'

case "$action" in
  inspect)
    jq '{schemaVersion:1,node:{name:.metadata.name,uid:.metadata.uid,resourceVersion:.metadata.resourceVersion,unschedulable:(.spec.unschedulable // false),taints:(.spec.taints // [] | sort_by(.key,.effect,.value)),blaznLabels:(.metadata.labels // {} | with_entries(select(.key|startswith("blazn.dev/"))))}}' <<<"$node_json"
    ;;
  quarantine)
    jq -e '(.spec.unschedulable == true) and any((.spec.taints // [])[]; (.key == "blazn.dev/bootstrap" or .key == "blazn.dev/quarantine") and .effect == "NoSchedule")' <<<"$node_json" >/dev/null || qual_die 'node is not unschedulable with an exact Blazn quarantine/bootstrap NoSchedule taint'
    pods=$(kubectl --context "$BLAZN_QUALIFICATION_KUBE_CONTEXT" get pods --all-namespaces --field-selector "spec.nodeName=${node}" -o json)
    jq -e '[.items[] | select((.metadata.deletionTimestamp == null) and ((.metadata.ownerReferences // [] | any(.kind == "DaemonSet")) | not) and (.metadata.annotations["kubernetes.io/config.mirror"] == null))] | length == 0' <<<"$pods" >/dev/null || qual_die 'non-DaemonSet/static workload is present on quarantined node'
    jq -n --arg node "$node" --arg uid "$uid" --arg rv "$rv" '{schemaVersion:1,status:"passed",node:$node,uid:$uid,resourceVersion:$rv,quarantineNoSchedule:true,ordinaryWorkloads:0}'
    ;;
  stale-cas)
    qual_require_approval kubernetes-stale-cas
    do_stale_cas() {
      stale="${expected_rv}-stale"
      current_unschedulable=$(jq -r '.spec.unschedulable // false' <<<"$node_json")
      patch=$(jq -nc --arg stale "$stale" --argjson value "$current_unschedulable" '[{"op":"test","path":"/metadata/resourceVersion","value":$stale},{"op":"add","path":"/spec/unschedulable","value":$value}]')
      if kubectl --context "$BLAZN_QUALIFICATION_KUBE_CONTEXT" patch node "$node" --type=json --patch "$patch" >/dev/null 2>&1; then
        qual_die 'stale resourceVersion CAS unexpectedly succeeded'
      fi
      after=$(kubectl --context "$BLAZN_QUALIFICATION_KUBE_CONTEXT" get node "$node" -o json)
      [ "$(jq -r '.metadata.uid' <<<"$after")" = "$uid" ] || qual_die 'Node UID changed during stale CAS check'
      [ "$(jq -r '.metadata.resourceVersion' <<<"$after")" = "$rv" ] || qual_die 'failed stale CAS changed Node resourceVersion'
      jq -n --arg node "$node" --arg uid "$uid" --arg rv "$rv" '{schemaVersion:1,status:"passed",node:$node,uid:$uid,resourceVersion:$rv,staleCASDenied:true,stateUnchanged:true}'
    }
    qual_with_lock do_stale_cas
    ;;
  *) qual_die 'usage: kubernetes-checks.sh inspect|quarantine|stale-cas' ;;
esac
