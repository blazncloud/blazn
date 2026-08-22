#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
UPGRADE=$NODE_ROOT/scripts/upgrade-control-plane.sh
ROLLBACK=$NODE_ROOT/scripts/rollback-control-plane.sh
command -v sudo >/dev/null 2>&1 || { printf 'Node upgrade resume test skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'Node upgrade resume test skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-node-upgrade-$$
mkdir "$top"
cleanup() {
  sudo find "$top" -xdev -type l -print | grep . >/dev/null && { printf 'unexpected symlink in test root\n' >&2; return 1; }
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

cat >"$top/docker" <<'EOF'
#!/bin/sh
set -eu
case "$*" in
  "compose "*" ps -q postgres") printf 'synthetic-postgres\n' ;;
  "inspect --format "*) printf 'blazn-m2/postgres/running\n' ;;
  *"select count(*) from pg_roles where rolname='blazn_node_broker'"*) if [ -f "$FAKE_ROLE_STATE" ]; then printf '1\n'; else printf '0\n'; fi ;;
  *"select count(*) from schema_migrations where version in"*) printf '0\n' ;;
  *"select count(*) from pg_auth_members"*) printf '0\n' ;;
  "compose "*" exec -T postgres psql "*) body=$(cat); [ "${FAKE_SQL_FAIL:-0}" != 1 ] || exit 88; case "$body" in *"DROP ROLE blazn_node_broker"*) rm -f "$FAKE_ROLE_STATE" ;; *blazn_node_broker*) : >"$FAKE_ROLE_STATE" ;; esac ;;
  "compose "*" run --rm -T node-migration-preflight") [ -f "$FAKE_ROLE_STATE" ] ;;
  *) printf 'unexpected synthetic docker call: %s\n' "$*" >&2; exit 97 ;;
esac
EOF
chmod 0755 "$top/docker"

fixture() {
  name=$1
  root=$top/$name
  mkdir -p "$root/ownership" "$root/etc" "$root/bin"
  cp "$top/docker" "$root/bin/docker"
  jq -cn --arg host "$(hostname)" '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host,controlApi:{sourceDigest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},configDigest:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}' >"$root/ownership/control-plane.json"
  : >"$root/control-plane.env"
  chmod 0600 "$root/ownership/control-plane.json" "$root/control-plane.env"
  sudo chown -R 0:0 "$root/ownership" "$root/control-plane.env" "$root/etc"
  sudo chmod 0700 "$root/ownership" "$root/etc"
  printf '%s\n' "$root"
}

run_rollback() {
  root=$1
  fail_after=${2:-}
  observed=${3:-match}
  if [ "$observed" = match ]; then source_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; config_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; else source_digest=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc; config_digest=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd; fi
  sudo env \
    PATH="$root/bin:$PATH" FAKE_ROLE_STATE="$root/role-ready" BLAZN_FENCING_TOKEN=12 BLAZN_CORRELATION_ID=rollback \
    BLAZN_NODE_INFRA_TEST_MODE=1 BLAZN_NODE_ROLLBACK_TEST_FAIL_AFTER="$fail_after" \
    BLAZN_NODE_INFRA_TEST_OBSERVED_SOURCE_DIGEST="$source_digest" BLAZN_NODE_INFRA_TEST_OBSERVED_CONFIG_DIGEST="$config_digest" \
    BLAZN_NODE_INFRA_TEST_NODE_ROOT="$root/etc/node-broker" \
    BLAZN_NODE_INFRA_TEST_CREATE_JOURNAL="$root/ownership/secret-create.json" \
    BLAZN_NODE_INFRA_TEST_RETAIN_PARENT="$root/ownership" \
    BLAZN_NODE_BROKER_CREATE_JOURNAL="$root/ownership/secret-create.json" \
    BLAZN_CONTROL_PLANE_ENV_FILE="$root/control-plane.env" \
    BLAZN_RECEIPT_PATH="$root/ownership/control-plane.json" \
    BLAZN_NODE_BROKER_UPGRADE_RECEIPT="$root/ownership/node-broker-upgrade.json" \
    BLAZN_NODE_BROKER_UPGRADE_BACKUP_ROOT="$root/ownership/upgrade-inputs" \
    BLAZN_CONTROL_API_BUILD_RECEIPT="$root/ownership/no-build-receipt" \
    "$ROLLBACK"
}

run_upgrade() {
  root=$1
  fail_after=${2:-}
  sql_fail=${3:-0}
  sudo env \
    PATH="$root/bin:$PATH" \
    FAKE_ROLE_STATE="$root/role-ready" \
    FAKE_SQL_FAIL="$sql_fail" \
    BLAZN_FENCING_TOKEN=11 \
    BLAZN_NODE_INFRA_TEST_MODE=1 \
    BLAZN_NODE_INFRA_TEST_FAIL_AFTER="$fail_after" \
    BLAZN_NODE_INFRA_TEST_NODE_ROOT="$root/etc/node-broker" \
    BLAZN_NODE_INFRA_TEST_CREATE_JOURNAL="$root/ownership/secret-create.json" \
    BLAZN_NODE_BROKER_SECRETS_ROOT="$root/etc/node-broker/secrets" \
    BLAZN_CONTROL_PLANE_ENV_FILE="$root/control-plane.env" \
    BLAZN_RECEIPT_PATH="$root/ownership/control-plane.json" \
    BLAZN_NODE_BROKER_UPGRADE_RECEIPT="$root/ownership/node-broker-upgrade.json" \
    BLAZN_NODE_BROKER_UPGRADE_BACKUP_ROOT="$root/ownership/upgrade-inputs" \
    BLAZN_CONTROL_API_BUILD_RECEIPT="$root/ownership/no-build-receipt" \
    "$UPGRADE"
}

