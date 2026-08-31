#!/bin/sh
set -eu

# Build and scan the worker foundation from the exact clean checkout. This is
# deliberately not runtime proof: the foundation contains no Hermes artifact.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH='' cd -- "$ROOT/../.." && pwd)
for required in docker git python3; do
  command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-harness-foundation-e2e.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

commit=$(git -C "$REPO" rev-parse HEAD)
BLAZN_EXPECTED_SOURCE_COMMIT=$commit "$ROOT/phase5-harness-worker-image/build-foundation.sh" "$REPO" "$tmp/out"

python3 - "$tmp/out/build-report.json" "$commit" <<'PY'
import json, os, re, sys
path, commit = sys.argv[1:3]
report = json.load(open(path))
assert report["schema"] == "blazn.dev/harness-worker-foundation-build-report/v1"
assert report["sourceCommit"] == commit
assert report["hermesIncluded"] is False
assert report["runnable"] is False
for key in ("index", "linux/amd64", "linux/arm64"):
    assert re.fullmatch(r"sha256:[0-9a-f]{64}", report["image"][key]), (key, report["image"][key])
for arch in ("amd64", "arm64"):
    scan = os.path.join(os.path.dirname(path), f"harness-worker-foundation-{arch}-scan.json")
    assert os.path.isfile(scan) and os.path.getsize(scan) > 0, scan
PY

if BLAZN_EXPECTED_SOURCE_COMMIT=0000000000000000000000000000000000000000 \
  "$ROOT/phase5-harness-worker-image/build-foundation.sh" "$REPO" "$tmp/reject" 2>"$tmp/reject.err"; then
  printf 'wrong-commit build was accepted\n' >&2
  exit 1
fi
grep -Fq 'not the reviewed commit' "$tmp/reject.err"
printf 'harness worker foundation multi-architecture build and scan proof passed\n'
