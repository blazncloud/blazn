#!/bin/sh
set -eu

die() { printf 'blazn-node-infra: %s\n' "$*" >&2; exit 1; }

[ "$#" -eq 3 ] || die "usage: validate-secret-paths.sh SECRETS JOURNAL TEST_MODE"
secrets=$1
journal=$2
test_mode=$3
case "$test_mode" in 0|1) ;; *) die "Node infrastructure test mode is invalid" ;; esac

if [ "$test_mode" != 1 ]; then
  [ "$secrets" = /etc/blazn/node-broker/secrets ] || die "node broker secrets root is outside the reviewed path"
  case "$journal" in
    /var/lib/blazn/ownership/node-broker-secret-create.json|/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json) ;;
    *) die "node broker create journal is outside the reviewed path" ;;
  esac
fi
case "$(dirname -- "$secrets"):$journal" in /*:/*) ;; *) die "secret and journal paths must be absolute" ;; esac
