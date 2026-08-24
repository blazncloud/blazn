#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
dockerfile=$ROOT/Dockerfile.sandbox-io

# These assertions intentionally match literal Dockerfile build arguments.
# shellcheck disable=SC2016
grep -F 'FROM --platform=$BUILDPLATFORM golang:1.26.2-bookworm@sha256:' "$dockerfile" >/dev/null
# shellcheck disable=SC2016
grep -F 'CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH' "$dockerfile" >/dev/null
grep -F 'FROM scratch' "$dockerfile" >/dev/null
grep -F 'USER 65532:65532' "$dockerfile" >/dev/null
grep -F 'ENTRYPOINT ["/blazn-sandbox-io"]' "$dockerfile" >/dev/null
grep -F 'COPY internal/sandboxio ./internal/sandboxio' "$dockerfile" >/dev/null

if grep -E '^FROM [^@[:space:]]+:[^@[:space:]]+([[:space:]]|$)' "$dockerfile" >/dev/null; then
  printf 'Sandbox I/O Dockerfile contains a mutable base image\n' >&2
  exit 1
fi
if grep -E '(^|[[:space:]])(apt|apt-get|apk|dnf|yum|curl|wget)([[:space:]]|$)' "$dockerfile" >/dev/null; then
  printf 'Sandbox I/O Dockerfile performs an unreviewed package or network operation\n' >&2
  exit 1
fi
if grep -R -E 'os\.Getenv|os\.LookupEnv|exec\.Command|net/http|crypto/tls|database/sql|client-go|kubectl|/bin/(sh|bash)' \
  "$ROOT/cmd/blazn-sandbox-io" "$ROOT/internal/sandboxio" --include='*.go' >/dev/null; then
  printf 'Sandbox I/O helper contains a credential, shell, network, database, or generic exec boundary\n' >&2
  exit 1
fi

printf 'Sandbox I/O static audit passed\n'
