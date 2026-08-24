#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

require() {
  eval "value=\${$1-}"
  [ -n "$value" ] || fail "$1 is required"
}

valid_dns_label() {
  printf '%s\n' "$1" | LC_ALL=C grep -Eq '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
}

valid_key() {
  [ "${#1}" -le 253 ] && printf '%s\n' "$1" | LC_ALL=C grep -Eq '^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$'
}

valid_port() {
  case "$1" in ''|*[!0-9]*|0|0*) return 1 ;; esac
  [ "$1" -le 65535 ]
}

valid_image_component() {
  printf '%s\n' "$1" | LC_ALL=C grep -Eq '^[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*$'
}

valid_registry_host() {
  host=$1
  [ "${#host}" -le 253 ] || return 1
  while :; do
    label=${host%%.*}
    valid_dns_label "$label" || return 1
    [ "$label" = "$host" ] && break
    host=${host#*.}
  done
}

is_ipv4_address() {
  printf '%s\n' "$1" | awk -F. '
    NF != 4 { exit 1 }
    { for (i = 1; i <= 4; i++) if ($i !~ /^(0|[1-9][0-9]{0,2})$/ || $i + 0 > 255) exit 1 }
  '
}

valid_image_repository() {
  repository=$1
  [ -n "$repository" ] && [ "${#repository}" -le 255 ] || return 1
  case "$repository" in */*) ;; *) return 1 ;; esac
  case "$repository" in /*|*/|*//*) return 1 ;; esac
  first=1
  while :; do
    component=${repository%%/*}
    if [ "$first" -eq 1 ]; then
      case "$component" in
        *:*)
          host=${component%:*}
          port=${component##*:}
          valid_registry_host "$host" || return 1
          valid_port "$port" || return 1
          ;;
        *.*|localhost)
          valid_registry_host "$component" || return 1
          ;;
        *)
          return 1
          ;;
      esac
    else
      valid_image_component "$component" || return 1
    fi
    [ "$component" = "$repository" ] && break
    repository=${repository#*/}
    first=0
  done
}

valid_host_cidr() {
  case "$1" in */32) ;; *) return 1 ;; esac
  address=${1%/32}
  printf '%s\n' "$address" | awk -F. '
    NF != 4 { exit 1 }
    {
      for (i = 1; i <= 4; i++) {
        if ($i !~ /^(0|[1-9][0-9]{0,2})$/ || $i + 0 > 255) exit 1
      }
      if ($1 + 0 == 0 || $1 + 0 == 127 || $1 + 0 >= 224) exit 1
    }
  '
}

[ "$#" -eq 1 ] || { printf 'usage: %s OUTPUT\n' "$0" >&2; exit 64; }
output=$1
[ ! -e "$output" ] || fail "refusing to overwrite output: $output"
[ -d "$(dirname -- "$output")" ] || fail "output directory does not exist"

for name in \
  BLAZN_CONTROLLER_IMAGE \
  BLAZN_SANDBOX_IO_IMAGE \
  BLAZN_DATABASE_URL_SECRET_NAME \
  BLAZN_DATABASE_URL_SECRET_KEY \
  BLAZN_DATABASE_ENDPOINT_KIND \
  BLAZN_KUBERNETES_API_CIDR \
  BLAZN_KUBERNETES_API_PORT \
  BLAZN_KUBERNETES_API_AUDIENCE \
  BLAZN_BEN1_POSTGRES_CIDR \
  BLAZN_BEN1_POSTGRES_PORT \
  BLAZN_OBJECT_SECRET_NAME \
  BLAZN_OBJECT_ACCESS_KEY \
  BLAZN_OBJECT_SECRET_KEY \
  BLAZN_OBJECT_ENDPOINT_CIDR \
  BLAZN_OBJECT_ENDPOINT_PORT \
  BLAZN_OBJECT_REGION \
  BLAZN_OBJECT_BUCKET; do
  require "$name"
done
for name in BLAZN_SOURCE_HOST BLAZN_SOURCE_CIDR BLAZN_SOURCE_DNS_CIDR; do require "$name"; done

case "$BLAZN_CONTROLLER_IMAGE" in
  *@sha256:????????????????????????????????????????????????????????????????) ;;
  *) fail "BLAZN_CONTROLLER_IMAGE must be a full sha256 digest reference" ;;
esac
image_repository=${BLAZN_CONTROLLER_IMAGE%@sha256:*}
image_digest=${BLAZN_CONTROLLER_IMAGE##*@sha256:}
valid_image_repository "$image_repository" || fail "BLAZN_CONTROLLER_IMAGE repository is invalid"
printf '%s\n' "$image_digest" | LC_ALL=C grep -Eq '^[0-9a-f]{64}$' || fail "BLAZN_CONTROLLER_IMAGE digest is invalid"
image_name=${image_repository##*/}
case "$image_name" in *:*) fail "BLAZN_CONTROLLER_IMAGE must not contain a mutable tag" ;; esac

case "$BLAZN_SANDBOX_IO_IMAGE" in
  *@sha256:????????????????????????????????????????????????????????????????) ;;
  *) fail "BLAZN_SANDBOX_IO_IMAGE must be a full sha256 digest reference" ;;
