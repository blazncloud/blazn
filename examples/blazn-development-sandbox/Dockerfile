FROM docker.io/library/node@sha256:d2166de198f26e17e5a442f537754dd616ab069c47cc57b889310a717e0abbf9 AS node

FROM golang@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS dependencies
WORKDIR /opt/blazn-source
COPY go.mod go.sum ./
RUN go mod download
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
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
