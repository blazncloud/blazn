SHELL := /bin/sh

.PHONY: fmt fmt-check generate-client check-generated test release test-release test-install ci

fmt:
	go fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { \
		echo "Go files need formatting:" >&2; \
		gofmt -l . >&2; \
		exit 1; \
	}

generate-client:
	go run ./cmd/generate-client

check-generated:
	go run ./cmd/generate-client --check

test:
	go test ./...

release:
	./scripts/release.sh

test-release:
	./scripts/test-release.sh

test-install:
	./scripts/test-install.sh

ci: fmt-check check-generated test test-release test-install
