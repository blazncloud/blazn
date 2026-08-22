#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$TEST_DIR/../../.." && pwd)
STAGE=$REPO_ROOT/infra/milestone-2/scripts/stage-release.sh
PROMOTE=$REPO_ROOT/infra/milestone-2/scripts/promote-release.sh
ROLLBACK=$REPO_ROOT/infra/milestone-2/scripts/rollback-release.sh
command -v sudo >/dev/null 2>&1 || { printf 'release promotion tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'release promotion tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-release-promotion-test-$$
mkdir -p "$top/bin" "$top/active/infra/milestone-2/systemd"
cleanup() {
  sudo find "$top" -xdev -type l -delete
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

cat >"$top/bin/systemctl" <<'EOF'
#!/bin/sh
set -eu
case "$1" in
  is-active) printf '%s\n' "${FAKE_SYSTEMD_STATE:-inactive}"; exit 3 ;;
  daemon-reload) exit 0 ;;
  *) exit 97 ;;
esac
EOF
cat >"$top/bin/findmnt" <<'EOF'
#!/bin/sh
exit 1
EOF
chmod 0755 "$top/bin/systemctl" "$top/bin/findmnt"

cp "$REPO_ROOT/infra/milestone-2/systemd/blazn-control-plane.service" "$top/active/infra/milestone-2/systemd/blazn-control-plane.service"
printf 'legacy payload\n' >"$top/active/legacy.txt"
cp "$top/active/infra/milestone-2/systemd/blazn-control-plane.service" "$top/installed.service"
chmod 0644 "$top/installed.service"
sudo chown -R 0:0 "$top/active" "$top/installed.service"

commit=$(git -C "$REPO_ROOT" rev-parse HEAD)
common_env() {
  sudo env PATH="$top/bin:$PATH" BLAZN_FENCING_TOKEN=21 BLAZN_CORRELATION_ID=release-test \
    BLAZN_RELEASE_ROOT="$top/releases" BLAZN_RELEASE_RECEIPT_ROOT="$top/receipts" \
    BLAZN_ACTIVE_RELEASE_PATH="$top/active" BLAZN_ACTIVE_RELEASE_RECEIPT="$top/active-release.json" \
    BLAZN_RELEASE_PROMOTION_INTENT="$top/promotion-intent.json" BLAZN_SYSTEMD_UNIT_PATH="$top/installed.service" \
    "$@"
}

common_env "$STAGE" "$REPO_ROOT" "$commit" >"$top/stage.out"
common_env "$STAGE" "$REPO_ROOT" "$commit" >"$top/stage-retry.out"
[ -d "$top/releases/$commit" ]
sudo jq -e --arg commit "$commit" '.commit==$commit and (.releaseDigest|test("^sha256:[a-f0-9]{64}$"))' "$top/receipts/$commit.json" >/dev/null

if common_env env FAKE_SYSTEMD_STATE=failed "$PROMOTE" "$commit" >"$top/failed-running.out" 2>"$top/failed-running.err"; then
  printf 'failed unit with a potentially running Compose project unexpectedly promoted\n' >&2
  exit 1
fi
grep -F 'must be exactly inactive' "$top/failed-running.err" >/dev/null
[ -d "$top/active" ] && [ ! -e "$top/promotion-intent.json" ]

if common_env env BLAZN_PROMOTION_FAILPOINT=after-adopt "$PROMOTE" "$commit" >"$top/fault.out" 2>"$top/fault.err"; then
  printf 'release adoption failpoint unexpectedly passed\n' >&2
  exit 1
fi
[ -f "$top/promotion-intent.json" ] && [ ! -e "$top/active" ]
legacy_id=$(sudo jq -r .previousId "$top/promotion-intent.json")
[ -d "$top/releases/$legacy_id" ]
common_env "$PROMOTE" "$commit" >"$top/promote.out"
[ -L "$top/active" ] && [ "$(readlink -f "$top/active")" = "$top/releases/$commit" ]
[ ! -e "$top/promotion-intent.json" ]
cmp -s "$top/installed.service" "$top/releases/$commit/infra/milestone-2/systemd/blazn-control-plane.service"

common_env "$ROLLBACK" >"$top/rollback.out"
[ -L "$top/active" ] && [ "$(readlink -f "$top/active")" = "$top/releases/$legacy_id" ]
grep -F 'legacy payload' "$top/active/legacy.txt" >/dev/null
cmp -s "$top/installed.service" "$top/active/infra/milestone-2/systemd/blazn-control-plane.service"

trap - EXIT HUP INT TERM
cleanup
printf 'immutable release stage, retry, crash-resume, promotion, unit, and exact rollback tests passed\n'
