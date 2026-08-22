#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
UPGRADE=$ROOT_DIR/scripts/upgrade-live-v1-to-v2.sh
command -v sudo >/dev/null 2>&1 || { printf 'live-upgrade fault tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'live-upgrade fault tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-live-upgrade-test-$$
mkdir "$top"
cleanup() {
  if sudo find "$top" -xdev -type l -print | grep . >/dev/null; then
    printf 'unexpected symlink in live-upgrade test root\n' >&2
    return 1
  fi
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

cat >"$top/docker" <<'EOF'
#!/bin/sh
set -eu
case "$*" in
  "inspect --format "*) printf 'blazn-m2/postgres/running\n' ;;
  *" ps -q postgres") printf 'synthetic-postgres-container\n' ;;
  *"select count(*) from pg_roles where rolname='blazn_bootstrap'"*)
    if [ ! -f "$FAKE_DOCKER_STATE" ]; then printf '0\n'; else printf '1\n'; fi
    ;;
  *"select count(*) from pg_roles where rolname='blazn_migration'"*|*"select count(*) from pg_roles where rolname='blazn_runtime'"*) printf '1\n' ;;
  *"select count(*) from pg_auth_members"*"blazn_bootstrap"*) printf '%s\n' "${FAKE_DOCKER_BOOTSTRAP_MEMBERSHIPS:-0}" ;;
  *"select count(*) from pg_auth_members"*) printf '0\n' ;;
  *"select rolname,rolcanlogin"*"blazn_bootstrap"*) printf 'blazn_bootstrap|t|f|f|f|f|f\n' ;;
  *"select rolname,rolcanlogin"*"blazn_migration"*) printf 'blazn_migration|t|f|f|f|f|f\n' ;;
  *"select rolname,rolcanlogin"*"blazn_runtime"*) printf 'blazn_runtime|t|f|f|f|f|f\n' ;;
  *" /bin/sh -euc "*)
    while IFS= read -r ignored; do :; done
    printf 'blazn_bootstrap\n'
    ;;
  *" exec -T postgres psql "*)
    body=$(cat)
    case "$body" in *"ROLE blazn_bootstrap"*) : >"$FAKE_DOCKER_STATE" ;; esac
    ;;
  *) printf 'unexpected synthetic docker call: %s\n' "$*" >&2; exit 97 ;;
esac
EOF
chmod 0755 "$top/docker"

