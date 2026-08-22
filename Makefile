SHELL := /bin/sh

.PHONY: fmt fmt-check test test-control-api test-infra release test-release test-install ci

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

test-control-api:
	./scripts/test-control-api.sh

test-infra:
	./infra/milestone-2/tests/test-preflight.sh
	./infra/milestone-2/tests/test-contract.sh
	shellcheck infra/milestone-2/scripts/*.sh infra/milestone-2/tests/*.sh

release:
	./scripts/release.sh

test-release:
	./scripts/test-release.sh

test-install:
	./scripts/test-install.sh

ci: fmt-check test test-release test-install
