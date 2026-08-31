#!/bin/sh
set -eu

# Builds and scans a worker-only multi-architecture OCI foundation. It never
# downloads, invents, or packages Hermes, and it never pushes.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../versions.env"
[ "$#" -eq 2 ] || { printf 'usage: %s SOURCE_DIR OUTPUT_DIR\n' "$0" >&2; exit 64; }
source_dir=$1
output_dir=$2
: "${BLAZN_EXPECTED_SOURCE_COMMIT:?set the reviewed source commit}"
for required in docker git python3 sha256sum curl tar; do
  command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }
done
docker buildx version >/dev/null 2>&1 || { printf 'docker buildx is required (pinned plugin: %s)\n' "$BUILDX_URL" >&2; exit 1; }
[ -d "$source_dir/.git" ] || { printf 'source directory is not a git checkout\n' >&2; exit 1; }
head_commit=$(git -C "$source_dir" rev-parse HEAD)
[ "$head_commit" = "$BLAZN_EXPECTED_SOURCE_COMMIT" ] || { printf 'source checkout is %s, not the reviewed commit\n' "$head_commit" >&2; exit 1; }
[ -z "$(git -C "$source_dir" status --porcelain)" ] || { printf 'source checkout is not clean\n' >&2; exit 1; }
[ ! -e "$output_dir" ] || { printf 'output directory already exists\n' >&2; exit 1; }
mkdir -m 0700 "$output_dir" "$output_dir/tools"

fetch_tool() {
  fetch_url=$1
  fetch_sha=$2
  fetch_out=$3
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

builder=blazn-harness-worker-foundation-$$
docker buildx create --name "$builder" --driver docker-container --bootstrap >/dev/null
cleanup() { docker buildx rm --force "$builder" >/dev/null 2>&1 || :; }
trap cleanup EXIT HUP INT TERM

name=harness-worker-foundation
docker buildx build --builder "$builder" \
  --platform linux/amd64,linux/arm64 \
  --provenance=false --sbom=false \
  -f "$source_dir/Dockerfile.harness-worker-foundation" \
  --metadata-file "$output_dir/$name-metadata.json" \
  --output "type=oci,dest=$output_dir/$name.oci.tar" \
  "$source_dir" >"$output_dir/$name-build.log" 2>&1 || {
    tail -30 "$output_dir/$name-build.log" >&2
    printf '%s build failed\n' "$name" >&2
    exit 1
  }
mkdir "$output_dir/$name.oci"
tar -xf "$output_dir/$name.oci.tar" -C "$output_dir/$name.oci"

for scan_arch in amd64 arm64; do
  python3 - "$output_dir/$name.oci" "$output_dir/$name-$scan_arch.oci" "$scan_arch" <<'PY'
import json, os, shutil, sys
layout, view, arch = sys.argv[1:4]
index = json.load(open(os.path.join(layout, "index.json")))
algo, hexdigest = index["manifests"][0]["digest"].split(":")
child_index = json.load(open(os.path.join(layout, "blobs", algo, hexdigest)))
child = next(m for m in child_index["manifests"] if m.get("platform", {}).get("architecture") == arch)
shutil.copytree(layout, view)
json.dump({"schemaVersion": 2, "manifests": [child]}, open(os.path.join(view, "index.json"), "w"))
PY
  "$output_dir/tools/trivy" image --input "$output_dir/$name-$scan_arch.oci" \
    --severity CRITICAL --exit-code 1 --no-progress --quiet \
    --format json --output "$output_dir/$name-$scan_arch-scan.json" || {
      printf '%s linux/%s has CRITICAL findings\n' "$name" "$scan_arch" >&2
      exit 1
    }
done

python3 - "$output_dir" "$BLAZN_EXPECTED_SOURCE_COMMIT" <<'PY'
import hashlib, json, os, sys
output_dir, commit = sys.argv[1:3]
name = "harness-worker-foundation"
metadata = json.load(open(os.path.join(output_dir, f"{name}-metadata.json")))
index_digest = metadata["containerimage.digest"]
layout = os.path.join(output_dir, f"{name}.oci")
index = json.load(open(os.path.join(layout, "index.json")))
assert len(index["manifests"]) == 1 and index["manifests"][0]["digest"] == index_digest
algo, hexdigest = index_digest.split(":")
child_index = json.load(open(os.path.join(layout, "blobs", algo, hexdigest)))
children = {
    f'{m.get("platform", {}).get("os")}/{m.get("platform", {}).get("architecture")}': m["digest"]
    for m in child_index["manifests"]
}
assert set(children) == {"linux/amd64", "linux/arm64"}, children
archive_digest = hashlib.sha256()
with open(os.path.join(output_dir, f"{name}.oci.tar"), "rb") as archive:
    for chunk in iter(lambda: archive.read(1 << 20), b""):
        archive_digest.update(chunk)
report = {
    "schema": "blazn.dev/harness-worker-foundation-build-report/v1",
    "sourceCommit": commit,
    "hermesIncluded": False,
    "runnable": False,
    "remainingGate": "approved multi-architecture Hermes artifact and final-image qualification",
    "image": {
        "index": index_digest,
        "linux/amd64": children["linux/amd64"],
        "linux/arm64": children["linux/arm64"],
        "archiveSha256": archive_digest.hexdigest(),
    },
}
with open(os.path.join(output_dir, "build-report.json"), "w") as out:
    json.dump(report, out, indent=2, sort_keys=True)
    out.write("\n")
print(json.dumps(report, indent=2, sort_keys=True))
PY
printf 'worker-only foundation build, scan, and digest capture complete; Hermes remains gated\n'
