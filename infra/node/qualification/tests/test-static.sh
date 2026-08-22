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
jq -n --arg head "$(git -C "$repo_root" rev-parse HEAD)" --arg tree "$(git -C "$repo_root" rev-parse 'HEAD^{tree}')" \
  '{status:"passed",source:{head:$head,tree:$tree,remote:"https://github.com/blazncloud/blazn.git"}}' >"$output/artifacts/source.stdout"
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

printf '{"phase":"before","correlationId":"nodequal-static001","source":{"head":"ffffffffffffffffffffffffffffffffffffffff","tree":"ffffffffffffffffffffffffffffffffffffffff"},"protected":{}}\n' >"$output/artifacts/mismatched-source.stdout"
if "$qual_dir/evidence.py" record --output "$output" --step baseline-invariants \
  --stdout "$output/artifacts/mismatched-source.stdout" --stderr "$output/artifacts/source.stderr" --exit-code 0 >/dev/null 2>&1; then
  printf 'source-mismatched inventory evidence was accepted\n' >&2
  exit 1
fi

printf '{"status":"passed","qualificationApprovalInputDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","result":{"state":"active","currentStage":"complete","residues":[]}}\n' >"$output/artifacts/synthetic-receipt.stdout"
if "$qual_dir/evidence.py" record --output "$output" --step adopt-install \
  --stdout "$output/artifacts/synthetic-receipt.stdout" --stderr "$output/artifacts/source.stderr" --exit-code 0 >/dev/null 2>&1; then
  printf 'partial synthetic adopt receipt was accepted\n' >&2
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
clean_head=$(git -C "$clean_repo" rev-parse HEAD)
clean_tree=$(git -C "$clean_repo" rev-parse 'HEAD^{tree}')
binary_digest="sha256:$(shasum -a 256 "$fake_binary" | awk '{print $1}')"
approval_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
signature=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
active_receipt=$(jq -nc --arg binary "$binary_digest" --arg signature "$signature" '{schemaVersion:"nodes/v1alpha1",receiptId:"11111111-1111-4111-8111-111111111111",planId:"22222222-2222-4222-8222-222222222222",planDigest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",nodeId:"33333333-3333-4333-8333-333333333333",state:"active",currentStage:"complete",residues:[],mutations:[{status:"applied"}],binary:{digest:$binary},digest:"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",signingKeyId:"node-key",signature:$signature}')
removed_receipt=$(jq -nc --argjson receipt "$active_receipt" '$receipt | .state="removed" | .mutations[0].status="removed"')
fresh_gates=(source-provenance baseline-invariants lxd-create target-baseline ubuntu-preflight service-identity
  no-input-sudo-observe install idempotent-install repair expired-observe
  expired-repair-denied expired-uninstall install-crash-resume cleanup-crash-resume
  reinstall kubernetes-uid-rv kubernetes-stale-cas-denied
  kubernetes-quarantine-noschedule target-post-uninstall zero-residue post-invariants)
