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
printf '{"status":"passed","source":{"head":"0000000000000000000000000000000000000000"}}\n' >"$output/artifacts/source.stdout"
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

printf '{"status":"passed"}\n' >"$output/artifacts/asserted.stdout"
if "$qual_dir/evidence.py" record --output "$output" --step baseline-invariants \
  --stdout "$output/artifacts/asserted.stdout" --stderr "$output/artifacts/source.stderr" --exit-code 0 >/dev/null 2>&1; then
  printf 'manually asserted gate evidence was accepted\n' >&2
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
: >"$complete/artifacts/gate.stderr"
fresh_gates=(source-provenance baseline-invariants target-baseline ubuntu-preflight service-identity
  no-input-sudo-observe install idempotent-install repair expired-observe
  expired-repair-denied expired-uninstall install-crash-resume cleanup-crash-resume
  reinstall kubernetes-uid-rv kubernetes-stale-cas-denied
  kubernetes-quarantine-noschedule target-post-uninstall zero-residue post-invariants)
for gate in "${fresh_gates[@]}"; do
  case "$gate" in
    source-provenance) gate_json='{"status":"passed","source":{"head":"0000000000000000000000000000000000000000"}}' ;;
    baseline-invariants) gate_json='{"phase":"before","protected":{"units":[],"containers":[]}}' ;;
    post-invariants) gate_json='{"phase":"after","protected":{"units":[],"containers":[]}}' ;;
    target-baseline) gate_json='{"phase":"before","state":{"paths":"sha256:test"}}' ;;
    target-post-uninstall) gate_json='{"phase":"after","state":{"paths":"sha256:test"}}' ;;
    ubuntu-preflight) gate_json='{"os":"ubuntu","osVersion":"26.04"}' ;;
    service-identity) gate_json='{"service":{"accountUid":"1001","processUid":"1001"}}' ;;
    no-input-sudo-observe) gate_json='{"noInputRootObservation":"allowed"}' ;;
    install|idempotent-install|repair|reinstall) gate_json='{"state":"active","currentStage":"complete","residues":[]}' ;;
    expired-observe) gate_json='{"schemaVersion":"blazn.dev/node-root-helper/v1","ok":true,"observation":{"binding":{"clusterId":"test"}}}' ;;
    expired-repair-denied) gate_json='{"status":"passed","expiredRepairDenied":true,"denial":{"exitCode":1,"error":{"code":"node_failed"}}}' ;;
    expired-uninstall) gate_json='{"state":"removed","currentStage":"complete","residues":[]}' ;;
    install-crash-resume) gate_json='{"status":"passed","crash":{"lifecycle":"install"},"recovery":{"state":"active","residues":[]}}' ;;
    cleanup-crash-resume) gate_json='{"status":"passed","crash":{"lifecycle":"cleanup"},"recovery":{"state":"removed","residues":[]}}' ;;
    kubernetes-uid-rv) gate_json='{"node":{"uid":"uid-1","resourceVersion":"1"}}' ;;
    kubernetes-stale-cas-denied) gate_json='{"status":"passed","staleCASDenied":true,"stateUnchanged":true,"rejection":{"classification":"kubernetes-status-invalid-422-jsonpatch-test","reason":"Invalid","code":422}}' ;;
    kubernetes-quarantine-noschedule) gate_json='{"status":"passed","quarantineNoSchedule":true,"ordinaryWorkloads":0}' ;;
    zero-residue) gate_json='{"status":"passed","guestResidue":0,"kubernetesResidue":0,"protectedInvariants":true}' ;;
    *) printf 'missing test fixture for gate %s\n' "$gate" >&2; exit 1 ;;
  esac
  printf '%s\n' "$gate_json" >"$complete/artifacts/${gate}.stdout"
  "$qual_dir/evidence.py" record --output "$complete" --step "$gate" \
    --stdout "$complete/artifacts/${gate}.stdout" --stderr "$complete/artifacts/gate.stderr" --exit-code 0
done
"$qual_dir/evidence.py" finalize --output "$complete" >/dev/null
"$qual_dir/evidence.py" verify --output "$complete" >/dev/null

"$qual_dir/tests/test-guardrails.sh"
printf 'Node qualification static tests passed.\n'
