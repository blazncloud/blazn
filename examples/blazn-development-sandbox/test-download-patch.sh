#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-patch-test.XXXXXX")
cleanup() { rm -r -- "$work"; }
trap cleanup EXIT HUP INT TERM

sandbox=10000000-0000-4000-8000-000000000001
mock=$work/blazn
cat >"$mock" <<'EOF'
#!/bin/sh
destination=$7
printf 'patch payload\n' >"$destination"
case $DOWNLOAD_MODE in
  partial) exit 7 ;;
  mismatch) printf '{"sha256":"sha256:%064d"}\n' 0 ;;
  *)
    if command -v sha256sum >/dev/null 2>&1; then digest=$(sha256sum "$destination" | awk '{print $1}'); else digest=$(shasum -a 256 "$destination" | awk '{print $1}'); fi
    printf '{"sha256":"sha256:%s"}\n' "$digest"
    ;;
esac
EOF
chmod +x "$mock"

for mode in partial mismatch; do
  output=$work/$mode.patch
  if DOWNLOAD_MODE=$mode sh "$script_dir/download-patch.sh" "$mock" "$sandbox" "$output" "$work/$mode.json" >"$work/$mode.out" 2>"$work/$mode.err"; then
    printf 'patch download unexpectedly succeeded in %s mode\n' "$mode" >&2
    exit 1
  fi
  [ ! -e "$output" ]
  [ ! -e "$output.sha256" ]
  if find "$work" -name "$mode.patch.partial.*" -o -name "$mode.patch.sha256.partial.*" | grep . >/dev/null; then
    printf 'temporary patch output remained after %s failure\n' "$mode" >&2
    exit 1
  fi
done

output=$work/existing.patch
printf '%s\n' 'owner content' >"$output"
if DOWNLOAD_MODE=success sh "$script_dir/download-patch.sh" "$mock" "$sandbox" "$output" "$work/existing.json" >"$work/existing.out" 2>"$work/existing.err"; then
  printf '%s\n' 'patch download unexpectedly overwrote an existing output' >&2
  exit 1
fi
[ "$(cat "$output")" = 'owner content' ]
grep -F 'refusing to overwrite' "$work/existing.err" >/dev/null

output=$work/success.patch
DOWNLOAD_MODE=success sh "$script_dir/download-patch.sh" "$mock" "$sandbox" "$output" "$work/success.json" >/dev/null
[ -s "$output" ]
[ -s "$output.sha256" ]
(cd "$work" && if command -v sha256sum >/dev/null 2>&1; then sha256sum -c success.patch.sha256; else shasum -a 256 -c success.patch.sha256; fi) >/dev/null
printf '%s\n' 'Blazn development patch download checks passed.'
