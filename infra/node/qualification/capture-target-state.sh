#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

phase=${1:-}
case "$phase" in before|after) ;; *) qual_die 'usage: capture-target-state.sh before|after' ;; esac
qual_require_target
[ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ] || qual_die 'automated target-state inventory is defined only for the disposable Ubuntu guest'
qual_guest_name_matches_correlation
qual_require_command lxc
qual_require_command jq
[ "$(lxc config get "$BLAZN_QUALIFICATION_TARGET" user.blazn.qualification 2>/dev/null || true)" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'guest correlation marker differs'

guest=$BLAZN_QUALIFICATION_TARGET
guest_script=$(cat <<'SH'
set -o errexit
set -o nounset
set -o pipefail
digest_stream() { sha256sum | awk '{print "sha256:" $1}'; }
path_inventory() {
  for root in /usr/local/bin/blazn /etc/blazn/node /etc/systemd/system/blazn-node.service /etc/sudoers.d/blazn-node-observe /var/lib/blazn/node /var/lib/blazn-node-root /var/snap/microk8s /snap/microk8s; do
    if [ ! -e "$root" ] && [ ! -L "$root" ]; then
      printf '%s|absent\n' "$root"
    elif [ -f "$root" ] && [ ! -L "$root" ]; then
      stat -c '%n|file|%a|%u|%g|%s' "$root"
      printf '%s|sha256:%s\n' "$root" "$(sha256sum "$root" | awk '{print $1}')"
    elif [ -d "$root" ] && [ ! -L "$root" ]; then
      stat -c '%n|directory|%a|%u|%g' "$root"
      find "$root" -xdev -mindepth 1 -printf '%p|%y|%m|%U|%G|%s\n' | LC_ALL=C sort
      find "$root" -xdev -type f -exec sha256sum {} + | LC_ALL=C sort
    else
      stat -c '%n|other-or-symlink|%F|%a|%u|%g' "$root"
    fi
  done
}
printf 'paths=%s\n' "$(path_inventory | digest_stream)"
printf 'packages=%s\n' "$(dpkg-query -W -f='${binary:Package}\t${Version}\n' | LC_ALL=C sort | digest_stream)"
if command -v snap >/dev/null 2>&1; then printf 'snaps=%s\n' "$(snap list 2>/dev/null | LC_ALL=C sort | digest_stream)"; else printf 'snaps=absent\n'; fi
printf 'accounts=%s;%s\n' "$(getent passwd blazn-node 2>/dev/null || printf absent)" "$(getent group blazn-node 2>/dev/null || printf absent)"
printf 'unit=%s\n' "$(systemctl show blazn-node.service --property=LoadState,ActiveState,SubState,UnitFileState --no-pager 2>/dev/null | tr '\n' ',' || printf absent)"
if command -v nft >/dev/null 2>&1; then printf 'firewall=%s\n' "$(nft list ruleset 2>/dev/null | digest_stream)"; else printf 'firewall=absent\n'; fi
if command -v ip >/dev/null 2>&1; then printf 'network=%s\n' "$({ ip -details address show; ip route show table all; ip -6 route show table all; } | digest_stream)"; else printf 'network=absent\n'; fi
printf 'kernel=%s\n' "$(uname -srm)"
awk -F= '$1=="ID" || $1=="VERSION_ID" {gsub(/"/,"",$2); printf "os_%s=%s\n",tolower($1),$2}' /etc/os-release
SH
)

raw=$(lxc exec "$guest" -- bash -c "$guest_script")
jq -Rn \
  --arg phase "$phase" \
  --arg correlation "$BLAZN_QUALIFICATION_CORRELATION_ID" \
  --arg target "$guest" \
  --arg observedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg sourceHead "$(git -C "$repo_root" rev-parse HEAD)" \
  --arg sourceTree "$(git -C "$repo_root" rev-parse 'HEAD^{tree}')" \
  --arg raw "$raw" \
  '{schemaVersion:1,phase:$phase,correlationId:$correlation,target:$target,observedAt:$observedAt,source:{head:$sourceHead,tree:$sourceTree},state:($raw|split("\n")|map(select(length>0)|capture("^(?<key>[^=]+)=(?<value>.*)$"))|map({(.key):.value})|add)}'
