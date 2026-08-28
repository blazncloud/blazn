FROM golang@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS go-dependencies
WORKDIR /opt/blazn-source
COPY go.mod go.sum ./
RUN go mod download

FROM docker.io/library/node@sha256:d2166de198f26e17e5a442f537754dd616ab069c47cc57b889310a717e0abbf9 AS dependencies
RUN apk upgrade --no-cache && apk add --no-cache git ca-certificates
COPY --from=go-dependencies /usr/local/go /usr/local/go
COPY --from=go-dependencies /go/pkg/mod /go/pkg/mod
ENV PATH=/usr/local/go/bin:$PATH
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go && \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
COPY services/control-api/package.json services/control-api/package-lock.json /opt/blazn-control-api/
RUN npm ci --prefix /opt/blazn-control-api --ignore-scripts --no-audit --no-fund && \
    npm install --prefix /tmp/npm-tar-patch --ignore-scripts --no-audit --no-fund tar@7.5.19 && \
    rm -rf /usr/local/lib/node_modules/npm/node_modules/tar && \
    mv /tmp/npm-tar-patch/node_modules/tar /usr/local/lib/node_modules/npm/node_modules/tar && \
    rm -rf /tmp/npm-tar-patch /root/.npm

FROM dependencies
COPY examples/blazn-development-sandbox/start.sh /usr/local/bin/blazn-development-sandbox
ENV HOME=/workspace/artifacts/home \
    USER=blazn \
    LOGNAME=blazn \
    TMPDIR=/workspace/artifacts/tmp \
    GOCACHE=/workspace/artifacts/cache/go-build \
    GOMODCACHE=/go/pkg/mod \
    NPM_CONFIG_CACHE=/workspace/artifacts/cache/npm
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/blazn-development-sandbox"]
