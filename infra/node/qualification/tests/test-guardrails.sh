#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

qual_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp_root=$(mktemp -d)
trap 'rm -rf -- "$tmp_root"' EXIT
fake_bin="$tmp_root/bin"
mkdir "$fake_bin"
calls="$tmp_root/calls"
: >"$calls"

cat >"$fake_bin/lxc" <<'EOF'
#!/bin/sh
printf 'lxc %s\n' "$*" >>"$QUAL_TEST_CALLS"
exit 0
EOF
cat >"$fake_bin/kubectl" <<'EOF'
#!/bin/sh
printf 'kubectl %s\n' "$*" >>"$QUAL_TEST_CALLS"
case "$*" in
  'config current-context') printf '%s\n' frontro-shared ;;
esac
exit 0
EOF
chmod +x "$fake_bin/lxc" "$fake_bin/kubectl"

base_env=(env PATH="$fake_bin:$PATH" QUAL_TEST_CALLS="$calls"
  BLAZN_QUALIFICATION_CORRELATION_ID=nodequal-static001
  BLAZN_QUALIFICATION_TARGET=blazn-q-static001
  BLAZN_QUALIFICATION_PROFILE=lxd-ubuntu-26.04
  BLAZN_QUALIFICATION_LXD_IMAGE_FINGERPRINT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa)

# shellcheck disable=SC2016
if env BLAZN_QUALIFICATION_CORRELATION_ID=nodequal-static001 BLAZN_QUALIFICATION_TARGET=ben4 BLAZN_QUALIFICATION_PROFILE=lxd-ubuntu-26.04 \
  bash -c 'source "$1/lib/common.sh"; qual_require_target' _ "$qual_dir" >/dev/null 2>&1; then
  printf 'ben4 target guard failed\n' >&2
  exit 1
fi

"${base_env[@]}" "$qual_dir/lxd-disposable.sh" plan >/dev/null
[ ! -s "$calls" ] || { printf 'LXD plan executed a command\n' >&2; exit 1; }

if "${base_env[@]}" BLAZN_QUALIFICATION_LXD_CPU=9 "$qual_dir/lxd-disposable.sh" plan >/dev/null 2>&1; then
  printf 'LXD plan accepted an excessive CPU limit\n' >&2
  exit 1
fi
if "${base_env[@]}" BLAZN_QUALIFICATION_LXD_MEMORY=17GiB "$qual_dir/lxd-disposable.sh" plan >/dev/null 2>&1; then
  printf 'LXD plan accepted an excessive memory limit\n' >&2
  exit 1
fi
if "${base_env[@]}" BLAZN_QUALIFICATION_LXD_ROOT_DISK=128GiB "$qual_dir/lxd-disposable.sh" plan >/dev/null 2>&1; then
  printf 'LXD plan accepted an excessive root disk limit\n' >&2
  exit 1
fi
if "${base_env[@]}" BLAZN_QUALIFICATION_LXD_PROCESSES=4096 "$qual_dir/lxd-disposable.sh" plan >/dev/null 2>&1; then
  printf 'LXD plan accepted an excessive process limit\n' >&2
  exit 1
fi

# shellcheck disable=SC2016
digest_one=$("${base_env[@]}" bash -c 'source "$1/lib/common.sh"; qual_validate_lxd_limits; qual_approval_input_digest lxd-create' _ "$qual_dir")
# shellcheck disable=SC2016
digest_two=$("${base_env[@]}" BLAZN_QUALIFICATION_LXD_CPU=5 bash -c 'source "$1/lib/common.sh"; qual_validate_lxd_limits; qual_approval_input_digest lxd-create' _ "$qual_dir")
[[ "$digest_one" =~ ^sha256:[0-9a-f]{64}$ ]] || { printf 'approval digest is malformed\n' >&2; exit 1; }
[ "$digest_one" != "$digest_two" ] || { printf 'approval digest did not bind LXD CPU\n' >&2; exit 1; }
for binding in \
  BLAZN_QUALIFICATION_INSTALL_PROFILE_SHA256=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  BLAZN_QUALIFICATION_EXPECTED_HOSTNAME=mac-mini-3 \
  BLAZN_QUALIFICATION_LOCK_IDENTITY=1:2:0:600 \
  BLAZN_QUALIFICATION_CRASH_TIMEOUT_SECONDS=301 \
  BLAZN_QUALIFICATION_SNAPSHOT_CONFIG_SHA256=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
  BLAZN_QUALIFICATION_LXD_ROOT_DISK=48GiB \
  BLAZN_QUALIFICATION_LXD_PROCESSES=1200; do
  # shellcheck disable=SC2016
  bound_digest=$("${base_env[@]}" "$binding" bash -c 'source "$1/lib/common.sh"; qual_validate_lxd_limits; qual_approval_input_digest lxd-create' _ "$qual_dir")
  [ "$bound_digest" != "$digest_one" ] || { printf 'approval digest omitted %s\n' "${binding%%=*}" >&2; exit 1; }