esac
helper_repository=${BLAZN_SANDBOX_IO_IMAGE%@sha256:*}
helper_digest=${BLAZN_SANDBOX_IO_IMAGE##*@sha256:}
valid_image_repository "$helper_repository" || fail "BLAZN_SANDBOX_IO_IMAGE repository is invalid"
printf '%s\n' "$helper_digest" | LC_ALL=C grep -Eq '^[0-9a-f]{64}$' || fail "BLAZN_SANDBOX_IO_IMAGE digest is invalid"
helper_name=${helper_repository##*/}
case "$helper_name" in *:*) fail "BLAZN_SANDBOX_IO_IMAGE must not contain a mutable tag" ;; esac

valid_dns_label "$BLAZN_DATABASE_URL_SECRET_NAME" || fail "BLAZN_DATABASE_URL_SECRET_NAME is invalid"
valid_key "$BLAZN_DATABASE_URL_SECRET_KEY" || fail "BLAZN_DATABASE_URL_SECRET_KEY is invalid"
valid_dns_label "$BLAZN_OBJECT_SECRET_NAME" || fail "BLAZN_OBJECT_SECRET_NAME is invalid"
valid_key "$BLAZN_OBJECT_ACCESS_KEY" || fail "BLAZN_OBJECT_ACCESS_KEY is invalid"
valid_key "$BLAZN_OBJECT_SECRET_KEY" || fail "BLAZN_OBJECT_SECRET_KEY is invalid"
[ "$BLAZN_OBJECT_ACCESS_KEY" != "$BLAZN_OBJECT_SECRET_KEY" ] || fail "object credential keys must be distinct"
valid_host_cidr "$BLAZN_KUBERNETES_API_CIDR" || fail "BLAZN_KUBERNETES_API_CIDR must be one exact, usable IPv4 /32"
valid_host_cidr "$BLAZN_BEN1_POSTGRES_CIDR" || fail "BLAZN_BEN1_POSTGRES_CIDR must be one exact, usable IPv4 /32"
valid_host_cidr "$BLAZN_OBJECT_ENDPOINT_CIDR" || fail "BLAZN_OBJECT_ENDPOINT_CIDR must be one exact, usable IPv4 /32"
valid_dns_label "${BLAZN_SOURCE_HOST%%.*}" || fail "BLAZN_SOURCE_HOST is invalid"
valid_registry_host "$BLAZN_SOURCE_HOST" || fail "BLAZN_SOURCE_HOST is invalid"
valid_host_cidr "$BLAZN_SOURCE_CIDR" || fail "BLAZN_SOURCE_CIDR must be one exact, usable IPv4 /32"
valid_host_cidr "$BLAZN_SOURCE_DNS_CIDR" || fail "BLAZN_SOURCE_DNS_CIDR must be one exact, usable IPv4 /32"
valid_port "$BLAZN_KUBERNETES_API_PORT" || fail "BLAZN_KUBERNETES_API_PORT is invalid"
valid_port "$BLAZN_BEN1_POSTGRES_PORT" || fail "BLAZN_BEN1_POSTGRES_PORT is invalid"
valid_port "$BLAZN_OBJECT_ENDPOINT_PORT" || fail "BLAZN_OBJECT_ENDPOINT_PORT is invalid"
printf '%s\n' "$BLAZN_OBJECT_REGION" | LC_ALL=C grep -Eq '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$' || fail "BLAZN_OBJECT_REGION is invalid"
if [ "${#BLAZN_OBJECT_BUCKET}" -lt 3 ] || [ "${#BLAZN_OBJECT_BUCKET}" -gt 63 ] ||
   ! valid_registry_host "$BLAZN_OBJECT_BUCKET" || is_ipv4_address "$BLAZN_OBJECT_BUCKET"; then
  fail "BLAZN_OBJECT_BUCKET is invalid"
fi
printf '%s\n' "$BLAZN_KUBERNETES_API_AUDIENCE" | LC_ALL=C grep -Eq '^[A-Za-z0-9][A-Za-z0-9./:_-]{0,252}$' || fail "BLAZN_KUBERNETES_API_AUDIENCE is invalid"

case "$BLAZN_DATABASE_ENDPOINT_KIND" in
  ip)
    [ -z "${BLAZN_DNS_CIDR-}" ] || fail "BLAZN_DNS_CIDR must be absent for an IP database endpoint"
    ;;
  hostname)
    require BLAZN_DNS_CIDR
    valid_host_cidr "$BLAZN_DNS_CIDR" || fail "BLAZN_DNS_CIDR must be one exact, usable IPv4 /32"
    ;;
  *) fail "BLAZN_DATABASE_ENDPOINT_KIND must be ip or hostname" ;;
