FROM golang@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS go-dependencies
WORKDIR /opt/blazn-source
COPY go.mod go.sum ./
RUN go mod download

FROM docker.io/library/node@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90 AS dependencies
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y --no-install-recommends git ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=go-dependencies /usr/local/go /usr/local/go
COPY --from=go-dependencies /go/pkg/mod /go/pkg/mod
ENV PATH=/usr/local/go/bin:$PATH
COPY services/control-api/package.json services/control-api/package-lock.json /opt/blazn-control-api/
RUN npm ci --prefix /opt/blazn-control-api --ignore-scripts --no-audit --no-fund && \
    find /usr/local/lib/node_modules/npm -type d -path '*/node_modules/tar' -prune -exec rm -rf '{}' + && \
    rm -rf /root/.npm

FROM dependencies
COPY examples/blazn-development-sandbox/start.sh /usr/local/bin/blazn-development-sandbox
ENV HOME=/workspace/artifacts/home \
    TMPDIR=/workspace/artifacts/tmp \
    GOCACHE=/workspace/artifacts/cache/go-build \
    GOMODCACHE=/go/pkg/mod \
    NPM_CONFIG_CACHE=/workspace/artifacts/cache/npm
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/blazn-development-sandbox"]
