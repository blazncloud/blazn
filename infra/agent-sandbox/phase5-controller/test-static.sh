#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH='' cd -- "$ROOT/../../.." && pwd)
RENDER=$ROOT/render-install.sh
IMAGE='registry.example/blazn/sandbox-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
HELPER_IMAGE='registry.example/blazn/sandbox-io@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-controller-static.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

render() {
  output=$1
  shift
  env \
    BLAZN_CONTROLLER_IMAGE="$IMAGE" \
    BLAZN_SANDBOX_IO_IMAGE="$HELPER_IMAGE" \
    BLAZN_DATABASE_URL_SECRET_NAME=controller-db-url \
    BLAZN_DATABASE_URL_SECRET_KEY=database-url \
    BLAZN_DATABASE_ENDPOINT_KIND=ip \
    BLAZN_KUBERNETES_API_CIDR=10.20.30.40/32 \
    BLAZN_KUBERNETES_API_PORT=16443 \
    BLAZN_KUBERNETES_API_AUDIENCE=https://kubernetes.default.svc \
    BLAZN_BEN1_POSTGRES_CIDR=10.20.30.41/32 \
    BLAZN_BEN1_POSTGRES_PORT=5432 \
    BLAZN_OBJECT_SECRET_NAME=controller-object-credentials \
    BLAZN_OBJECT_ACCESS_KEY=access-key \
    BLAZN_OBJECT_SECRET_KEY=secret-key \
    BLAZN_OBJECT_ENDPOINT_CIDR=10.20.30.42/32 \
    BLAZN_OBJECT_ENDPOINT_PORT=9443 \
    BLAZN_OBJECT_REGION=us-test-1 \
    BLAZN_OBJECT_BUCKET=blazn-artifacts \
    BLAZN_SOURCE_HOST=github.com \
    BLAZN_SOURCE_CIDR=140.82.112.4/32 \
    BLAZN_SOURCE_DNS_CIDR=10.20.30.53/32 \
    BLAZN_DNS_CIDR= \
    "$@" "$RENDER" "$output"
}

expect_fail() {
  name=$1
  shift
  if render "$tmp/rejected-$name.yaml" "$@" >/dev/null 2>&1; then
    printf 'renderer accepted unsafe case: %s\n' "$name" >&2
    exit 1
  fi
}

