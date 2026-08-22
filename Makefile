SHELL := /bin/sh

.PHONY: fmt fmt-check test release test-release ci

fmt:
	go fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { \
		echo "Go files need formatting:" >&2; \
		gofmt -l . >&2; \
		exit 1; \
	}

test:
	go test ./...

release:
	./scripts/release.sh

test-release:
	./scripts/test-release.sh

ci: fmt-check test test-release
