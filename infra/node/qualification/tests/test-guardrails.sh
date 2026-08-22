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

# shellcheck disable=SC2016
digest_one=$("${base_env[@]}" bash -c 'source "$1/lib/common.sh"; qual_validate_lxd_limits; qual_approval_input_digest lxd-create' _ "$qual_dir")
# shellcheck disable=SC2016
digest_two=$("${base_env[@]}" BLAZN_QUALIFICATION_LXD_CPU=5 bash -c 'source "$1/lib/common.sh"; qual_validate_lxd_limits; qual_approval_input_digest lxd-create' _ "$qual_dir")
[[ "$digest_one" =~ ^sha256:[0-9a-f]{64}$ ]] || { printf 'approval digest is malformed\n' >&2; exit 1; }
[ "$digest_one" != "$digest_two" ] || { printf 'approval digest did not bind LXD CPU\n' >&2; exit 1; }

bash -c 'source "$1/lib/common.sh"; qual_require_expired_repair_denial "$2"' _ "$qual_dir" \
  '{"error":{"code":"node_failed","message":"repair requires an authorized fresh, unexpired plan: expired"},"exitCode":1}'
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

printf 'Node qualification mutation guard tests passed.\n'
