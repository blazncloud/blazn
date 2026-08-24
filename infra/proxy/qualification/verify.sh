#!/usr/bin/env bash
# shellcheck source=infra/proxy/qualification/lib/common.sh
. "$(dirname -- "$0")/lib/common.sh"
proxy_qual_exec verify "$@"