fixture() {
  name=$1
  root=$top/$name
  secrets=$root/secrets
  ownership=$root/ownership
  mkdir -p "$secrets" "$ownership"
  printf admin >"$secrets/postgres-password"
  printf migration >"$secrets/migration-database-url"
  printf runtime >"$secrets/runtime-database-url"
  printf initial >"$secrets/initial-password"
  printf oldrootaccess >"$secrets/s3-access-key"
  printf oldrootsecret >"$secrets/s3-secret-key"
  chmod 0444 "$secrets"/*
  jq -cn --arg host "$(hostname)" --arg secrets "$secrets" \
    '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host,paths:{secrets:$secrets}}' >"$ownership/control-plane.json"
  chmod 0600 "$ownership/control-plane.json"
  sudo chown -R 0:0 "$secrets" "$ownership"
  sudo chmod 0700 "$secrets" "$ownership"
  printf '%s\n' "$root"
}

run_upgrade() {
  root=$1
  shift
  sudo env \
    PATH="$top:$PATH" \
    FAKE_DOCKER_STATE="$root/docker-role-ready" \
    FAKE_DOCKER_BOOTSTRAP_MEMBERSHIPS="${FAKE_DOCKER_BOOTSTRAP_MEMBERSHIPS:-0}" \
    BLAZN_FENCING_TOKEN=7 \
    BLAZN_SECRETS_ROOT="$root/secrets" \
    BLAZN_RECEIPT_PATH="$root/ownership/control-plane.json" \
    BLAZN_UPGRADE_RECEIPT_PATH="$root/ownership/control-plane-v2-upgrade.json" \
    POSTGRES_DB=blazn \
    POSTGRES_USER=blazn_admin \
    "$UPGRADE" "$@"
}

normal=$(fixture normal)
run_upgrade "$normal" >"$normal/run-1.out"
grep -F 'identity-ready' "$normal/run-1.out" >/dev/null
sudo jq -e '.phase == "identity-ready" and .identityValidatedAt' "$normal/ownership/control-plane-v2-upgrade.json" >/dev/null
[ "$(sudo sha256sum "$normal/secrets/s3-access-key" | awk '{print $1}')" = "$(sudo sha256sum "$normal/secrets/s3-root-access-key" | awk '{print $1}')" ]
[ "$(sudo sha256sum "$normal/secrets/s3-secret-key" | awk '{print $1}')" = "$(sudo sha256sum "$normal/secrets/s3-root-secret-key" | awk '{print $1}')" ]
before=$(sudo sha256sum "$normal/secrets/bootstrap-database-url" "$normal/secrets/s3-runtime-access-key" "$normal/secrets/s3-runtime-secret-key")
run_upgrade "$normal" >"$normal/run-2.out"
after=$(sudo sha256sum "$normal/secrets/bootstrap-database-url" "$normal/secrets/s3-runtime-access-key" "$normal/secrets/s3-runtime-secret-key")
[ "$before" = "$after" ] || { printf 'idempotent retry changed generated secrets\n' >&2; exit 1; }
if grep -E 'oldroot|postgresql://|[a-f0-9]{48,}' "$normal"/*.out >/dev/null; then
  printf 'live upgrade emitted secret material\n' >&2
  exit 1
fi
if FAKE_DOCKER_BOOTSTRAP_MEMBERSHIPS=1 run_upgrade "$normal" >"$normal/membership.out" 2>"$normal/membership.err"; then
  printf 'bootstrap role with inherited membership unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'unreviewed inherited role memberships' "$normal/membership.err" >/dev/null

partial=$(fixture partial)
sudo mkdir "$partial/secrets/.m2-v2-upgrade-staging"
sudo chmod 0700 "$partial/secrets/.m2-v2-upgrade-staging"
sudo sh -c "printf 'postgresql://blazn_bootstrap:%064d@postgres:5432/blazn\\n' 1 >'$partial/secrets/.m2-v2-upgrade-staging/bootstrap-database-url'"
sudo sh -c "printf 'blaznruntime%016d\\n' 2 >'$partial/secrets/.m2-v2-upgrade-staging/s3-runtime-access-key'"
sudo sh -c "printf '%064d\\n' 3 >'$partial/secrets/.m2-v2-upgrade-staging/s3-runtime-secret-key'"
sudo sh -c 'chmod 0444 "$1"/*' sh "$partial/secrets/.m2-v2-upgrade-staging"
sudo ln "$partial/secrets/s3-access-key" "$partial/secrets/s3-root-access-key"
run_upgrade "$partial" >"$partial/resumed.out"
sudo jq -e '.phase == "identity-ready"' "$partial/ownership/control-plane-v2-upgrade.json" >/dev/null
[ ! -e "$partial/secrets/.m2-v2-upgrade-staging" ]

corrupt=$(fixture corrupt)
printf wrong >"$corrupt/wrong"
chmod 0444 "$corrupt/wrong"
sudo chown 0:0 "$corrupt/wrong"
sudo ln "$corrupt/wrong" "$corrupt/secrets/s3-root-access-key"
if run_upgrade "$corrupt" >"$corrupt/out" 2>"$corrupt/err"; then
  printf 'ambiguous partial upgrade unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'without an upgrade receipt or recovery staging directory' "$corrupt/err" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'live-upgrade clean, retry, partial-resume, and fail-closed tests passed\n'
