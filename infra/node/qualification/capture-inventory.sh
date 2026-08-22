#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

phase=${1:-}
case "$phase" in before|after) ;; *) qual_die 'usage: capture-inventory.sh before|after' ;; esac
qual_require_target
qual_require_command jq
qual_require_command systemctl
qual_require_command docker

protected_units=${BLAZN_QUALIFICATION_PROTECTED_UNITS:-}
protected_containers=${BLAZN_QUALIFICATION_PROTECTED_CONTAINERS:-}
[ -n "$protected_units" ] || qual_die 'BLAZN_QUALIFICATION_PROTECTED_UNITS must explicitly name existing workload/HomeAI units'
[ -n "$protected_containers" ] || qual_die 'BLAZN_QUALIFICATION_PROTECTED_CONTAINERS must explicitly name existing workload/HomeAI containers'

tmp_dir=$(mktemp -d)
trap 'rm -rf -- "$tmp_dir"' EXIT

: >"$tmp_dir/units.jsonl"
IFS=, read -r -a unit_list <<<"$protected_units"
for unit in "${unit_list[@]}"; do
  case "$unit" in ''|*[!a-zA-Z0-9@_.:-]*) qual_die "unsafe protected unit name: ${unit}" ;; esac
  unit_value=$(systemctl show "$unit" --property=Id,LoadState,ActiveState,SubState,UnitFileState --no-pager)
  grep -q '^LoadState=loaded$' <<<"$unit_value" || qual_die "protected unit is not loaded: ${unit}"
  jq -Rn --arg name "$unit" '[inputs | select(length > 0) | split("=") | {(.[0]): .[1]}] | add + {requestedName:$name}' <<<"$unit_value" >>"$tmp_dir/units.jsonl"
done

: >"$tmp_dir/containers.jsonl"
IFS=, read -r -a container_list <<<"$protected_containers"
for container in "${container_list[@]}"; do
  case "$container" in ''|*[!a-zA-Z0-9_.-]*) qual_die "unsafe protected container name: ${container}" ;; esac
  docker inspect "$container" >/dev/null 2>&1 || qual_die "protected HomeAI/container target is not observable: ${container}"
  docker inspect --format '{{json .}}' "$container" |
    jq '{name:(.Name|ltrimstr("/")), image:.Image, imageName:.Config.Image, running:.State.Running, status:.State.Status, restartCount:.RestartCount}' >>"$tmp_dir/containers.jsonl"
done

target_json='{}'
case "$BLAZN_QUALIFICATION_PROFILE" in
  lxd-ubuntu-26.04)
    qual_guest_name_matches_correlation
    if lxc info "$BLAZN_QUALIFICATION_TARGET" >/dev/null 2>&1; then
      [ "$(lxc config get "$BLAZN_QUALIFICATION_TARGET" user.blazn.qualification 2>/dev/null || true)" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'LXD target correlation marker differs'
      target_json=$(lxc list "$BLAZN_QUALIFICATION_TARGET" --format=json | jq '.[0] | {name,status,type,architecture,location}')
    else
      target_json=$(jq -n --arg name "$BLAZN_QUALIFICATION_TARGET" '{name:$name,status:"absent"}')
    fi
    ;;
  native-mac)
    target_json=$("$qual_dir/native-mac-preflight.sh")
    ;;
  *) qual_die 'unsupported qualification profile' ;;
esac

jq -s 'sort_by(.requestedName // .name)' "$tmp_dir/units.jsonl" >"$tmp_dir/units.json"
jq -s 'sort_by(.name)' "$tmp_dir/containers.jsonl" >"$tmp_dir/containers.json"
jq -n \
  --arg phase "$phase" \
  --arg correlation "$BLAZN_QUALIFICATION_CORRELATION_ID" \
  --arg observedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg sourceHead "$(git -C "$repo_root" rev-parse HEAD)" \
  --arg sourceTree "$(git -C "$repo_root" rev-parse 'HEAD^{tree}')" \
  --argjson units "$(cat "$tmp_dir/units.json")" \
  --argjson containers "$(cat "$tmp_dir/containers.json")" \
  --argjson target "$target_json" \
  '{schemaVersion:1,phase:$phase,correlationId:$correlation,observedAt:$observedAt,source:{head:$sourceHead,tree:$sourceTree},protected:{units:$units,containers:$containers},target:$target}'
