#!/bin/sh
set -eu

# (Re)start the node broker on the cluster host that runs the MicroK8s worker
# issuer. Run on that host, with an image built from this repository's
# services/control-api (the same image the API uses):
#
#   sudo infra/frontro/node-broker.sh blazn-test-control-api:<revision>
#
# The broker listens only on the private address below, authenticates the API
# with the caller key, reaches the issuer through its Unix socket, and reads
# its secrets from a root-only directory:
#   caller-key  database-url  join-credential-v1  postgres-ca.crt

image=${1:?usage: node-broker.sh IMAGE}
BIND=${BLAZN_NODE_BROKER_BIND:-192.168.0.100}
SECRETS=${BLAZN_NODE_BROKER_SECRETS:-/etc/blazn/node-broker-frontro/secrets}
DATABASE_HOST=${BLAZN_DATABASE_HOST:-frontro-db-1:192.168.0.105}

[ "$(id -u)" -eq 0 ] || { printf 'run as root\n' >&2; exit 1; }
for name in caller-key database-url join-credential-v1 postgres-ca.crt; do
  [ -f "$SECRETS/$name" ] || { printf 'missing broker secret: %s\n' "$name" >&2; exit 1; }
done
[ -S /run/blazn/microk8s-worker-issuer.sock ] || { printf 'worker issuer socket is absent; start blazn-microk8s-worker-issuer first\n' >&2; exit 1; }

docker rm -f blazn-node-broker >/dev/null 2>&1 || :
docker run -d --name blazn-node-broker --restart unless-stopped --network host \
  --user 65532:65532 --add-host "$DATABASE_HOST" \
  -v /run/blazn:/run/blazn -v "$SECRETS:/run/secrets:ro" \
  -e NODE_ENV=production -e NODE_BROKER_BIND="$BIND" -e NODE_BROKER_PORT=18081 \
  -e BLAZN_NODE_BROKER_SECRETS_ROOT=/run/secrets \
  -e BLAZN_NODE_BROKER_CALLER_KEY_FILE=/run/secrets/caller-key \
  "$image" node dist/node-broker-main.js >/dev/null
sleep 5
docker ps --filter name=blazn-node-broker --format 'blazn-node-broker {{.Status}} ({{.Image}})'