done

bash -c 'source "$1/lib/common.sh"; qual_require_expired_repair_denial "$2"' _ "$qual_dir" \
  '{"error":{"code":"node_failed","message":"repair requires an authorized fresh, unexpired plan: install plan is not active at trusted current time"},"exitCode":1}'
if bash -c 'source "$1/lib/common.sh"; qual_require_expired_repair_denial "$2"' _ "$qual_dir" \
  '{"error":{"code":"node_failed","message":"network unavailable"},"exitCode":1}' >/dev/null 2>&1; then
  printf 'unrelated repair failure passed expired-plan gate\n' >&2
  exit 1
fi
bash -c 'source "$1/lib/common.sh"; qual_require_stale_cas_rejection "$2"' _ "$qual_dir" \
  '{"kind":"Status","status":"Failure","reason":"Invalid","code":422,"message":"jsonpatch test operation does not apply to resourceVersion"}' >/dev/null
bash -c 'source "$1/lib/common.sh"; qual_require_stale_cas_rejection "$2"' _ "$qual_dir" \
  'Error from server (Invalid): jsonpatch test operation does not apply to resourceVersion' >/dev/null
if bash -c 'source "$1/lib/common.sh"; qual_require_stale_cas_rejection "$2"' _ "$qual_dir" \
  '{"kind":"Status","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}' >/dev/null 2>&1; then
  printf 'RBAC denial passed stale-CAS gate\n' >&2
  exit 1
fi
if bash -c 'source "$1/lib/common.sh"; qual_require_stale_cas_rejection "$2"' _ "$qual_dir" \
  '{"kind":"Status","status":"Failure","reason":"Invalid","code":422,"message":"unsupported media type"}' >/dev/null 2>&1; then
  printf 'generic Invalid response passed stale-CAS gate\n' >&2
  exit 1
fi

if "${base_env[@]}" BLAZN_QUALIFICATION_MODE=mutate "$qual_dir/lxd-disposable.sh" create >/dev/null 2>&1; then
  printf 'LXD create accepted missing approval\n' >&2
  exit 1
fi
[ ! -s "$calls" ] || { printf 'unapproved LXD create reached lxc\n' >&2; exit 1; }

if "${base_env[@]}" BLAZN_QUALIFICATION_MODE=mutate "$qual_dir/lifecycle.sh" repair >/dev/null 2>&1; then
  printf 'lifecycle accepted missing approval\n' >&2
  exit 1
fi
[ ! -s "$calls" ] || { printf 'unapproved lifecycle reached target\n' >&2; exit 1; }

if "${base_env[@]}" BLAZN_QUALIFICATION_KUBE_CONTEXT=frontro-shared \
  BLAZN_QUALIFICATION_KUBE_NODE=test BLAZN_QUALIFICATION_EXPECTED_NODE_UID=uid \
  BLAZN_QUALIFICATION_EXPECTED_RESOURCE_VERSION=1 "$qual_dir/kubernetes-checks.sh" inspect >/dev/null 2>&1; then
  printf 'shared Kubernetes context guard failed\n' >&2
  exit 1
fi

legacy_owner='King''Jammin'
if rg -n "${legacy_owner}/blazn|github\\.com/${legacy_owner}" "$qual_dir" >/dev/null; then
  printf 'old repository owner remains in qualification harness\n' >&2
  exit 1
fi
if rg -n 'runtime\.json' "$qual_dir/lifecycle.sh" >/dev/null || ! rg -n 'root_observation=\$\(target_daemon_observe\)' "$qual_dir/lifecycle.sh" >/dev/null; then
  printf 'expired-plan binding bypasses the receipt-authorized root observation surface\n' >&2
  exit 1
fi
for action in snapshot restore delete; do
  rg -F "action:\"${action}\"" "$qual_dir/lxd-disposable.sh" >/dev/null || { printf 'LXD %s lacks structured evidence\n' "$action" >&2; exit 1; }
done
restore_line=$(rg -n 'lxc restore "\$BLAZN_QUALIFICATION_TARGET"' "$qual_dir/lifecycle.sh" | cut -d: -f1)
verify_line=$(rg -n '^  verify_binary$' "$qual_dir/lifecycle.sh" | head -n1 | cut -d: -f1)
[ -n "$restore_line" ] && [ -n "$verify_line" ] && [ "$restore_line" -lt "$verify_line" ] || { printf 'crash lifecycle does not restore the approved snapshot before execution\n' >&2; exit 1; }
rg -F 'restoredUnderLifecycleLock:true' "$qual_dir/lifecycle.sh" >/dev/null || { printf 'crash evidence lacks locked snapshot restoration proof\n' >&2; exit 1; }

printf 'Node qualification mutation guard tests passed.\n'
