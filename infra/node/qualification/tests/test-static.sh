#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

qual_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd -P)
repo_root=$(unset CDPATH; cd -- "$qual_dir/../../.." && pwd -P)
tmp_root=$(mktemp -d)
trap 'rm -rf -- "$tmp_root"' EXIT

command -v shellcheck >/dev/null 2>&1 || { printf 'shellcheck is required\n' >&2; exit 1; }
find "$qual_dir" -type f -name '*.sh' -print0 | xargs -0 shellcheck -S style -x
PYTHONPYCACHEPREFIX="$tmp_root/pycache" python3 -m py_compile "$qual_dir/evidence.py" "$qual_dir/compare-invariants.py" "$qual_dir/compare-target-state.py"
python3 -m json.tool "$qual_dir/schemas/qualification-run.schema.json" >/dev/null
python3 -m json.tool "$qual_dir/schemas/inventory.schema.json" >/dev/null

output="$tmp_root/evidence"
fake_binary="$tmp_root/blazn"
cat >"$fake_binary" <<'EOF'
#!/bin/sh
printf '{"version":"v0.0.0-qualification","commit":"0000000000000000000000000000000000000000"}\n'
EOF
chmod +x "$fake_binary"
export BLAZN_QUALIFICATION_CORRELATION_ID=nodequal-static001
export BLAZN_QUALIFICATION_TARGET=blazn-q-static001
export BLAZN_QUALIFICATION_PROFILE=lxd-ubuntu-26.04
"$qual_dir/evidence.py" init --output "$output" --repo "$repo_root" --scope fresh-linux --binary "$fake_binary"
printf '{}\n' >"$output/artifacts/source.stdout"
: >"$output/artifacts/source.stderr"
"$qual_dir/evidence.py" record --output "$output" --step source-provenance \
  --stdout "$output/artifacts/source.stdout" --stderr "$output/artifacts/source.stderr" --exit-code 0
if "$qual_dir/evidence.py" finalize --output "$output" >/dev/null 2>&1; then
  printf 'incomplete evidence finalized successfully\n' >&2
  exit 1
fi
printf '{"accessToken":"secret"}\n' >"$output/artifacts/secret.stdout"
if "$qual_dir/evidence.py" record --output "$output" --step baseline-invariants \
  --stdout "$output/artifacts/secret.stdout" --stderr "$output/artifacts/source.stderr" --exit-code 0 >/dev/null 2>&1; then
  printf 'credential-bearing evidence was accepted\n' >&2
  exit 1
fi

complete="$tmp_root/complete"
clean_repo="$tmp_root/repo"
mkdir "$clean_repo"
git -C "$clean_repo" init -q
git -C "$clean_repo" config user.name qualification-test
git -C "$clean_repo" config user.email qualification-test@invalid
git -C "$clean_repo" remote add origin https://github.com/blazncloud/blazn.git
printf 'fixture\n' >"$clean_repo/fixture"
git -C "$clean_repo" add fixture
git -C "$clean_repo" commit -qm fixture
"$qual_dir/evidence.py" init --output "$complete" --repo "$clean_repo" --scope fresh-linux --binary "$fake_binary"
printf '{"status":"passed"}\n' >"$complete/artifacts/gate.stdout"
: >"$complete/artifacts/gate.stderr"
fresh_gates=(source-provenance baseline-invariants target-baseline ubuntu-preflight service-identity
  no-input-sudo-observe install idempotent-install repair expired-observe
  expired-repair-denied expired-uninstall install-crash-resume cleanup-crash-resume
  reinstall kubernetes-uid-rv kubernetes-stale-cas-denied
  kubernetes-quarantine-noschedule target-post-uninstall zero-residue post-invariants)
for gate in "${fresh_gates[@]}"; do
  "$qual_dir/evidence.py" record --output "$complete" --step "$gate" \
    --stdout "$complete/artifacts/gate.stdout" --stderr "$complete/artifacts/gate.stderr" --exit-code 0
done
"$qual_dir/evidence.py" finalize --output "$complete" >/dev/null
"$qual_dir/evidence.py" verify --output "$complete" >/dev/null

"$qual_dir/tests/test-guardrails.sh"
printf 'Node qualification static tests passed.\n'
