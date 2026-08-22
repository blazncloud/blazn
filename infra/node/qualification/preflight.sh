#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

qual_require_target
qual_require_command git
qual_require_command python3
qual_require_command jq

remote=$(git -C "$repo_root" remote get-url origin)
[ "$remote" = 'https://github.com/blazncloud/blazn.git' ] || qual_die "origin is not canonical: ${remote}"
head=$(git -C "$repo_root" rev-parse HEAD)
tree=$(git -C "$repo_root" rev-parse 'HEAD^{tree}')

case "${BLAZN_QUALIFICATION_PROFILE:-}" in
  lxd-ubuntu-26.04)
    qual_guest_name_matches_correlation
    qual_require_command lxc
    lxc info >/dev/null
    ;;
  native-mac)
    "${qual_dir}/native-mac-preflight.sh" >/dev/null
    ;;
  *) qual_die 'profile must be lxd-ubuntu-26.04 or native-mac' ;;
esac

if qual_is_mutation; then
  qual_require_approval preflight
  qual_validate_lock
fi

python3 - "$head" "$tree" "$remote" <<'PY'
import json, os, sys
print(json.dumps({
    "schemaVersion": 1,
    "status": "passed",
    "mode": os.environ.get("BLAZN_QUALIFICATION_MODE", "dry-run"),
    "correlationId": os.environ["BLAZN_QUALIFICATION_CORRELATION_ID"],
    "target": os.environ["BLAZN_QUALIFICATION_TARGET"],
    "profile": os.environ["BLAZN_QUALIFICATION_PROFILE"],
    "source": {"head": sys.argv[1], "tree": sys.argv[2], "remote": sys.argv[3]},
}, sort_keys=True))
PY