for gate in "${fresh_gates[@]}"; do
  case "$gate" in
    source-provenance) gate_json=$(jq -nc --arg head "$clean_head" --arg tree "$clean_tree" '{status:"passed",source:{head:$head,tree:$tree,remote:"https://github.com/blazncloud/blazn.git"}}') ;;
    baseline-invariants|post-invariants) phase=before; [ "$gate" = post-invariants ] && phase=after; gate_json=$(jq -nc --arg phase "$phase" --arg head "$clean_head" --arg tree "$clean_tree" '{phase:$phase,correlationId:"nodequal-static001",source:{head:$head,tree:$tree},protected:{units:[{name:"homeai.service"}],containers:[{name:"homeai"}]}}') ;;
    target-baseline|target-post-uninstall) phase=before; [ "$gate" = target-post-uninstall ] && phase=after; gate_json=$(jq -nc --arg phase "$phase" --arg head "$clean_head" --arg tree "$clean_tree" '{phase:$phase,correlationId:"nodequal-static001",target:"blazn-q-static001",source:{head:$head,tree:$tree},state:{paths:"sha256:test"}}') ;;
    ubuntu-preflight) gate_json='{"os":"ubuntu","osVersion":"26.04"}' ;;
    lxd-create) gate_json=$(jq -nc --arg digest "$approval_digest" '{status:"passed",qualificationApprovalInputDigest:$digest,target:"blazn-q-static001",imageFingerprintDigest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",limits:{cpu:"4",memory:"8GiB",rootDisk:"32GiB",processes:"1024"}}') ;;
    service-identity) gate_json='{"service":{"accountUid":"1001","processUid":"1001"}}' ;;
    no-input-sudo-observe) gate_json='{"noInputRootObservation":"allowed"}' ;;
    install|idempotent-install|repair|reinstall) gate_json=$(jq -nc --arg digest "$approval_digest" --argjson result "$active_receipt" '{status:"passed",qualificationApprovalInputDigest:$digest,result:$result}') ;;
    expired-observe) gate_json='{"schemaVersion":"blazn.dev/node-root-helper/v1","ok":true,"observation":{"binding":{"clusterId":"test"}}}' ;;
    expired-repair-denied) gate_json=$(jq -nc --arg digest "$approval_digest" --arg signature "$signature" '{status:"passed",qualificationApprovalInputDigest:$digest,expiredRepairDenied:true,denial:{exitCode:1,error:{code:"node_failed",message:"repair requires an authorized fresh, unexpired plan: install plan is not active at trusted current time"}},signedPlan:{expiresAt:"2026-08-22T00:00:00Z",planId:"22222222-2222-4222-8222-222222222222",digest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",signature:$signature}}') ;;
    expired-uninstall) gate_json=$(jq -nc --arg digest "$approval_digest" --argjson result "$removed_receipt" '{status:"passed",qualificationApprovalInputDigest:$digest,result:$result}') ;;
    install-crash-resume) gate_json=$(jq -nc --arg digest "$approval_digest" --argjson recovery "$active_receipt" '{status:"passed",qualificationApprovalInputDigest:$digest,crash:{lifecycle:"install"},recovery:$recovery}') ;;
    cleanup-crash-resume) gate_json=$(jq -nc --arg digest "$approval_digest" --argjson recovery "$removed_receipt" '{status:"passed",qualificationApprovalInputDigest:$digest,crash:{lifecycle:"cleanup"},recovery:$recovery}') ;;
    kubernetes-uid-rv) gate_json='{"node":{"uid":"uid-1","resourceVersion":"1"}}' ;;
    kubernetes-stale-cas-denied) gate_json=$(jq -nc --arg digest "$approval_digest" '{status:"passed",qualificationApprovalInputDigest:$digest,staleCASDenied:true,stateUnchanged:true,rejection:{classification:"kubernetes-status-invalid-422-jsonpatch-test",reason:"Invalid",code:422}}') ;;
    kubernetes-quarantine-noschedule) gate_json='{"status":"passed","quarantineNoSchedule":true,"ordinaryWorkloads":0}' ;;
    zero-residue) gate_json='{"status":"passed","guestResidue":0,"kubernetesResidue":0,"protectedInvariants":true}' ;;
    *) printf 'missing test fixture for gate %s\n' "$gate" >&2; exit 1 ;;
  esac
  printf '%s\n' "$gate_json" >"$complete/artifacts/${gate}.stdout"
  "$qual_dir/evidence.py" record --output "$complete" --step "$gate" \
    --stdout "$complete/artifacts/${gate}.stdout" --stderr "$complete/artifacts/gate.stderr" --exit-code 0
done
printf '%s\n' "$(jq -nc --arg digest "$approval_digest" --argjson result "$active_receipt" '{status:"passed",qualificationApprovalInputDigest:$digest,result:$result}')" >"$complete/artifacts/adopt-install.stdout"
"$qual_dir/evidence.py" record --output "$complete" --step adopt-install \
  --stdout "$complete/artifacts/adopt-install.stdout" --stderr "$complete/artifacts/gate.stderr" --exit-code 0
"$qual_dir/evidence.py" finalize --output "$complete" >/dev/null
"$qual_dir/evidence.py" verify --output "$complete" >/dev/null
post_artifact="$complete/artifacts/post-invariants.stdout"
jq '.protected.containers[0].name="changed-homeai"' "$post_artifact" >"$post_artifact.tmp"
mv "$post_artifact.tmp" "$post_artifact"
post_digest="sha256:$(shasum -a 256 "$post_artifact" | awk '{print $1}')"
post_bytes=$(wc -c <"$post_artifact" | tr -d ' ')
jq --arg digest "$post_digest" --argjson bytes "$post_bytes" '(.steps[] | select(.id=="post-invariants").stdout) |= (.digest=$digest | .bytes=$bytes)' "$complete/run.json" >"$complete/run.json.tmp"
mv "$complete/run.json.tmp" "$complete/run.json"
if "$qual_dir/evidence.py" verify --output "$complete" >/dev/null 2>&1; then
  printf 'cross-inventory drift finalized after descriptor rehash\n' >&2
  exit 1
fi

"$qual_dir/tests/test-guardrails.sh"
printf 'Node qualification static tests passed.\n'
