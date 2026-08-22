#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
NODE_ROOT=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
UPGRADE=$NODE_ROOT/scripts/upgrade-control-plane.sh
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
  *"select count(*) from pg_auth_members"*) printf '0\n' ;;
  "compose "*" exec -T postgres psql "*) body=$(cat); case "$body" in *blazn_node_broker*) : >"$FAKE_ROLE_STATE" ;; esac ;;
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
  jq -cn --arg host "$(hostname)" '{schemaVersion:"blazn.dev/control-plane-ownership/v1",owner:"blazn-poc",host:$host}' >"$root/ownership/control-plane.json"
  : >"$root/control-plane.env"
  chmod 0600 "$root/ownership/control-plane.json" "$root/control-plane.env"
  sudo chown -R 0:0 "$root/ownership" "$root/control-plane.env" "$root/etc"
  sudo chmod 0700 "$root/ownership" "$root/etc"
  printf '%s\n' "$root"
}

run_upgrade() {
  root=$1
  fail_after=${2:-}
  sudo env \
    PATH="$root/bin:$PATH" \
    FAKE_ROLE_STATE="$root/role-ready" \
    BLAZN_FENCING_TOKEN=11 \
    BLAZN_NODE_INFRA_TEST_MODE=1 \
    BLAZN_NODE_INFRA_TEST_FAIL_AFTER="$fail_after" \
    BLAZN_NODE_INFRA_TEST_NODE_ROOT="$root/etc/node-broker" \
    BLAZN_NODE_INFRA_TEST_STAGE="$root/etc/node-broker-staging" \
    BLAZN_NODE_BROKER_SECRETS_ROOT="$root/etc/node-broker/secrets" \
    BLAZN_CONTROL_PLANE_ENV_FILE="$root/control-plane.env" \
    BLAZN_RECEIPT_PATH="$root/ownership/control-plane.json" \
    BLAZN_NODE_BROKER_UPGRADE_RECEIPT="$root/ownership/node-broker-upgrade.json" \
    BLAZN_NODE_BROKER_MAIN_RECEIPT_BACKUP="$root/ownership/control-plane.before.json" \
    BLAZN_CONTROL_API_BUILD_RECEIPT="$root/ownership/no-build-receipt" \
    "$UPGRADE"
}

for fault in secrets-installed role-ready receipt-bound; do
  root=$(fixture "$fault")
  if run_upgrade "$root" "$fault" >"$root/first.out" 2>"$root/first.err"; then
    printf 'injected fault unexpectedly completed: %s\n' "$fault" >&2
    exit 1
  fi
  grep -F "injected test fault after $fault" "$root/first.err" >/dev/null
  run_upgrade "$root" >"$root/retry.out"
  sudo jq -e '.phase=="receipt-bound"' "$root/ownership/node-broker-upgrade.json" >/dev/null
  sudo jq -e '.nodeBroker.schemaVersion=="blazn.dev/node-broker-infra/v1"' "$root/ownership/control-plane.json" >/dev/null
  before=$(sudo sha256sum "$root/etc/node-broker/secrets/database-url" "$root/etc/node-broker/secrets/enrollment-hmac-v1" "$root/etc/node-broker/secrets/join-credential-v1")
  run_upgrade "$root" >"$root/idempotent.out"
  after=$(sudo sha256sum "$root/etc/node-broker/secrets/database-url" "$root/etc/node-broker/secrets/enrollment-hmac-v1" "$root/etc/node-broker/secrets/join-credential-v1")
  [ "$before" = "$after" ] || { printf 'retry changed Node broker secrets: %s\n' "$fault" >&2; exit 1; }
  if grep -E 'postgresql://|[a-f0-9]{48,}' "$root"/*.out "$root"/*.err >/dev/null; then
    printf 'upgrade test output exposed secret material\n' >&2
    exit 1
  fi
done

trap - EXIT HUP INT TERM
cleanup
printf 'Node infrastructure fault-resumable upgrade tests passed\n'
