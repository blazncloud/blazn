#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
usage: run-live.sh --workspace WORKSPACE_ID [options]

Run the complete Blazn CLI-only development acceptance against the current
repository commit. WORKSPACE_ID may also be supplied as BLAZN_WORKSPACE_ID.

Options:
  --workspace UUID              Blazn workspace (required)
  --blazn PATH                  CLI executable (default: BLAZN_BIN or PATH)
  --source COMMIT               exact Git commit (default: current HEAD)
  --arch amd64|arm64            target architecture (default: amd64)
  --publish-template            publish the template before acceptance
  --patch-output PATH           durable patch destination
  -h, --help                    show this help
EOF
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
workspace=${BLAZN_WORKSPACE_ID:-}
blazn=${BLAZN_BIN:-}
source_commit=
architecture=
publish_template=0
patch_output=${BLAZN_DEVELOPMENT_PATCH_OUTPUT:-}

while [ "$#" -gt 0 ]; do
  case $1 in
    --workspace) [ "$#" -ge 2 ] || { printf '%s\n' '--workspace requires a value' >&2; exit 64; }; workspace=$2; shift 2 ;;
    --blazn) [ "$#" -ge 2 ] || { printf '%s\n' '--blazn requires a value' >&2; exit 64; }; blazn=$2; shift 2 ;;
    --source) [ "$#" -ge 2 ] || { printf '%s\n' '--source requires a value' >&2; exit 64; }; source_commit=$2; shift 2 ;;
    --arch) [ "$#" -ge 2 ] || { printf '%s\n' '--arch requires a value' >&2; exit 64; }; architecture=$2; shift 2 ;;
    --publish-template) publish_template=1; shift ;;
    --patch-output) [ "$#" -ge 2 ] || { printf '%s\n' '--patch-output requires a value' >&2; exit 64; }; patch_output=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown option: %s\n' "$1" >&2; usage >&2; exit 64 ;;
  esac
done

[ -n "$workspace" ] || { printf '%s\n' 'workspace is required: pass --workspace UUID or set BLAZN_WORKSPACE_ID' >&2; exit 64; }

if [ -z "$blazn" ]; then
  blazn=$(command -v blazn || true)
fi
[ -n "$blazn" ] && [ -x "$blazn" ] || { printf '%s\n' 'Blazn CLI is not executable; pass --blazn PATH or add blazn to PATH' >&2; exit 1; }

command -v git >/dev/null 2>&1 || { printf '%s\n' 'git is required' >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { printf '%s\n' 'jq is required' >&2; exit 1; }
command -v mktemp >/dev/null 2>&1 || { printf '%s\n' 'mktemp is required' >&2; exit 1; }

if [ -z "$source_commit" ]; then
  if [ -n "$(git -C "$repo_root" status --porcelain --untracked-files=normal)" ]; then
    printf '%s\n' 'working tree is dirty or has untracked files; commit/push it or pass --source EXACT_ORIGIN_COMMIT' >&2
    exit 1
  fi
  source_commit=$(git -C "$repo_root" rev-parse --verify HEAD)
fi
if [ "${BLAZN_SOURCE_PREFLIGHT_FETCH:-1}" = 1 ]; then
  git -C "$repo_root" fetch --quiet --no-tags origin || {
    printf '%s\n' 'unable to refresh origin before source preflight' >&2
    exit 1
  }
fi
git -C "$repo_root" cat-file -e "$source_commit^{commit}" 2>/dev/null || {
  printf 'source commit is not present locally: %s\n' "$source_commit" >&2
  exit 1
}
origin_ref=$(git -C "$repo_root" for-each-ref --format='%(refname)' --contains "$source_commit" refs/remotes/origin/ | head -1)
if [ -z "$origin_ref" ]; then
  remote_refs=$(git -C "$repo_root" ls-remote --heads --tags origin) || {
    printf '%s\n' 'unable to inspect origin refs during source preflight' >&2
    exit 1
  }
  printf '%s\n' "$remote_refs" | awk -v commit="$source_commit" '$1 == commit { found=1 } END { exit found ? 0 : 1 }' || {
    printf 'source commit is not reachable from a known origin ref or exact remote tip; push it first: %s\n' "$source_commit" >&2
    exit 1
  }
fi

if [ -z "$architecture" ]; then
  architecture=amd64
fi

template_file=$repo_root/examples/coding-agent/sandbox-template-dev.yaml
template_name=$(jq -er '.metadata.name' "$template_file")
template_version=$(jq -er '.spec.version' "$template_file")
template_reference=$template_name@$template_version
runner=${BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER:-$script_dir/test-live.sh}
[ -x "$runner" ] || { printf 'development acceptance runner is not executable: %s\n' "$runner" >&2; exit 1; }

printf 'Blazn development MVP: commit=%s architecture=%s template=%s\n' \
  "$source_commit" "$architecture" "$template_reference"

if [ -z "$patch_output" ]; then
  patch_default_dir=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-output.XXXXXX")
  patch_output=$patch_default_dir/change-${source_commit}.patch
  export BLAZN_DEVELOPMENT_PATCH_DEFAULT_DIR="$patch_default_dir"
fi
export BLAZN_DEVELOPMENT_PATCH_OUTPUT="$patch_output"

if [ "$publish_template" -eq 1 ]; then
  BLAZN_SOURCE_PREFLIGHT_FETCH=0 BLAZN_SKIP_TEMPLATE_PUBLISH=0 \
    exec "$runner" "$blazn" "$workspace" "$template_file" "$template_reference" "$source_commit" "$architecture"
fi
BLAZN_SOURCE_PREFLIGHT_FETCH=0 BLAZN_SKIP_TEMPLATE_PUBLISH=1 \
  exec "$runner" "$blazn" "$workspace" "$template_file" "$template_reference" "$source_commit" "$architecture"
