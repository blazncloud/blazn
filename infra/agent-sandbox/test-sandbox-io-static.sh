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
grep -F 'COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt' "$dockerfile" >/dev/null
grep -F 'github.com/go-git/go-git/v5 v5.19.2' "$ROOT/go.mod" >/dev/null

if grep -E '^FROM [^@[:space:]]+:[^@[:space:]]+([[:space:]]|$)' "$dockerfile" >/dev/null; then
  printf 'Sandbox I/O Dockerfile contains a mutable base image\n' >&2
  exit 1
fi
if grep -E '(^|[[:space:]])(apt|apt-get|apk|dnf|yum|curl|wget)([[:space:]]|$)' "$dockerfile" >/dev/null; then
  printf 'Sandbox I/O Dockerfile performs an unreviewed package or network operation\n' >&2
  exit 1
fi
if grep -R -E 'os\.Getenv|os\.LookupEnv|exec\.Command|crypto/tls|database/sql|client-go|kubectl|/bin/(sh|bash)' \
  "$ROOT/cmd/blazn-sandbox-io" "$ROOT/internal/sandboxio" --include='*.go' --exclude='*_test.go' >/dev/null; then
  printf 'Sandbox I/O helper contains a credential, shell, TLS override, database, or generic exec boundary\n' >&2
  exit 1
fi
if grep -R -E 'net/http' "$ROOT/cmd/blazn-sandbox-io" "$ROOT/internal/sandboxio" --include='*.go' --exclude='source_materializer.go' --exclude='source_materializer_test.go' >/dev/null; then
  printf 'Sandbox I/O network access escaped the reviewed source materializer\n' >&2
  exit 1
fi
source=$ROOT/internal/sandboxio/source_materializer.go
for boundary in \
  'base.Proxy = nil' \
  'request.Header.Get("Authorization") != ""' \
  'request.Header.Get("Proxy-Authorization") != ""' \
  'request.Header.Get("Cookie") != ""' \
  'request.URL.Scheme != t.scheme' \
  'request.URL.Host != t.host' \
  'request.URL.RawQuery != "service=git-upload-pack"' \
  'Depth: 1' \
  'Tags: git.NoTags' \
  'Auth:'; do
  if [ "$boundary" = 'Auth:' ]; then
    if grep -F "$boundary" "$source" >/dev/null; then
      printf 'Sandbox source materializer supplies Git authentication\n' >&2
      exit 1
    fi
  elif ! grep -F "$boundary" "$source" >/dev/null; then
    printf 'Sandbox source transport boundary is missing: %s\n' "$boundary" >&2
    exit 1
  fi
done

printf 'Sandbox I/O static audit passed\n'
