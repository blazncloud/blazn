SHELL := /bin/sh

.PHONY: fmt fmt-check generate-client check-generated generate-workspace-client check-workspace-generated generate-project-client check-project-generated generate-proxy-contract check-proxy-generated generate-node-client check-node-generated generate-sandbox-client check-sandbox-generated test test-control-api test-infra test-sandbox-contract test-project-contract test-project-postgres test-sandbox-postgres release test-release test-install ci

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

generate-workspace-client:
	go run ./cmd/generate-workspace-client

check-workspace-generated:
	go run ./cmd/generate-workspace-client --check

generate-project-client:
	go run ./cmd/generate-project-client

check-project-generated:
	go run ./cmd/generate-project-client --check

generate-node-client:
	go run ./cmd/generate-node-client

check-node-generated:
	go run ./cmd/generate-node-client --check

generate-sandbox-client:
	go run ./cmd/generate-sandbox-client

check-sandbox-generated:
	go run ./cmd/generate-sandbox-client --check

generate-proxy-contract:
	go run ./cmd/generate-proxy-contract

check-proxy-generated:
	go run ./cmd/generate-proxy-contract --check

test:
	go test ./...

test-control-api:
	./scripts/test-control-api.sh

test-infra:
	./infra/milestone-2/tests/test-preflight.sh
	./infra/milestone-2/tests/test-contract.sh
	./infra/milestone-2/tests/test-live-upgrade.sh
	./infra/milestone-2/tests/test-workspace-secret-upgrade.sh
	./infra/milestone-2/tests/test-poc-identity.sh
	./infra/milestone-2/tests/test-release-promotion.sh
	./infra/milestone-2/tests/test-control-plane-env.sh
	./infra/milestone-2/tests/test-api-build.sh
	./infra/milestone-2/tests/test-rollback-metadata-policy.sh
	shellcheck infra/milestone-2/scripts/*.sh infra/milestone-2/tests/*.sh
	./infra/node/tests/test-contract.sh
	./infra/node/tests/test-secret-create-resume.sh
	./infra/node/tests/test-plan-materials.sh
	./infra/node/tests/test-upgrade-resume.sh
	./infra/node/tests/test-backup-metadata.sh
	./infra/node/tests/test-worker-issuer-infra.sh
	./infra/node/tests/test-postgres-privileges.sh
	shellcheck infra/node/scripts/*.sh infra/node/tests/*.sh
	./infra/agent-sandbox/test-adapter-static.sh
	shellcheck infra/agent-sandbox/*.sh

test-sandbox-contract:
	./scripts/test-sandbox-contract.sh

test-project-contract:
	./scripts/test-project-contract.sh

test-project-postgres:
	./scripts/test-project-postgres.sh

test-sandbox-postgres:
	./scripts/test-sandbox-postgres.sh

release:
	./scripts/release.sh

test-release:
	./scripts/test-release.sh

test-install:
	./scripts/test-install.sh

ci: fmt-check check-generated check-workspace-generated check-project-generated check-proxy-generated check-node-generated check-sandbox-generated test test-sandbox-contract test-project-contract test-release test-install
