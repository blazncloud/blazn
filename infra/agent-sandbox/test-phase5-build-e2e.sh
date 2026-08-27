#!/bin/sh
set -eu

# End-to-end proof of the Phase 5 image pipeline: build both multi-platform
# images from this exact checkout, scan them, publish them to a disposable
# local registry through the real publish script (with a fake in-cluster
# credential source), verify remote digests, and prove tampered archives are
# refused. Never touches a real registry or cluster.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH='' cd -- "$ROOT/../.." && pwd)
for required in docker git python3 curl tar; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
docker buildx version >/dev/null 2>&1 || { printf 'docker buildx is required\n' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-build-e2e.XXXXXX")
registry_container=''
cleanup() {
  if [ -n "$registry_container" ]; then docker rm -f "$registry_container" >/dev/null 2>&1 || :; fi
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

commit=$(git -C "$REPO" rev-parse HEAD)
BLAZN_EXPECTED_SOURCE_COMMIT=$commit "$ROOT/phase5-build/build-images.sh" "$REPO" "$tmp/out"
[ -f "$tmp/out/build-report.json" ]
controller_index=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["images"]["sandbox-controller"]["index"])' "$tmp/out/build-report.json")
sandbox_io_index=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["images"]["sandbox-io"]["index"])' "$tmp/out/build-report.json")
case "$controller_index" in sha256:????????????????????????????????????????????????????????????????) ;; *) printf 'bad controller index digest\n' >&2; exit 1 ;; esac
[ "$controller_index" != "$sandbox_io_index" ]

# A build that claims a different source commit must be refused.
if BLAZN_EXPECTED_SOURCE_COMMIT=0000000000000000000000000000000000000000 "$ROOT/phase5-build/build-images.sh" "$REPO" "$tmp/reject" 2>"$tmp/reject.err"; then
  printf 'wrong-commit build was accepted\n' >&2; exit 1
fi
grep -Fq 'not the reviewed commit' "$tmp/reject.err"

registry_container=$(docker run -d --rm -p 127.0.0.1:5001:5000 "registry:3@sha256:1be55279f18a2fe1a74edf2664cac61c1bea305b7b4642dab412e7affdcb3e33")
attempt=0
until curl -fs http://127.0.0.1:5001/v2/ >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -le 30 ] || { printf 'local registry did not start\n' >&2; exit 1; }
  sleep 1
done

# The publish script must consume the credential through kubectl only.
mkdir -m 0700 "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
[ "$*" = 'get secret frontro-registry-pull -n frontro-agent-runtime -o jsonpath={.data.\.dockerconfigjson}' ] || { printf 'unexpected kubectl invocation: %s\n' "$*" >&2; exit 1; }
printf '%s' '{"auths":{"127.0.0.1:5001":{"auth":"dGVzdDp0ZXN0"}}}' | base64 | tr -d '\n'
EOF
chmod 0700 "$tmp/bin/kubectl"

publish() {
  env PATH="$tmp/bin:$PATH" \
    BLAZN_EXPECTED_SOURCE_COMMIT="$commit" \
    BLAZN_EXPECTED_CONTROLLER_INDEX="$controller_index" \
    BLAZN_EXPECTED_SANDBOX_IO_INDEX="$sandbox_io_index" \
    BLAZN_PUBLISH_CONTROLLER_REPOSITORY=127.0.0.1:5001/blazn/sandbox-controller \
    BLAZN_PUBLISH_SANDBOX_IO_REPOSITORY=127.0.0.1:5001/blazn/sandbox-io \
    "$ROOT/phase5-build/publish-images.sh" "$tmp/out"
}
publish
curl -fs http://127.0.0.1:5001/v2/blazn/sandbox-controller/manifests/"$commit" -H 'Accept: application/vnd.oci.image.index.v1+json' -o "$tmp/remote-index.json"
python3 - "$tmp/remote-index.json" <<'PY'
import json, sys
index = json.load(open(sys.argv[1]))
platforms = {f'{m["platform"]["os"]}/{m["platform"]["architecture"]}' for m in index["manifests"] if "platform" in m}
assert {"linux/amd64", "linux/arm64"} <= platforms, platforms
PY

# Tampered archives must be refused before anything is pushed.
printf 'tamper' >>"$tmp/out/sandbox-io.oci.tar"
if publish 2>"$tmp/tamper.err"; then printf 'tampered archive was published\n' >&2; exit 1; fi
grep -Fq 'archive bytes changed' "$tmp/tamper.err"

printf 'Phase 5 image pipeline end-to-end proof passed\n'
