#!/bin/sh
set -eu

# Builds the linux/amd64 + linux/arm64 blazn-sandbox-controller and
# blazn-sandbox-io images from a clean checkout of a reviewed commit into
# content-addressed OCI archives, scans them, and records every digest.
# Non-mutating outside OUTPUT_DIR; never pushes.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../versions.env"
[ "$#" -eq 2 ] || { printf 'usage: %s SOURCE_DIR OUTPUT_DIR\n' "$0" >&2; exit 64; }
source_dir=$1
output_dir=$2
: "${BLAZN_EXPECTED_SOURCE_COMMIT:?set the reviewed source commit}"
for required in docker git python3 sha256sum curl tar; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
docker buildx version >/dev/null 2>&1 || { printf 'docker buildx is required (pinned plugin: %s)\n' "$BUILDX_URL" >&2; exit 1; }
[ -d "$source_dir/.git" ] || { printf 'source directory is not a git checkout\n' >&2; exit 1; }
head_commit=$(git -C "$source_dir" rev-parse HEAD)
[ "$head_commit" = "$BLAZN_EXPECTED_SOURCE_COMMIT" ] || { printf 'source checkout is %s, not the reviewed commit\n' "$head_commit" >&2; exit 1; }
[ -z "$(git -C "$source_dir" status --porcelain)" ] || { printf 'source checkout is not clean\n' >&2; exit 1; }
[ ! -e "$output_dir" ] || { printf 'output directory already exists\n' >&2; exit 1; }
mkdir -m 0700 "$output_dir" "$output_dir/tools"

fetch_tool() {
  fetch_url=$1; fetch_sha=$2; fetch_out=$3
  attempt=0
  while :; do
    attempt=$((attempt + 1))
    if curl --fail --silent --show-error --location --output "$fetch_out" "$fetch_url"; then
      [ "$(sha256sum "$fetch_out" | awk '{print $1}')" = "$fetch_sha" ] || { printf 'checksum mismatch for %s\n' "$fetch_url" >&2; exit 1; }
      return 0
    fi
    [ "$attempt" -lt 3 ] || { printf 'could not download %s\n' "$fetch_url" >&2; exit 1; }
    sleep 5
  done
}
fetch_tool "$TRIVY_URL" "$TRIVY_SHA256" "$output_dir/tools/trivy.tgz"
tar -xzf "$output_dir/tools/trivy.tgz" -C "$output_dir/tools" trivy
chmod 0700 "$output_dir/tools/trivy"

builder=blazn-image-pipeline-$$
docker buildx create --name "$builder" --driver docker-container --bootstrap >/dev/null
cleanup() { docker buildx rm --force "$builder" >/dev/null 2>&1 || :; }
trap cleanup EXIT HUP INT TERM

build_one() {
  build_name=$1; build_dockerfile=$2
  docker buildx build --builder "$builder" \
    --platform linux/amd64,linux/arm64 \
    --provenance=false --sbom=false \
    -f "$source_dir/$build_dockerfile" \
    --metadata-file "$output_dir/$build_name-metadata.json" \
    --output "type=oci,dest=$output_dir/$build_name.oci.tar" \
    "$source_dir" >"$output_dir/$build_name-build.log" 2>&1 || { tail -30 "$output_dir/$build_name-build.log" >&2; printf '%s build failed\n' "$build_name" >&2; exit 1; }
  mkdir "$output_dir/$build_name.oci"
  tar -xf "$output_dir/$build_name.oci.tar" -C "$output_dir/$build_name.oci"
  "$output_dir/tools/trivy" image --input "$output_dir/$build_name.oci" \
    --severity CRITICAL --exit-code 1 --no-progress --quiet \
    --format json --output "$output_dir/$build_name-scan.json" || { printf '%s has CRITICAL findings; see %s-scan.json\n' "$build_name" "$build_name" >&2; exit 1; }
}
build_one sandbox-controller Dockerfile.sandbox-controller
build_one sandbox-io Dockerfile.sandbox-io

python3 - "$output_dir" "$BLAZN_EXPECTED_SOURCE_COMMIT" <<'PY'
import hashlib, json, os, sys
output_dir, commit = sys.argv[1], sys.argv[2]
report = {"schema": "blazn.dev/image-build-report/v1", "sourceCommit": commit, "images": {}}
for name in ("sandbox-controller", "sandbox-io"):
    metadata = json.load(open(os.path.join(output_dir, f"{name}-metadata.json")))
    index_digest = metadata["containerimage.digest"]
    layout = os.path.join(output_dir, f"{name}.oci")
    index = json.load(open(os.path.join(layout, "index.json")))
    manifests = index["manifests"]
    assert len(manifests) == 1 and manifests[0]["digest"] == index_digest, "layout root does not match the reported index"
    algo, hexdigest = index_digest.split(":")
    child_index = json.load(open(os.path.join(layout, "blobs", algo, hexdigest)))
    children = {}
    for manifest in child_index["manifests"]:
        platform = manifest.get("platform", {})
        key = f'{platform.get("os")}/{platform.get("architecture")}'
        children[key] = manifest["digest"]
    assert set(children) == {"linux/amd64", "linux/arm64"}, children
    digest = hashlib.sha256()
    with open(os.path.join(output_dir, f"{name}.oci.tar"), "rb") as archive:
        for chunk in iter(lambda: archive.read(1 << 20), b""):
            digest.update(chunk)
    report["images"][name] = {
        "index": index_digest,
        "linux/amd64": children["linux/amd64"],
        "linux/arm64": children["linux/arm64"],
        "archiveSha256": digest.hexdigest(),
    }
with open(os.path.join(output_dir, "build-report.json"), "w") as out:
    json.dump(report, out, indent=2, sort_keys=True)
    out.write("\n")
print(json.dumps(report["images"], indent=2, sort_keys=True))
PY
printf 'image build, scan, and digest capture complete\n'
