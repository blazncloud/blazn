#!/bin/sh
set -eu

# Publishes reviewed OCI archives to the existing in-cluster registry using
# the in-cluster pull/push credential in place. Never prints credentials.
# Verifies the pushed digests equal the reviewed build-report digests.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../versions.env"
[ "$#" -eq 1 ] || { printf 'usage: %s BUILD_OUTPUT_DIR\n' "$0" >&2; exit 64; }
build_dir=$1
: "${BLAZN_EXPECTED_CONTROLLER_INDEX:?set the reviewed sandbox-controller index digest}"
: "${BLAZN_EXPECTED_SANDBOX_IO_INDEX:?set the reviewed sandbox-io index digest}"
: "${BLAZN_EXPECTED_SOURCE_COMMIT:?set the reviewed source commit}"
registry_secret_namespace=${BLAZN_REGISTRY_SECRET_NAMESPACE:-frontro-agent-runtime}
registry_secret_name=${BLAZN_REGISTRY_SECRET_NAME:-frontro-registry-pull}
controller_repository=${BLAZN_PUBLISH_CONTROLLER_REPOSITORY:-${BLAZN_CONTROLLER_REPOSITORY:?}}
sandbox_io_repository=${BLAZN_PUBLISH_SANDBOX_IO_REPOSITORY:-${BLAZN_SANDBOX_IO_REPOSITORY:?}}
for required in kubectl python3 sha256sum curl tar base64; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
[ -f "$build_dir/build-report.json" ] || { printf 'build report is missing\n' >&2; exit 1; }

umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-publish.XXXXXX")
cleanup() {
  find "$work" -xdev -type f -delete
  find "$work" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

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
fetch_tool "$CRANE_URL" "$CRANE_SHA256" "$work/crane.tgz"
tar -xzf "$work/crane.tgz" -C "$work" crane
chmod 0700 "$work/crane"

# Verify the reviewed report against both the sealed expectations and the
# actual archive bytes before anything leaves this host.
python3 - "$build_dir" "$BLAZN_EXPECTED_SOURCE_COMMIT" "$BLAZN_EXPECTED_CONTROLLER_INDEX" "$BLAZN_EXPECTED_SANDBOX_IO_INDEX" <<'PY'
import hashlib, json, os, sys
build_dir, commit, controller_index, sandbox_io_index = sys.argv[1:5]
report = json.load(open(os.path.join(build_dir, "build-report.json")))
assert report["schema"] == "blazn.dev/image-build-report/v1"
assert report["sourceCommit"] == commit, "report commit differs from the reviewed commit"
expected = {"sandbox-controller": controller_index, "sandbox-io": sandbox_io_index}
for name, index in expected.items():
    entry = report["images"][name]
    assert entry["index"] == index, f"{name} report index differs from the reviewed digest"
    digest = hashlib.sha256()
    with open(os.path.join(build_dir, f"{name}.oci.tar"), "rb") as archive:
        for chunk in iter(lambda: archive.read(1 << 20), b""):
            digest.update(chunk)
    assert digest.hexdigest() == entry["archiveSha256"], f"{name} archive bytes changed since the build"
print("archives verified against the reviewed report")
PY

docker_config=$work/docker
mkdir -m 0700 "$docker_config"
kubectl get secret "$registry_secret_name" -n "$registry_secret_namespace" -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d >"$docker_config/config.json"
chmod 0600 "$docker_config/config.json"
[ -s "$docker_config/config.json" ] || { printf 'registry credential secret is empty\n' >&2; exit 1; }

publish_one() {
  publish_name=$1; publish_repository=$2; publish_index=$3
  mkdir "$work/$publish_name.oci"
  tar -xf "$build_dir/$publish_name.oci.tar" -C "$work/$publish_name.oci"
  # crane push --index uploads every blob and manifest but tags a wrapper
  # index of its own; retag the exact reviewed index digest afterwards.
  DOCKER_CONFIG=$docker_config "$work/crane" push --insecure --index "$work/$publish_name.oci" "$publish_repository:upload-$BLAZN_EXPECTED_SOURCE_COMMIT" >/dev/null 2>&1
  DOCKER_CONFIG=$docker_config "$work/crane" tag --insecure "$publish_repository@$publish_index" "$BLAZN_EXPECTED_SOURCE_COMMIT" >/dev/null 2>&1 || { printf '%s reviewed index is not present in the registry after upload\n' "$publish_name" >&2; exit 1; }
  pushed=$(DOCKER_CONFIG=$docker_config "$work/crane" digest --insecure "$publish_repository:$BLAZN_EXPECTED_SOURCE_COMMIT")
  [ "$pushed" = "$publish_index" ] || { printf '%s pushed digest %s does not match the reviewed index\n' "$publish_name" "$pushed" >&2; exit 1; }
  DOCKER_CONFIG=$docker_config "$work/crane" manifest --insecure "$publish_repository@$publish_index" >"$work/$publish_name-remote-manifest.json"
  python3 - "$work/$publish_name-remote-manifest.json" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1]))
platforms = {f'{m["platform"]["os"]}/{m["platform"]["architecture"]}' for m in manifest["manifests"] if "platform" in m}
assert {"linux/amd64", "linux/arm64"} <= platforms, platforms
PY
  printf '%s published as %s@%s\n' "$publish_name" "$publish_repository" "$publish_index"
}
publish_one sandbox-controller "$controller_repository" "$BLAZN_EXPECTED_CONTROLLER_INDEX"
publish_one sandbox-io "$sandbox_io_repository" "$BLAZN_EXPECTED_SANDBOX_IO_INDEX"
printf 'publication complete and digest-verified\n'
