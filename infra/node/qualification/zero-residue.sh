#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

before=${1:-}
after=${2:-}
[ -f "$before" ] && [ -f "$after" ] || qual_die 'usage: zero-residue.sh BEFORE-INVENTORY AFTER-INVENTORY'
qual_require_target
"$qual_dir/compare-invariants.py" "$before" "$after" >/dev/null

if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
  target_before=${3:-}
  target_after=${4:-}
  [ -f "$target_before" ] && [ -f "$target_after" ] || qual_die 'fresh Linux zero-residue requires target before/after inventories'
  "$qual_dir/compare-target-state.py" "$target_before" "$target_after" >/dev/null
  qual_guest_name_matches_correlation
  if lxc info "$BLAZN_QUALIFICATION_TARGET" >/dev/null 2>&1; then
    qual_die 'disposable LXD guest remains after cleanup'
  fi
fi

if [ -n "${BLAZN_QUALIFICATION_KUBE_CONTEXT:-}" ]; then
  qual_refuse_shared_cluster
  node=${BLAZN_QUALIFICATION_KUBE_NODE:-}
  [ -n "$node" ] || qual_die 'Kubernetes node name is required for residue scan'
  if kubectl --context "$BLAZN_QUALIFICATION_KUBE_CONTEXT" get node "$node" >/dev/null 2>&1; then
    qual_die 'qualification Kubernetes Node remains after cleanup'
  fi
  leftovers=$(kubectl --context "$BLAZN_QUALIFICATION_KUBE_CONTEXT" get all,configmap,secret,serviceaccount,role,rolebinding,persistentvolumeclaim,networkpolicy,poddisruptionbudget,ingress,clusterrole,clusterrolebinding,persistentvolume,namespace --all-namespaces -l "blazn.dev/qualification-correlation=${BLAZN_QUALIFICATION_CORRELATION_ID}" -o name 2>/dev/null | awk 'END {print NR}')
  [ "${leftovers:-1}" -eq 0 ] || qual_die "${leftovers} correlation-labelled Kubernetes resources remain"
fi

jq -n --arg correlation "$BLAZN_QUALIFICATION_CORRELATION_ID" '{schemaVersion:1,status:"passed",correlationId:$correlation,guestResidue:0,kubernetesResidue:0,protectedInvariants:true}'
