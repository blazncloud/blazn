#!/bin/sh
set -eu

die() { printf 'blazn-node-infra: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "secret provisioning must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "secret provisioning must run through the control-plane lock"
command -v openssl >/dev/null 2>&1 || die "openssl is required"

root=${BLAZN_NODE_BROKER_SECRETS_ROOT:-/etc/blazn/node-broker/secrets}
case "$root" in /etc/blazn/node-broker/secrets) ;; *) die "node broker secrets root is outside the reviewed path" ;; esac
[ ! -e /etc/blazn/node-broker ] || die "node broker secret boundary already exists"

umask 077
mkdir -p -- "$root"
chmod 0700 /etc/blazn/node-broker "$root"
password=$(openssl rand -hex 32)
printf 'postgresql://blazn_node_broker:%s@postgres:5432/%s\n' "$password" "${POSTGRES_DB:-blazn}" >"$root/database-url"
openssl rand 32 >"$root/enrollment-hmac-v1"
openssl rand 32 >"$root/join-credential-v1"
chmod 0444 "$root/database-url"
chmod 0400 "$root/enrollment-hmac-v1" "$root/join-credential-v1"
printf 'created root-owned Node broker secrets under %s\n' "$root"
