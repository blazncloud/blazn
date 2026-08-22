#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INSTALLER=$TEST_DIR/../scripts/install-worker-issuer.sh
ROLLBACK=$TEST_DIR/../scripts/rollback-worker-issuer.sh
COMPOSE=$TEST_DIR/../../milestone-2/compose.yaml
command -v sudo >/dev/null 2>&1 || { printf 'worker issuer infra test skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'worker issuer infra test skipped: passwordless sudo unavailable\n'; exit 0; }
top=${TMPDIR:-/tmp}/blazn-worker-issuer-infra-$$
mkdir "$top"
cleanup(){ sudo find "$top" -xdev -type l -print | grep . >/dev/null && return 1; sudo find "$top" -xdev -type f -delete; sudo find "$top" -xdev -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM

helper=$top/helper
printf '#!/bin/sh\nexit 0\n' >"$helper"; chmod 0755 "$helper"
systemctl=$top/systemctl
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >>"$BLAZN_TEST_LOG"\n' >"$systemctl"; chmod 0755 "$systemctl"
tmpfiles=$top/tmpfiles
printf '#!/bin/sh\nexit 0\n' >"$tmpfiles"; chmod 0755 "$tmpfiles"

run_install(){
  root=$1; fault=${2:-}
  sudo env BLAZN_FENCING_TOKEN=test BLAZN_ISSUER_INFRA_TEST_MODE=1 BLAZN_ISSUER_TEST_FAIL_AFTER="$fault" BLAZN_TEST_LOG="$root/systemctl.log" \
    BLAZN_ISSUER_BINARY_SOURCE="$helper" BLAZN_NODE_BROKER_UID=65532 BLAZN_ISSUER_TEST_BROKER_GID=65531 BLAZN_ISSUER_TEST_MICROK8S_GID=1001 BLAZN_ISSUER_TEST_REVISION=9072 \
    BLAZN_ISSUER_TEST_SYSTEMCTL="$systemctl" BLAZN_ISSUER_TEST_TMPFILES="$tmpfiles" \
    BLAZN_ISSUER_CONFIG_ROOT="$root/etc/issuer" BLAZN_ISSUER_BINARY_PATH="$root/usr/libexec/issuer" BLAZN_ISSUER_UNIT_PATH="$root/etc/systemd/issuer.service" BLAZN_ISSUER_TMPFILES_PATH="$root/etc/tmpfiles/issuer.conf" \
    BLAZN_ISSUER_RECEIPT_PATH="$root/ownership/issuer.json" BLAZN_ISSUER_RECOVERY_ROOT="$root/ownership/recovery" "$INSTALLER"
}

for fault in initialized secret-created config-bound files-installed service-started complete; do
  root=$top/$fault; mkdir -p "$root/ownership"; : >"$root/systemctl.log"; sudo chown -R 0:0 "$root"; sudo chmod 0700 "$root" "$root/ownership"
  if run_install "$root" "$fault" >"$top/$fault.out" 2>"$top/$fault.err"; then printf 'issuer fault unexpectedly completed: %s\n' "$fault" >&2; exit 1; fi
  grep -F "injected issuer fault after $fault" "$top/$fault.err" >/dev/null
  run_install "$root" >"$top/$fault-retry.out"
  sudo jq -e '.phase=="complete" and .liveJoinBlocked==true and .secret.decodedBytes==32 and .socket.path=="/run/blazn/microk8s-worker-issuer.sock"' "$root/ownership/issuer.json" >/dev/null
  before=$(sudo sha256sum "$root/etc/issuer/issuer-hmac-v1" "$root/etc/issuer/config.json" "$root/usr/libexec/issuer")
  run_install "$root" >"$top/$fault-idempotent.out"
  after=$(sudo sha256sum "$root/etc/issuer/issuer-hmac-v1" "$root/etc/issuer/config.json" "$root/usr/libexec/issuer")
  [ "$before" = "$after" ] || { printf 'issuer retry changed receipt-bound material\n' >&2; exit 1; }
  [ "$(sudo stat -c '%u:%a' "$root/etc/issuer/issuer-hmac-v1")" = 0:400 ]
  [ "$(sudo sh -c 'wc -c <"$1"' sh "$root/etc/issuer/issuer-hmac-v1" | tr -d ' ')" = 43 ]
  sudo cmp "$root/etc/issuer/issuer-hmac-v1" "$root/ownership/recovery/issuer-hmac-v1"
  ! sudo grep -F "$(sudo sed -n '1p' "$root/etc/issuer/issuer-hmac-v1")" "$root/ownership/issuer.json" "$root/ownership/recovery/inventory.json" >/dev/null
  sudo env BLAZN_FENCING_TOKEN=test BLAZN_ISSUER_RECEIPT_PATH="$root/ownership/issuer.json" BLAZN_ISSUER_TEST_SYSTEMCTL="$systemctl" BLAZN_TEST_LOG="$root/systemctl.log" "$ROLLBACK" >/dev/null
  sudo jq -e '.phase=="rolled-back" and .rollback.retainedRecovery==true and .rollback.groupRetained==true' "$root/ownership/issuer.json" >/dev/null
  sudo test -f "$root/ownership/recovery/issuer-hmac-v1"
  sudo test ! -e "$root/usr/libexec/issuer"
done

grep -F 'source: /run/blazn/microk8s-worker-issuer.sock' "$COMPOSE" >/dev/null
grep -F 'target: /run/blazn/microk8s-worker-issuer.sock' "$COMPOSE" >/dev/null
grep -F 'network_mode: "service:api"' "$COMPOSE" >/dev/null
! sed -n '/  node-broker:/,/^  [a-z]/p' "$COMPOSE" | grep -E '/var/snap/microk8s|kubeconfig|docker.sock' >/dev/null
trap - EXIT HUP INT TERM
cleanup
printf 'worker issuer journal, secret encoding, and narrow Compose boundary passed\n'
