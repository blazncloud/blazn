#!/bin/sh
set -eu

# Build the control API from this checkout and roll it out to the Frontro
# cluster. Run from a coordinator that can SSH to both hosts:
#
#   infra/frontro/deploy-api.sh [--migrate] [--no-rollout] [--deployment api-dev]
#
#   BLAZN_BUILD_HOST    host with Docker and Go that builds and pushes (default ben5)
#   BLAZN_CLUSTER_HOST  host with MicroK8s admin kubectl (default ben1)
#
# Credentials are minted per run from Kubernetes service-account tokens and
# piped between processes; nothing secret is written outside tmpfs.

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
BUILD_HOST=${BLAZN_BUILD_HOST:-ben5}
CLUSTER_HOST=${BLAZN_CLUSTER_HOST:-ben1}
KUBECTL='sudo -n microk8s kubectl'
NAMESPACE=blazn-test
deployment=api-dev
migrate=false
rollout=true

while [ "$#" -gt 0 ]; do
  case "$1" in
    --migrate) migrate=true ;;
    --no-rollout) rollout=false ;;
    --deployment) shift; deployment=${1:?--deployment needs a name} ;;
    *) printf 'usage: %s [--migrate] [--no-rollout] [--deployment api-dev|api]\n' "$0" >&2; exit 64 ;;
  esac
  shift
done
case "$deployment" in
  api-dev) health=https://api.blazn.frontro.com/healthz ;;
  api) health=https://blazn-test.frontro.com/healthz ;;
  *) printf 'unknown deployment: %s\n' "$deployment" >&2; exit 64 ;;
esac

revision=$(git -C "$ROOT" rev-parse --short=12 HEAD)
if [ -n "$(git -C "$ROOT" status --porcelain -- services/control-api)" ]; then
  [ "${BLAZN_ALLOW_DIRTY:-}" = 1 ] || { printf 'services/control-api has uncommitted changes; commit them or set BLAZN_ALLOW_DIRTY=1\n' >&2; exit 1; }
  revision=$revision-dirty
fi
image=blazn-test-control-api:$revision
remote=blazn-deploy/$revision

printf 'building %s on %s\n' "$image" "$BUILD_HOST"
# shellcheck disable=SC2029
ssh "$BUILD_HOST" "mkdir -p $remote"
rsync -a --delete --exclude node_modules --exclude dist "$ROOT/services/control-api/" "$BUILD_HOST:$remote/control-api/"
rsync -a --delete "$ROOT/infra/frontro/scripts/" "$BUILD_HOST:$remote/scripts/"
# The Agent Run controller ships the in-Sandbox harness for both architectures.
rsync -a --delete "$ROOT/go.mod" "$ROOT/go.sum" "$BUILD_HOST:$remote/agent-src/"
rsync -a --delete "$ROOT/cmd/blazn-agent/" "$BUILD_HOST:$remote/agent-src/cmd/blazn-agent/"
rsync -a --delete "$ROOT/internal/sandboxagent/" "$BUILD_HOST:$remote/agent-src/internal/sandboxagent/"
# shellcheck disable=SC2029
ssh "$BUILD_HOST" "cd $remote/agent-src && for arch in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=\$arch go build -buildvcs=false -trimpath -ldflags='-s -w' -o ../control-api/agent/blazn-agent-linux-\$arch ./cmd/blazn-agent || exit 1; done"
# shellcheck disable=SC2029
ssh "$BUILD_HOST" "sudo -n docker build -q -t $image $remote/control-api >$remote/build.log 2>&1 || { tail -20 $remote/build.log; exit 1; }"

work=$(ssh "$CLUSTER_HOST" 'mktemp -d /dev/shm/blazn-deploy.XXXXXX')
# shellcheck disable=SC2029
trap 'ssh "$CLUSTER_HOST" "rm -rf $work"' EXIT
scp -q "$ROOT/infra/frontro/scripts/openbao-read.py" "$CLUSTER_HOST:$work/openbao-read.py"
# shellcheck disable=SC2029
ssh "$CLUSTER_HOST" "$KUBECTL -n $NAMESPACE get configmap openbao-client-ca -o jsonpath='{.data.ca\.crt}' > $work/ca.crt"
# shellcheck disable=SC2029
openbao=$(ssh "$CLUSTER_HOST" "$KUBECTL -n frontro-secrets get service openbao -o jsonpath='{.spec.clusterIP}'")

# read_secret SERVICE_ACCOUNT ROLE PATH: prints the secret as JSON on stdout.
read_secret() {
  # shellcheck disable=SC2029
  ssh "$CLUSTER_HOST" "$KUBECTL create token $1 -n $NAMESPACE --audience=openbao --duration=10m | python3 $work/openbao-read.py $openbao $work/ca.crt $2 $3"
}

if [ "$migrate" = true ]; then
  printf 'applying migrations\n'
  # shellcheck disable=SC2029
  ssh "$CLUSTER_HOST" "$KUBECTL -n $NAMESPACE get configmap api-public-ca -o jsonpath='{.data.postgres-ca\.crt}'" |
    ssh "$BUILD_HOST" "cat > $remote/postgres-ca.crt"
  # shellcheck disable=SC2029
  read_secret blazn-migrate blazn-test-migration apps/data/blazn/test/migration |
    ssh "$BUILD_HOST" "python3 $remote/scripts/migrate.py $image $remote/postgres-ca.crt"
fi

printf 'publishing\n'
# shellcheck disable=SC2029
reference=$(read_secret blazn-registry blazn-test-registry apps/data/blazn/test/registry |
  ssh "$BUILD_HOST" "python3 $remote/scripts/publish-image.py $image $revision")
printf 'published %s\n' "$reference"

if [ "$rollout" = false ]; then
  printf 'rollout skipped; deploy with:\n  %s -n %s set image deployment/%s api=%s\n' "$KUBECTL" "$NAMESPACE" "$deployment" "$reference"
  exit 0
fi

# shellcheck disable=SC2029
# The Agent Run controller, where deployed, runs the same image beside the API.
containers=api=$reference
# shellcheck disable=SC2029
if ssh "$CLUSTER_HOST" "$KUBECTL -n $NAMESPACE get deployment/$deployment -o jsonpath='{.spec.template.spec.containers[*].name}'" | tr ' ' '\n' | grep -qx agent-run-controller; then
  containers="$containers agent-run-controller=$reference"
fi
# shellcheck disable=SC2029
ssh "$CLUSTER_HOST" "$KUBECTL -n $NAMESPACE set image deployment/$deployment $containers >/dev/null && $KUBECTL -n $NAMESPACE rollout status deployment/$deployment --timeout=240s"
status=$(curl --noproxy '*' -fsS -m 20 "$health")
printf '%s\n' "$status"
case "$status" in
  *'"status":"ok"'*) ;;
  *) printf 'health check did not report ok\n' >&2; exit 1 ;;
esac
printf 'deployed. Record the new digest:\n  KUBECTL="ssh %s %s" infra/frontro/export.py && git add infra/frontro\n' "$CLUSTER_HOST" "$KUBECTL"