esac

api_host=${BLAZN_KUBERNETES_API_CIDR%/32}
object_host=${BLAZN_OBJECT_ENDPOINT_CIDR%/32}
tmp=$(mktemp "$(dirname -- "$output")/.blazn-phase5-controller.XXXXXX")
cleanup() { rm -f -- "$tmp"; }
trap cleanup EXIT HUP INT TERM

sed \
  -e "s|BLAZN_CONTROLLER_IMAGE|$BLAZN_CONTROLLER_IMAGE|g" \
  -e "s|BLAZN_SANDBOX_IO_IMAGE|$BLAZN_SANDBOX_IO_IMAGE|g" \
  -e "s|BLAZN_DATABASE_URL_SECRET_NAME|$BLAZN_DATABASE_URL_SECRET_NAME|g" \
  -e "s|BLAZN_DATABASE_URL_SECRET_KEY|$BLAZN_DATABASE_URL_SECRET_KEY|g" \
  -e "s|BLAZN_KUBERNETES_API_HOST|$api_host|g" \
  -e "s|BLAZN_KUBERNETES_API_CIDR|$BLAZN_KUBERNETES_API_CIDR|g" \
  -e "s|BLAZN_KUBERNETES_API_PORT|$BLAZN_KUBERNETES_API_PORT|g" \
  -e "s|BLAZN_KUBERNETES_API_AUDIENCE|$BLAZN_KUBERNETES_API_AUDIENCE|g" \
  -e "s|BLAZN_BEN1_POSTGRES_CIDR|$BLAZN_BEN1_POSTGRES_CIDR|g" \
  -e "s|BLAZN_BEN1_POSTGRES_PORT|$BLAZN_BEN1_POSTGRES_PORT|g" \
  -e "s|BLAZN_OBJECT_SECRET_NAME|$BLAZN_OBJECT_SECRET_NAME|g" \
  -e "s|BLAZN_OBJECT_ACCESS_KEY|$BLAZN_OBJECT_ACCESS_KEY|g" \
  -e "s|BLAZN_OBJECT_SECRET_KEY|$BLAZN_OBJECT_SECRET_KEY|g" \
  -e "s|BLAZN_OBJECT_ENDPOINT_HOST|$object_host|g" \
  -e "s|BLAZN_OBJECT_ENDPOINT_CIDR|$BLAZN_OBJECT_ENDPOINT_CIDR|g" \
  -e "s|BLAZN_OBJECT_ENDPOINT_PORT|$BLAZN_OBJECT_ENDPOINT_PORT|g" \
  -e "s|BLAZN_OBJECT_REGION|$BLAZN_OBJECT_REGION|g" \
  -e "s|BLAZN_OBJECT_BUCKET|$BLAZN_OBJECT_BUCKET|g" \
  -e "s|BLAZN_SOURCE_HOST|$BLAZN_SOURCE_HOST|g" \
  -e "s|BLAZN_SOURCE_CIDR|$BLAZN_SOURCE_CIDR|g" \
  -e "s|BLAZN_SOURCE_DNS_CIDR|$BLAZN_SOURCE_DNS_CIDR|g" \
  "$ROOT/controller.yaml.in" >"$tmp"

if [ "$BLAZN_DATABASE_ENDPOINT_KIND" = hostname ]; then
  sed "s|BLAZN_DNS_CIDR|$BLAZN_DNS_CIDR|g" "$ROOT/dns-egress.yaml.in" >>"$tmp"
fi
placeholder_pattern='BLAZN_CONTROLLER_IMAGE|BLAZN_SANDBOX_IO_IMAGE|BLAZN_DATABASE_URL_SECRET_NAME|BLAZN_DATABASE_URL_SECRET_KEY|BLAZN_KUBERNETES_API_HOST|BLAZN_KUBERNETES_API_CIDR|BLAZN_KUBERNETES_API_PORT|BLAZN_KUBERNETES_API_AUDIENCE|BLAZN_BEN1_POSTGRES_CIDR|BLAZN_BEN1_POSTGRES_PORT|BLAZN_DNS_CIDR|BLAZN_SOURCE_HOST|BLAZN_SOURCE_CIDR|BLAZN_SOURCE_DNS_CIDR|BLAZN_OBJECT_SECRET_NAME|BLAZN_OBJECT_ACCESS_KEY|BLAZN_OBJECT_SECRET_KEY|BLAZN_OBJECT_ENDPOINT_HOST|BLAZN_OBJECT_ENDPOINT_CIDR|BLAZN_OBJECT_ENDPOINT_PORT|BLAZN_OBJECT_REGION|BLAZN_OBJECT_BUCKET'
if LC_ALL=C grep -E "$placeholder_pattern" "$tmp" >/dev/null; then
  fail "rendered manifest contains an unresolved placeholder"
fi
chmod 0400 "$tmp"
mv -- "$tmp" "$output"
trap - EXIT HUP INT TERM
