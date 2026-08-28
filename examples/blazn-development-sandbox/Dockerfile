FROM golang@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS go-dependencies
WORKDIR /opt/blazn-source
COPY go.mod go.sum ./
RUN go mod download

FROM docker.io/library/node@sha256:afff6d8c97964a438d2e6a9c96509367e45d8bf93f790ad561a1eaea926303d9 AS dependencies
COPY --from=go-dependencies /usr/local/go /usr/local/go
COPY --from=go-dependencies /go/pkg/mod /go/pkg/mod
ENV PATH=/usr/local/go/bin:$PATH
COPY services/control-api/package.json services/control-api/package-lock.json /opt/blazn-control-api/
RUN npm ci --prefix /opt/blazn-control-api --ignore-scripts --no-audit --no-fund && \
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
