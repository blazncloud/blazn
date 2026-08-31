SHELL := /bin/sh

.PHONY: fmt fmt-check generate-client check-generated generate-workspace-client check-workspace-generated generate-project-client check-project-generated generate-run-client check-run-generated generate-agent-harness-client check-agent-harness-generated generate-proxy-contract check-proxy-generated generate-node-client check-node-generated generate-sandbox-client check-sandbox-generated test test-control-api test-identity test-identity-root test-infra test-runtime-persistence test-sandbox-contract test-project-contract test-run-contract test-development-contract test-development-sandbox-static test-harness-contract test-project-postgres test-run-postgres test-agent-harness-postgres test-development-postgres test-sandbox-postgres test-sandbox-controller-postgres test-phase5-controller-deployment-static test-phase5-controller-secret-init test-sandbox-io-static release test-release test-install ci

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

generate-run-client:
	go run ./cmd/generate-run-client

check-run-generated:
	go run ./cmd/generate-run-client --check

generate-agent-harness-client:
	go run ./cmd/generate-agent-harness-client

check-agent-harness-generated:
	go run ./cmd/generate-agent-harness-client --check

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

test-identity:
	./infra/identity/test-static.sh
	shellcheck -x -e SC2016 infra/identity/*.sh

test-identity-root:
	./infra/identity/test-secret-generation.sh
	./infra/identity/test-path-and-repair.sh

test-runtime-persistence:
	./infra/runtime-persistence/test-static.sh
	shellcheck infra/runtime-persistence/*.sh

test-infra:
	./infra/milestone-2/tests/test-preflight.sh
	./infra/milestone-2/tests/test-contract.sh
	./infra/milestone-2/tests/test-config-digest.sh
	./infra/milestone-2/tests/test-live-upgrade.sh
	./infra/milestone-2/tests/test-workspace-secret-upgrade.sh
	./infra/milestone-2/tests/test-poc-identity.sh
	./infra/milestone-2/tests/test-release-promotion.sh
	./infra/milestone-2/tests/test-control-plane-env.sh
	./infra/milestone-2/tests/test-control-plane-compose.sh
	./infra/milestone-2/tests/test-identity-overlay.sh
	./infra/milestone-2/tests/test-api-build.sh
	./infra/milestone-2/tests/test-rollback-metadata-policy.sh
	shellcheck infra/milestone-2/scripts/*.sh infra/milestone-2/tests/*.sh infra/milestone-2/postgres-compat/*.sh
	./infra/node/tests/test-contract.sh
	./infra/node/tests/test-secret-create-resume.sh
	./infra/node/tests/test-plan-materials.sh
	./infra/node/tests/test-plan-material-rotation.sh
	./infra/node/tests/test-upgrade-resume.sh
	./infra/node/tests/test-backup-metadata.sh
	./infra/node/tests/test-worker-issuer-infra.sh
	./infra/node/tests/test-postgres-privileges.sh
	shellcheck infra/node/scripts/*.sh infra/node/tests/*.sh
	./infra/agent-sandbox/test-adapter-static.sh
	./infra/agent-sandbox/phase5-controller/test-static.sh
	shellcheck infra/agent-sandbox/*.sh infra/agent-sandbox/phase4c/*.sh infra/agent-sandbox/phase5-controller/*.sh infra/agent-sandbox/phase5-boundary/*.sh

test-sandbox-contract:
	./scripts/test-sandbox-contract.sh

test-project-contract:
	./scripts/test-project-contract.sh

test-run-contract:
	./scripts/test-run-contract.sh

test-development-contract:
	./scripts/test-development-contract.sh

test-development-sandbox-static:
	shellcheck examples/blazn-development-sandbox/*.sh
	./examples/blazn-development-sandbox/test-entrypoint.sh
	./examples/blazn-development-sandbox/test-read-retry.sh
	./examples/blazn-development-sandbox/test-dev-session.sh
	sh examples/blazn-development-sandbox/test-delete-and-prove.sh
	sh examples/blazn-development-sandbox/test-download-patch.sh
	shellcheck examples/coding-agent/scripts/*.sh
	grep -F 'ln -s /usr/local/go/bin/go /usr/local/bin/go' examples/blazn-development-sandbox/Dockerfile >/dev/null
	grep -F 'ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt' examples/blazn-development-sandbox/Dockerfile >/dev/null
	go run ./cmd/blazn --output json template validate -f examples/coding-agent/sandbox-template-dev.yaml | jq -e '.valid == true and (.errors | length == 0) and (.warnings | length == 0)' >/dev/null

test-harness-contract:
	./scripts/test-harness-contract.sh

test-project-postgres:
	./scripts/test-project-postgres.sh

test-run-postgres:
	./scripts/test-run-postgres.sh

test-agent-harness-postgres:
	./scripts/test-agent-harness-postgres.sh

test-development-postgres:
	./scripts/test-development-postgres.sh

test-sandbox-postgres:
	./scripts/test-sandbox-postgres.sh

test-sandbox-controller-postgres:
	./scripts/test-sandbox-controller-postgres.sh

test-phase5-controller-deployment-static:
	./infra/agent-sandbox/phase5-controller/test-static.sh
	shellcheck infra/agent-sandbox/phase5-controller/*.sh

test-phase5-controller-secret-init:
	go test ./cmd/blazn-sandbox-controller-secret-init

test-sandbox-io-static:
	./infra/agent-sandbox/test-sandbox-io-static.sh

release:
	./scripts/release.sh

test-release:
	./scripts/test-release.sh

test-install:
	./scripts/test-install.sh

ci: fmt-check check-generated check-workspace-generated check-project-generated check-run-generated check-proxy-generated check-node-generated check-sandbox-generated test test-control-api test-identity test-runtime-persistence test-sandbox-contract test-project-contract test-run-contract test-development-contract test-development-sandbox-static test-harness-contract test-release test-install
