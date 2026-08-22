#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || { printf 'transaction sealing must run as root\n' >&2; exit 1; }
[ "$#" -eq 4 ] || { printf 'usage: %s INSTALL_BUNDLE FIXTURE_DIRECTORY PREINSTALL_INVENTORY TRANSACTION_DIRECTORY\n' "$0" >&2; exit 64; }
install_bundle=$1
fixtures=$2
pre=$3
transaction=$4
[ ! -e "$transaction" ] || { printf 'refusing to overwrite transaction directory\n' >&2; exit 1; }

fixture_files='blazn-poc.yaml
bootstrap.yaml
controller-boundary.yaml
synthetic-canary.yaml'
inventory_files='admission.json
api-resources.txt
clusterqueues.json
context
creator-principal
inventory.sha256
kube-system.uid
phase4c-targets
relevant-crds.txt
runtimeclasses.json
version.json'

check_closed() {
  directory=$1
  expected=$2
  actual=$(find "$directory" -mindepth 1 -maxdepth 1 -type f -links 1 -printf '%f\n' | LC_ALL=C sort)
  [ "$actual" = "$expected" ] || { printf 'input directory is not the closed reviewed set: %s\n' "$directory" >&2; exit 1; }
  [ -z "$(find "$directory" -mindepth 1 -maxdepth 1 ! -type f -print -quit)" ] || { printf 'input directory contains a link or non-file\n' >&2; exit 1; }
}

if [ ! -f "$install_bundle" ] || [ -L "$install_bundle" ] || [ "$(stat -c '%h' "$install_bundle")" != 1 ]; then printf 'install bundle is unsafe\n' >&2; exit 1; fi
check_closed "$fixtures" "$fixture_files"
check_closed "$pre" "$inventory_files"
(cd "$pre" && sha256sum -c inventory.sha256 >/dev/null)

mkdir -m 0700 "$transaction"
mkdir -m 0700 "$transaction/fixtures" "$transaction/pre"
install -o root -g root -m 0400 "$install_bundle" "$transaction/install.yaml"
for file in $fixture_files; do install -o root -g root -m 0400 "$fixtures/$file" "$transaction/fixtures/$file"; done
for file in $inventory_files; do install -o root -g root -m 0400 "$pre/$file" "$transaction/pre/$file"; done
(
  cd "$transaction"
  sha256sum install.yaml fixtures/* pre/* | LC_ALL=C sort >input.sha256
  sha256sum input.sha256 | cut -d' ' -f1 >input.digest
)
chmod 0400 "$transaction/input.sha256" "$transaction/input.digest"
printf 'sealed\n' >"$transaction/phase"
chmod 0600 "$transaction/phase"
sync -f "$transaction/phase"
sync -f "$transaction"
printf 'Sealed Phase 4C transaction; separately review and approve digest %s\n' "$(cat "$transaction/input.digest")"
