#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

action=${1:-observe}
lifecycle=${2:-install}
checkpoint=${3:-}
qual_require_target
[ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ] || qual_die 'fault injection is allowed only in the disposable LXD guest'
qual_guest_name_matches_correlation
qual_require_command lxc
[ "$(lxc config get "$BLAZN_QUALIFICATION_TARGET" user.blazn.qualification 2>/dev/null || true)" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'guest correlation marker differs'

case "$lifecycle:$checkpoint" in
  install:join_intent|install:join|install:binding|install:broker_consume|install:broker_consumed|install:verify|install:receipt) ;;
  cleanup:cleanup_pending|cleanup:cleanup_support_removed|cleanup:cleanup_local_state_removed) ;;
  *) qual_die 'checkpoint is not in the reviewed install/cleanup fault-injection allowlist' ;;
esac

wal=/var/lib/blazn-node-root/install-wal.json
observe_checkpoint() {
  observed=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- jq -r '.lifecycle + ":" + .checkpoint' "$wal")
  if [ "$lifecycle" = cleanup ]; then
    [ "${observed%%:*}" = uninstall ] || qual_die 'cleanup checkpoint does not belong to uninstall WAL'
    printf 'cleanup:%s\n' "${observed#*:}"
  else
    printf '%s\n' "$observed"
  fi
}

[ "$action" = observe ] || qual_die 'crash-checkpoint.sh is read-only; use the integrated lifecycle crash action'
observe_checkpoint
