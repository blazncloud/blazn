#!/bin/sh
set -eu

# Checks the public DNS for a Resend sending domain. Read-only.
#   infra/frontro/sender-domain/verify.sh [DOMAIN]
domain=${1:-mail.blazn.frontro.com}
resolver=${BLAZN_DNS_RESOLVER:-1.1.1.1}
failed=0

check() { # NAME TYPE PATTERN DESCRIPTION
  answer=$(dig +short "$2" "$1" "@$resolver" | tr '\n' ' ')
  if printf '%s' "$answer" | grep -Eq -- "$3"; then
    printf 'ok    %s\n' "$4"
  else
    printf 'FAIL  %s\n      %s %s -> %s\n' "$4" "$1" "$2" "${answer:-<nothing>}"
    failed=1
  fi
}

check "send.$domain" MX 'feedback\.' "return path has a bounce MX"
check "send.$domain" TXT 'v=spf1 ' "return path publishes SPF"
check "resend._domainkey.$domain" TXT 'p=[A-Za-z0-9+/]{100,}' "DKIM public key is published"
check "_dmarc.$domain" TXT 'v=DMARC1' "DMARC policy is published"
[ "$failed" -eq 0 ] && printf 'DNS for %s is ready; verify the domain in Resend next\n' "$domain"
exit "$failed"