sql_root=$(fixture sql-transaction)
if run_upgrade "$sql_root" '' 1 >"$sql_root/sql-first.out" 2>"$sql_root/sql-first.err"; then printf 'failing SQL role transaction unexpectedly passed\n' >&2; exit 1; fi
[ ! -e "$sql_root/role-ready" ] || { printf 'failing SQL role transaction left role state\n' >&2; exit 1; }
sudo jq -e '.phase=="inputs-backed-up"' "$sql_root/ownership/node-broker-upgrade.json" >/dev/null
run_upgrade "$sql_root" >"$sql_root/sql-retry.out"
sudo jq -e '.phase=="complete"' "$sql_root/ownership/node-broker-upgrade.json" >/dev/null

for fault in secrets-published input-root-created main-backed-up environment-backed-up build-backed-up role-ready environment-bound build-ready complete; do
  root=$(fixture "$fault")
  if run_upgrade "$root" "$fault" >"$root/first.out" 2>"$root/first.err"; then
    printf 'injected fault unexpectedly completed: %s\n' "$fault" >&2
    exit 1
  fi
  grep -F "injected test fault after $fault" "$root/first.err" >/dev/null
  run_upgrade "$root" >"$root/retry.out"
  sudo jq -e '.phase=="complete" and .schemaVersion=="blazn.dev/node-broker-upgrade/v2"' "$root/ownership/node-broker-upgrade.json" >/dev/null
  sudo jq -e '.nodeBroker.schemaVersion=="blazn.dev/node-broker-infra/v1"' "$root/ownership/control-plane.json" >/dev/null
  sudo grep -Fx 'BLAZN_NODE_BROKER_SECRETS_ROOT=/etc/blazn/node-broker/secrets' "$root/control-plane.env" >/dev/null
  before=$(sudo sha256sum "$root/etc/node-broker/secrets/database-url" "$root/etc/node-broker/secrets/enrollment-hmac-v1" "$root/etc/node-broker/secrets/join-credential-v1")
  run_upgrade "$root" >"$root/idempotent.out"
  after=$(sudo sha256sum "$root/etc/node-broker/secrets/database-url" "$root/etc/node-broker/secrets/enrollment-hmac-v1" "$root/etc/node-broker/secrets/join-credential-v1")
  [ "$before" = "$after" ] || { printf 'retry changed Node broker secrets: %s\n' "$fault" >&2; exit 1; }
  if grep -E 'postgresql://|[a-f0-9]{48,}' "$root"/*.out "$root"/*.err >/dev/null; then
    printf 'upgrade test output exposed secret material\n' >&2
    exit 1
  fi
done

for fault in rollback-started role-removed secrets-retained environment-restored build-restored main-restored source-restore-required rolled-back; do
  root=$(fixture "rollback-$fault")
  run_upgrade "$root" >"$root/upgrade.out"
  if run_rollback "$root" "$fault" >"$root/rollback-first.out" 2>"$root/rollback-first.err"; then printf 'rollback fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected rollback fault after $fault" "$root/rollback-first.err" >/dev/null
  if ! run_rollback "$root" >"$root/rollback-retry.out" 2>"$root/rollback-retry.err"; then sudo tail -80 "$root/rollback-retry.err" >&2; exit 1; fi
  sudo jq -e '(.nodeBroker|not)' "$root/ownership/control-plane.json" >/dev/null
  sudo jq -e '.phase=="rolled-back"' "$root/ownership/node-broker-upgrade.json" >/dev/null
  if ! sudo test ! -e "$root/etc/node-broker" || ! sudo test -d "$root/ownership/node-broker-rollback-rollback"; then printf 'rollback retention state is invalid after %s\n' "$fault" >&2; exit 1; fi
  [ ! -e "$root/role-ready" ] || { printf 'rollback retry left database role\n' >&2; exit 1; }
  sudo test ! -s "$root/control-plane.env" || { printf 'rollback did not restore original environment\n' >&2; exit 1; }
done

source_root=$(fixture source-mismatch)
run_upgrade "$source_root" >"$source_root/upgrade.out"
if run_rollback "$source_root" source-restore-required >"$source_root/source-first.out" 2>"$source_root/source-first.err"; then printf 'source-restore fault unexpectedly completed\n' >&2; exit 1; fi
if run_rollback "$source_root" '' mismatch >"$source_root/source-bad.out" 2>"$source_root/source-bad.err"; then printf 'mismatched source restore unexpectedly completed\n' >&2; exit 1; fi
grep -F 'prior source restore is required' "$source_root/source-bad.err" >/dev/null
sudo jq -e '.phase=="source-restore-required"' "$source_root/ownership/node-broker-upgrade.json" >/dev/null
run_rollback "$source_root" >"$source_root/source-good.out"
sudo jq -e '.phase=="rolled-back"' "$source_root/ownership/node-broker-upgrade.json" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'Node infrastructure fault-resumable upgrade tests passed\n'
