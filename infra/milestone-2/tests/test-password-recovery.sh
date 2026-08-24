#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
RECOVERY=$ROOT_DIR/scripts/recover-bootstrap-password.sh
LOCK=$ROOT_DIR/scripts/with-control-plane-lock.sh

top=${TMPDIR:-/tmp}/blazn-password-recovery-test-$$
mkdir -p "$top/bin"
cleanup() {
  find "$top" -xdev -type l -delete
  find "$top" -xdev -type f -delete
  find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

cat >"$top/bin/id" <<'EOF'
#!/bin/sh
[ "$1" = -u ] && { printf '0\n'; exit 0; }
exec /usr/bin/id "$@"
EOF
cat >"$top/bin/stat" <<'EOF'
#!/bin/sh
if [ "$1" = -c ] && [ "$2" = %u ]; then printf '0\n'; else exec /usr/bin/stat "$@"; fi
EOF
cat >"$top/bin/jq" <<'EOF'
#!/bin/sh
case "$*" in
  *".image "*) printf 'synthetic-control-api-image\n' ;;
  *) : ;;
esac
EOF
cat >"$top/bin/install" <<'EOF'
#!/bin/sh
set -eu
while [ "$#" -gt 2 ]; do shift; done
printf 'candidate-prepared\n' >>"$FAKE_RECOVERY_LOG"
exec /usr/bin/install -m 0400 -- "$1" "$2"
EOF
cat >"$top/bin/mv" <<'EOF'
#!/bin/sh
set -eu
while [ "$#" -gt 2 ]; do shift; done
case "$1:$2" in
  *".initial-password.recovery-candidate:"*"/initial-password")
    printf 'atomic-rename\n' >>"$FAKE_RECOVERY_LOG"
    [ "${FAKE_RENAME_FAIL:-0}" != 1 ] || exit 73
    ;;
esac
exec /usr/bin/mv -- "$1" "$2"
EOF
cat >"$top/bin/chmod" <<'EOF'
#!/bin/sh
set -eu
mode=$1
shift
[ "${1:-}" != -- ] || shift
target=${1:?missing chmod target}
case "$mode:$target" in
  0444:*".initial-password.recovery-candidate")
    printf 'candidate-publishable\n' >>"$FAKE_RECOVERY_LOG"
    [ "${FAKE_CHMOD_FAIL:-0}" != 1 ] || exit 74
    ;;
esac
exec /usr/bin/chmod "$mode" -- "$target"
EOF
cat >"$top/bin/docker" <<'EOF'
#!/bin/sh
set -eu
case "$*" in
  "image inspect "*) printf 'sha256:synthetic-image\n' ;;
  compose*" config --quiet") exit 0 ;;
  compose*" run "*)
    candidate=$BLAZN_SECRETS_ROOT/.initial-password.recovery-candidate
    [ -f "$candidate" ] || { printf 'database command ran without a candidate\n' >&2; exit 72; }
    candidate_mode=$(stat -c '%a' "$candidate")
    case "$candidate_mode" in 400|444) ;; *) printf 'candidate has unsafe mode\n' >&2; exit 72 ;; esac
    [ "$(cat "$BLAZN_SECRETS_ROOT/initial-password")" = old-password ] || { printf 'installed secret changed before database command\n' >&2; exit 72; }
    printf 'database-command-%s\n' "$candidate_mode" >>"$FAKE_RECOVERY_LOG"
    case "${FAKE_DB_RESULT:-success}" in
      success) exit 0 ;;
      known-failure) exit 10 ;;
      uncertain) exit 11 ;;
      *) exit 97 ;;
    esac
    ;;
  *) printf 'unexpected synthetic docker call: %s\n' "$*" >&2; exit 97 ;;