for script in "$ROOT"/*.sh; do sh -n "$script"; done
render "$tmp/ip.yaml"
[ "$(stat -c '%a' "$tmp/ip.yaml")" = 400 ]
[ "$(grep -c '^kind: ' "$tmp/ip.yaml")" -eq 6 ]
[ "$(grep -Fxc "        image: $IMAGE" "$tmp/ip.yaml")" -eq 2 ]
grep -F '  replicas: 0' "$tmp/ip.yaml" >/dev/null
grep -F '  namespace: blazn-poc-system' "$tmp/ip.yaml" >/dev/null
grep -F '  namespace: blazn-poc-sandboxes' "$tmp/ip.yaml" >/dev/null
grep -F 'automountServiceAccountToken: false' "$tmp/ip.yaml" >/dev/null
grep -F "          value: \"$HELPER_IMAGE\"" "$tmp/ip.yaml" >/dev/null
grep -F 'expirationSeconds: 600' "$tmp/ip.yaml" >/dev/null
grep -F 'audience: https://kubernetes.default.svc' "$tmp/ip.yaml" >/dev/null
grep -F 'command: ["/blazn-sandbox-controller-secret-init"]' "$tmp/ip.yaml" >/dev/null
[ "$(grep -Fxc '        - /var/run/blazn-api-ca/ca.crt' "$tmp/ip.yaml")" -eq 1 ]
[ "$(grep -Fxc '        - /var/run/blazn-private/kubernetes-ca.crt' "$tmp/ip.yaml")" -eq 1 ]
[ "$(grep -Fxc '        - /var/run/blazn-private/object-access' "$tmp/ip.yaml")" -eq 1 ]
[ "$(grep -Fxc '        - /var/run/blazn-private/object-secret' "$tmp/ip.yaml")" -eq 1 ]
grep -F 'mountPath: /var/run/blazn-private' "$tmp/ip.yaml" >/dev/null
[ "$(grep -Fxc '          mountPath: /var/run/blazn-api-ca' "$tmp/ip.yaml")" -eq 1 ]
[ "$(grep -Fxc '          value: /var/run/blazn-private/kubernetes-ca.crt' "$tmp/ip.yaml")" -eq 1 ]
grep -F 'medium: Memory' "$tmp/ip.yaml" >/dev/null
grep -F 'sizeLimit: 2Mi' "$tmp/ip.yaml" >/dev/null
grep -F 'readOnlyRootFilesystem: true' "$tmp/ip.yaml" >/dev/null
grep -F 'allowPrivilegeEscalation: false' "$tmp/ip.yaml" >/dev/null
grep -F 'drop: ["ALL"]' "$tmp/ip.yaml" >/dev/null
grep -F 'type: RuntimeDefault' "$tmp/ip.yaml" >/dev/null
grep -F 'cidr: 10.20.30.40/32' "$tmp/ip.yaml" >/dev/null
grep -F 'cidr: 10.20.30.41/32' "$tmp/ip.yaml" >/dev/null
grep -F 'cidr: 10.20.30.42/32' "$tmp/ip.yaml" >/dev/null
grep -F 'value: "https://10.20.30.42:9443"' "$tmp/ip.yaml" >/dev/null
grep -F 'value: "10.20.30.40"' "$tmp/ip.yaml" >/dev/null
grep -F 'value: "10.20.30.53/32"' "$tmp/ip.yaml" >/dev/null
grep -F 'value: '\''{"github.com":["140.82.112.4/32"]}'\''' "$tmp/ip.yaml" >/dev/null
[ "$(grep -c 'cidr: ' "$tmp/ip.yaml")" -eq 3 ]
placeholder_pattern='BLAZN_CONTROLLER_IMAGE|BLAZN_SANDBOX_IO_IMAGE|BLAZN_DATABASE_URL_SECRET_NAME|BLAZN_DATABASE_URL_SECRET_KEY|BLAZN_KUBERNETES_API_HOST|BLAZN_KUBERNETES_API_CIDR|BLAZN_KUBERNETES_API_PORT|BLAZN_KUBERNETES_API_AUDIENCE|BLAZN_BEN1_POSTGRES_CIDR|BLAZN_BEN1_POSTGRES_PORT|BLAZN_DNS_CIDR|BLAZN_SOURCE_HOST|BLAZN_SOURCE_CIDR|BLAZN_SOURCE_DNS_CIDR|BLAZN_OBJECT_SECRET_NAME|BLAZN_OBJECT_ACCESS_KEY|BLAZN_OBJECT_SECRET_KEY|BLAZN_OBJECT_ENDPOINT_HOST|BLAZN_OBJECT_ENDPOINT_CIDR|BLAZN_OBJECT_ENDPOINT_PORT|BLAZN_OBJECT_REGION|BLAZN_OBJECT_BUCKET'
if grep -E "$placeholder_pattern" "$tmp/ip.yaml" >/dev/null; then
  printf 'render left an unresolved placeholder\n' >&2
  exit 1
fi
if grep -F 'port: 53' "$tmp/ip.yaml" >/dev/null; then
  printf 'IP database render unexpectedly enables DNS\n' >&2
  exit 1
fi

port_image='registry.example:5000/blazn/sandbox-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
render "$tmp/registry-port.yaml" BLAZN_CONTROLLER_IMAGE="$port_image"
[ "$(grep -Fxc "        image: $port_image" "$tmp/registry-port.yaml")" -eq 2 ]

portless_image='registry.example/blazn/sandbox-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
render "$tmp/registry-portless.yaml" BLAZN_CONTROLLER_IMAGE="$portless_image"
[ "$(grep -Fxc "        image: $portless_image" "$tmp/registry-portless.yaml")" -eq 2 ]

localhost_image='localhost/blazn/sandbox-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
render "$tmp/registry-localhost.yaml" BLAZN_CONTROLLER_IMAGE="$localhost_image"
[ "$(grep -Fxc "        image: $localhost_image" "$tmp/registry-localhost.yaml")" -eq 2 ]

repository_component_250=$(printf '%0250d' 0 | tr 0 a)
repository_255="r.io/$repository_component_250"
[ "${#repository_255}" -eq 255 ]
repository_255_image="$repository_255@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
render "$tmp/repository-255.yaml" BLAZN_CONTROLLER_IMAGE="$repository_255_image"
[ "$(grep -Fxc "        image: $repository_255_image" "$tmp/repository-255.yaml")" -eq 2 ]

render "$tmp/hostname.yaml" BLAZN_DATABASE_ENDPOINT_KIND=hostname BLAZN_DNS_CIDR=10.20.30.53/32
grep -F 'cidr: 10.20.30.53/32' "$tmp/hostname.yaml" >/dev/null
[ "$(grep -Fc 'port: 53' "$tmp/hostname.yaml")" -eq 2 ]
[ "$(grep -c 'cidr: ' "$tmp/hostname.yaml")" -eq 4 ]
grep -F 'protocol: UDP' "$tmp/hostname.yaml" >/dev/null
grep -F 'protocol: TCP' "$tmp/hostname.yaml" >/dev/null

expect_fail missing-image BLAZN_CONTROLLER_IMAGE=
expect_fail missing-helper BLAZN_SANDBOX_IO_IMAGE=
expect_fail tag-only BLAZN_CONTROLLER_IMAGE=registry.example/blazn/sandbox-controller:latest
expect_fail tag-plus-digest BLAZN_CONTROLLER_IMAGE=registry.example/blazn/sandbox-controller:v1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail empty-repository-segment BLAZN_CONTROLLER_IMAGE=registry.example//blazn/sandbox-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail trailing-repository-slash BLAZN_CONTROLLER_IMAGE=registry.example/blazn/@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail invalid-repository-component BLAZN_CONTROLLER_IMAGE=registry.example/blazn/-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail unqualified-repository BLAZN_CONTROLLER_IMAGE=blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail registry-without-repository BLAZN_CONTROLLER_IMAGE=registry.example@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail underscore-portless-registry-host BLAZN_CONTROLLER_IMAGE=registry_example.com/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail consecutive-dot-portless-registry-host BLAZN_CONTROLLER_IMAGE=registry..example/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail leading-hyphen-portless-registry-host BLAZN_CONTROLLER_IMAGE=-registry.example/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail trailing-hyphen-portless-registry-host BLAZN_CONTROLLER_IMAGE=registry-.example/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail oversized-portless-registry-label BLAZN_CONTROLLER_IMAGE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail empty-registry-host BLAZN_CONTROLLER_IMAGE=:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail leading-dot-registry-host BLAZN_CONTROLLER_IMAGE=.registry.example:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail trailing-dot-registry-host BLAZN_CONTROLLER_IMAGE=registry.example.:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail consecutive-dot-registry-host BLAZN_CONTROLLER_IMAGE=registry..example:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail leading-hyphen-registry-label BLAZN_CONTROLLER_IMAGE=-registry.example:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail trailing-hyphen-registry-label BLAZN_CONTROLLER_IMAGE=registry-.example:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail oversized-registry-label BLAZN_CONTROLLER_IMAGE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail ipv6-registry-host BLAZN_CONTROLLER_IMAGE='[2001:db8::1]:5000/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
expect_fail nonnumeric-registry-port BLAZN_CONTROLLER_IMAGE=registry.example:https/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_fail invalid-registry-port BLAZN_CONTROLLER_IMAGE=registry.example:65536/blazn/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
repository_component_251="${repository_component_250}a"
repository_256="r.io/$repository_component_251"
[ "${#repository_256}" -eq 256 ]
expect_fail oversized-repository BLAZN_CONTROLLER_IMAGE="$repository_256@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
expect_fail uppercase-digest BLAZN_CONTROLLER_IMAGE=registry.example/blazn/sandbox-controller@sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
expect_fail broad-api BLAZN_KUBERNETES_API_CIDR=10.20.30.0/24
expect_fail broad-database BLAZN_BEN1_POSTGRES_CIDR=10.20.30.0/24
expect_fail unspecified-api BLAZN_KUBERNETES_API_CIDR=0.0.0.0/32
expect_fail loopback-database BLAZN_BEN1_POSTGRES_CIDR=127.0.0.1/32
expect_fail bad-api-port BLAZN_KUBERNETES_API_PORT=0
expect_fail broad-database-port BLAZN_BEN1_POSTGRES_PORT=1-65535
expect_fail overflow-port BLAZN_BEN1_POSTGRES_PORT=65536
expect_fail hostname-without-dns BLAZN_DATABASE_ENDPOINT_KIND=hostname
expect_fail broad-dns BLAZN_DATABASE_ENDPOINT_KIND=hostname BLAZN_DNS_CIDR=10.20.30.0/24
expect_fail dns-in-ip-mode BLAZN_DNS_CIDR=10.20.30.53/32
expect_fail bad-secret-name BLAZN_DATABASE_URL_SECRET_NAME=Bad_Name
expect_fail missing-source-host BLAZN_SOURCE_HOST=
expect_fail uppercase-source-host BLAZN_SOURCE_HOST=GitHub.com
expect_fail broad-source-cidr BLAZN_SOURCE_CIDR=140.82.112.0/24
expect_fail broad-source-dns BLAZN_SOURCE_DNS_CIDR=10.20.30.0/24
expect_fail missing-object-secret BLAZN_OBJECT_SECRET_NAME=
expect_fail same-object-keys BLAZN_OBJECT_ACCESS_KEY=credential BLAZN_OBJECT_SECRET_KEY=credential
expect_fail broad-object-cidr BLAZN_OBJECT_ENDPOINT_CIDR=10.20.30.0/24
expect_fail bad-object-port BLAZN_OBJECT_ENDPOINT_PORT=0
expect_fail bad-object-region BLAZN_OBJECT_REGION=US_TEST
expect_fail bad-object-bucket BLAZN_OBJECT_BUCKET=Bad_Bucket

touch "$tmp/existing.yaml"
if render "$tmp/existing.yaml" >/dev/null 2>&1; then
  printf 'renderer overwrote an existing output\n' >&2
  exit 1
fi

if grep -E '(^|[^[:alnum:]_])(kubectl|microk8s|helm)[[:space:]]+(apply|create|delete|edit|label|patch|replace|scale)' "$RENDER" >/dev/null; then
  printf 'renderer contains a mutation command\n' >&2
  exit 1
fi
if grep -E '^kind: (Namespace|Secret|CustomResourceDefinition|ValidatingWebhookConfiguration|MutatingWebhookConfiguration|ValidatingAdmissionPolicy|ValidatingAdmissionPolicyBinding)$' "$ROOT"/*.yaml.in >/dev/null; then
  printf 'template contains forbidden cluster/admission/secret mutation\n' >&2
  exit 1
fi
if grep -F 'hostPath:' "$ROOT/controller.yaml.in" >/dev/null; then
  printf 'template contains a host mount\n' >&2
  exit 1
fi
if grep -E 'hostPort:|hostAliases:|hostUsers:|hostNetwork: true|hostPID: true|hostIPC: true' "$ROOT/controller.yaml.in" >/dev/null; then
  printf 'template contains forbidden host coupling\n' >&2
  exit 1
fi
if grep -E 'resources: \["(secrets|nodes|runtimeclasses|customresourcedefinitions|validatingwebhookconfigurations|mutatingwebhookconfigurations)"\]' "$ROOT/controller.yaml.in" >/dev/null; then
  printf 'template grants forbidden resources\n' >&2
  exit 1
fi
if grep -F 'resources: ["sandboxes/status"]' "$ROOT/controller.yaml.in" >/dev/null; then
  printf 'template grants unused Sandbox status subresource authority\n' >&2
  exit 1
fi
if grep -F '["*"]' "$ROOT/controller.yaml.in" >/dev/null; then
  printf 'template grants wildcard authority\n' >&2
  exit 1
fi
grep -F 'resources: ["sandboxes"]' "$ROOT/controller.yaml.in" >/dev/null
grep -F 'verbs: ["create", "delete", "get", "list", "patch"]' "$ROOT/controller.yaml.in" >/dev/null
grep -F 'resources: ["pods"]' "$ROOT/controller.yaml.in" >/dev/null
grep -F 'resources: ["pods/exec"]' "$ROOT/controller.yaml.in" >/dev/null
grep -F 'resources: ["workloads"]' "$ROOT/controller.yaml.in" >/dev/null
grep -F 'resources: ["networkpolicies"]' "$ROOT/controller.yaml.in" >/dev/null
[ "$(grep -Fxc '  verbs: ["create", "delete", "get", "list", "patch"]' "$ROOT/controller.yaml.in")" -eq 1 ]
[ "$(grep -Fxc '  verbs: ["get", "list"]' "$ROOT/controller.yaml.in")" -eq 1 ]
[ "$(grep -Fxc '  verbs: ["create"]' "$ROOT/controller.yaml.in")" -eq 1 ]
[ "$(grep -Fxc '  verbs: ["list"]' "$ROOT/controller.yaml.in")" -eq 1 ]
[ "$(grep -Fxc '  verbs: ["create", "delete", "get", "list"]' "$ROOT/controller.yaml.in")" -eq 1 ]
[ "$(grep -c '^  resources: ' "$ROOT/controller.yaml.in")" -eq 5 ]
[ "$(grep -c '^  verbs: ' "$ROOT/controller.yaml.in")" -eq 5 ]
[ "$(grep -c '^kind: Role$' "$ROOT/controller.yaml.in")" -eq 1 ]
[ "$(grep -c '^kind: RoleBinding$' "$ROOT/controller.yaml.in")" -eq 1 ]
if grep -E '^kind: ClusterRole(Binding)?$' "$ROOT/controller.yaml.in" >/dev/null; then
  printf 'template grants cluster-scoped RBAC\n' >&2
  exit 1
fi

# These assertions intentionally match literal Dockerfile build arguments.
# shellcheck disable=SC2016
grep -F 'FROM --platform=$BUILDPLATFORM golang:1.26.2-bookworm@sha256:' "$REPO/Dockerfile.sandbox-controller" >/dev/null
# shellcheck disable=SC2016
grep -F 'CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH' "$REPO/Dockerfile.sandbox-controller" >/dev/null
grep -F 'FROM scratch' "$REPO/Dockerfile.sandbox-controller" >/dev/null
grep -F 'USER 65532:65532' "$REPO/Dockerfile.sandbox-controller" >/dev/null
grep -F 'ENTRYPOINT ["/blazn-sandbox-controller"]' "$REPO/Dockerfile.sandbox-controller" >/dev/null
if grep -E '^FROM [^@[:space:]]+:[^@[:space:]]+([[:space:]]|$)' "$REPO/Dockerfile.sandbox-controller" >/dev/null; then
  printf 'Dockerfile contains a mutable base image\n' >&2
  exit 1
fi
if grep -E '(^|[[:space:]])(apt|apt-get|apk|dnf|yum|curl|wget)([[:space:]]|$)' "$REPO/Dockerfile.sandbox-controller" >/dev/null; then
  printf 'Dockerfile performs an unreviewed package or network operation\n' >&2
  exit 1
fi

printf 'Phase 5 controller deployment static audit passed\n'
