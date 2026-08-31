#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH='' cd -- "$ROOT/../../.." && pwd)
dockerfile=$REPO/Dockerfile.harness-worker-foundation
builder=$ROOT/build-foundation.sh

for file in "$dockerfile" "$builder" "$ROOT/README.md"; do
  [ -f "$file" ] || { printf 'missing %s\n' "$file" >&2; exit 1; }
done
grep -Fq 'golang:1.26.2-bookworm@sha256:' "$dockerfile"
grep -Eq '^# syntax=docker/dockerfile:1\.7@sha256:[0-9a-f]{64}$' "$dockerfile"
grep -Fq "CGO_ENABLED=0 GOOS=\$TARGETOS GOARCH=\$TARGETARCH" "$dockerfile"
grep -Fq 'COPY --from=build --chmod=0555 /out/blazn-harness-worker /opt/blazn/blazn-harness-worker' "$dockerfile"
grep -Fq 'dev.blazn.hermes.included="false"' "$dockerfile"
grep -Fq 'dev.blazn.hermes.runnable="false"' "$dockerfile"
grep -Fq 'USER 65532:65532' "$dockerfile"
grep -Fq 'ENTRYPOINT ["/opt/blazn/blazn-harness-worker", "--", "/opt/blazn/hermes", "run", "--jsonl"]' "$dockerfile"
if grep -Eq 'COPY .*hermes|ADD .*hermes|curl .*hermes|wget .*hermes' "$dockerfile"; then
  printf 'foundation must not acquire a Hermes artifact\n' >&2
  exit 1
fi
grep -Fq -- '--platform linux/amd64,linux/arm64' "$builder"
# Match literal shell source.
# shellcheck disable=SC2016
grep -Fq 'fetch_tool "$BUILDX_URL" "$BUILDX_SHA256"' "$builder"
# Match literal shell source.
# shellcheck disable=SC2016
grep -Fq -- '--driver-opt "image=$BUILDKIT_IMAGE"' "$builder"
grep -Fq -- '--provenance=false --sbom=false' "$builder"
grep -Fq 'source checkout is not clean' "$builder"
grep -Fq 'hermesIncluded": False' "$builder"
grep -Fq '"runnable": False' "$builder"
grep -Fq 'set(children) == {"linux/amd64", "linux/arm64"}' "$builder"
printf 'harness worker foundation static checks passed\n'