esac
EOF
chmod 0755 "$top/bin"/*

fixture() {
  name=$1
  fixture_root=$top/$name
  mkdir -p "$fixture_root/secrets" "$fixture_root/locks"
  chmod 0700 "$fixture_root/secrets"
  printf 'DATABASE CONFIG\n' >"$fixture_root/env"
  chmod 0600 "$fixture_root/env"
  printf 'postgresql://synthetic.invalid/blazn\n' >"$fixture_root/secrets/bootstrap-database-url"
  printf 'old-password\n' >"$fixture_root/secrets/initial-password"
  chmod 0444 "$fixture_root/secrets/bootstrap-database-url" "$fixture_root/secrets/initial-password"
  printf 'new-password-value\n' >"$fixture_root/staged"
  chmod 0400 "$fixture_root/staged"
  : >"$fixture_root/build-receipt.json"
  chmod 0600 "$fixture_root/build-receipt.json"
  : >"$fixture_root/events"
  printf '%s\n' "$fixture_root"
}

run_locked() {
  fixture_root=$1
  shift
  env \
    PATH="$top/bin:$PATH" \
    FAKE_RECOVERY_LOG="$fixture_root/events" \
    BLAZN_LOCK_ROOT="$fixture_root/locks" \
    BLAZN_SECRETS_ROOT="$fixture_root/secrets" \
    BLAZN_CONTROL_PLANE_ENV_FILE="$fixture_root/env" \
    BLAZN_CONTROL_API_BUILD_RECEIPT="$fixture_root/build-receipt.json" \
    "$@" "$LOCK" password-recovery test-correlation auto "$RECOVERY" "$fixture_root/staged"
}

success=$(fixture success)
run_locked "$success" env >"$success/out" 2>"$success/err"
[ "$(cat "$success/events")" = "candidate-prepared
database-command-400
candidate-publishable
atomic-rename" ] || { printf 'recovery ordering is incorrect\n' >&2; exit 1; }
[ "$(cat "$success/secrets/initial-password")" = new-password-value ]
[ "$(stat -c '%a' "$success/secrets/initial-password")" = 444 ]
[ ! -e "$success/secrets/.initial-password.recovery-candidate" ]
grep -F 'bootstrap password recovery completed' "$success/out" >/dev/null

db_failure=$(fixture db-failure)
if run_locked "$db_failure" env FAKE_DB_RESULT=known-failure >"$db_failure/out" 2>"$db_failure/err"; then
  printf 'database failure unexpectedly succeeded\n' >&2
  exit 1
fi
[ "$(cat "$db_failure/secrets/initial-password")" = old-password ]
[ ! -e "$db_failure/secrets/.initial-password.recovery-candidate" ]
[ "$(cat "$db_failure/events")" = "candidate-prepared
database-command-400" ]

uncertain=$(fixture uncertain)
if run_locked "$uncertain" env FAKE_DB_RESULT=uncertain >"$uncertain/out" 2>"$uncertain/err"; then
  printf 'uncertain database result unexpectedly succeeded\n' >&2
  exit 1
fi
[ "$(cat "$uncertain/secrets/initial-password")" = old-password ]
cmp -s "$uncertain/staged" "$uncertain/secrets/.initial-password.recovery-candidate"
grep -F 'outcome is uncertain' "$uncertain/err" >/dev/null

chmod_failure=$(fixture chmod-failure)
if run_locked "$chmod_failure" env FAKE_CHMOD_FAIL=1 >"$chmod_failure/out" 2>"$chmod_failure/err"; then
  printf 'permission activation failure unexpectedly succeeded\n' >&2
  exit 1
fi
[ "$(cat "$chmod_failure/secrets/initial-password")" = old-password ]
cmp -s "$chmod_failure/staged" "$chmod_failure/secrets/.initial-password.recovery-candidate"
[ "$(stat -c '%a' "$chmod_failure/secrets/.initial-password.recovery-candidate")" = 400 ]
grep -F 'candidate permission activation failed' "$chmod_failure/err" >/dev/null

rename_failure=$(fixture rename-failure)
if run_locked "$rename_failure" env FAKE_RENAME_FAIL=1 >"$rename_failure/out" 2>"$rename_failure/err"; then
  printf 'rename failure unexpectedly succeeded\n' >&2
  exit 1
fi
[ "$(cat "$rename_failure/secrets/initial-password")" = old-password ]
cmp -s "$rename_failure/staged" "$rename_failure/secrets/.initial-password.recovery-candidate"
[ "$(stat -c '%a' "$rename_failure/secrets/.initial-password.recovery-candidate")" = 444 ]
grep -F 'preserve the recovery candidate' "$rename_failure/err" >/dev/null
run_locked "$rename_failure" env >"$rename_failure/retry.out" 2>"$rename_failure/retry.err"
[ "$(cat "$rename_failure/secrets/initial-password")" = new-password-value ]
[ "$(stat -c '%a' "$rename_failure/secrets/initial-password")" = 444 ]
grep -F 'database-command-444' "$rename_failure/events" >/dev/null

validation=$(fixture validation)
if env PATH="$top/bin:$PATH" BLAZN_SECRETS_ROOT="$validation/secrets" "$RECOVERY" "$validation/staged" >"$validation/direct.out" 2>"$validation/direct.err"; then
  printf 'direct recovery without the lock unexpectedly succeeded\n' >&2
  exit 1
fi
grep -F 'must run through with-control-plane-lock.sh' "$validation/direct.err" >/dev/null
chmod 0600 "$validation/staged"
if run_locked "$validation" env >"$validation/mode.out" 2>"$validation/mode.err"; then
  printf 'permissive staged password unexpectedly succeeded\n' >&2
  exit 1
fi
grep -F 'mode 0400 or 0440' "$validation/mode.err" >/dev/null
chmod 0400 "$validation/staged"
ln -s "$validation/staged" "$validation/staged-link"
if env PATH="$top/bin:$PATH" BLAZN_FENCING_TOKEN=1 BLAZN_SECRETS_ROOT="$validation/secrets" "$RECOVERY" "$validation/staged-link" >"$validation/link.out" 2>"$validation/link.err"; then
  printf 'symlink staged password unexpectedly succeeded\n' >&2
  exit 1
fi
grep -F 'symbolic link' "$validation/link.err" >/dev/null
if env PATH="$top/bin:$PATH" BLAZN_FENCING_TOKEN=1 "$RECOVERY" relative-password >"$validation/relative.out" 2>"$validation/relative.err"; then
  printf 'relative staged password unexpectedly succeeded\n' >&2
  exit 1
fi
grep -F 'absolute path' "$validation/relative.err" >/dev/null

# These assertions intentionally match literal shell variables in the runbook.
grep -F 'with-control-plane-lock.sh' "$ROOT_DIR/README.md" >/dev/null
# shellcheck disable=SC2016
grep -F 'password-recovery "$correlation_id" auto' "$ROOT_DIR/README.md" >/dev/null
# shellcheck disable=SC2016
grep -F 'recover-bootstrap-password.sh "$staged"' "$ROOT_DIR/README.md" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'password recovery lock, validation, ordering, cleanup, and retry tests passed\n'
