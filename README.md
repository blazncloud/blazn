<p align="center">
  <img src="docs/assets/blazn-icon.svg" width="160" height="160" alt="Blazn logo">
</p>

<h1 align="center">Blazn</h1>

<p align="center">
  The operating workspace for people and AI agent teams.
</p>

<p align="center">
  <a href="https://github.com/blazncloud/blazn/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/blazncloud/blazn/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/blazncloud/blazn/releases/tag/v0.1.0-poc.3"><img alt="Release v0.1.0-poc.3" src="https://img.shields.io/badge/release-v0.1.0--poc.3-f97316.svg"></a>
  <img alt="Go 1.26.2 or newer" src="https://img.shields.io/badge/go-%3E%3D1.26.2-101010.svg">
</p>

Blazn is a cross-platform workspace for people and teams to run, coordinate, and improve AI agents as a shared workforce. It brings agents, conversations, execution environments, models, knowledge, tools, schedules, projects, artifacts, and operational insight into one product—a shared company brain that can work across local machines and the Blazn cloud.

The visual identity carries forward the flame, Blaze orange, and dark ground established by Blaze Proxy. Blazn expands that foundation into a broader workspace, control plane, node fabric, and governed AI-routing system.

## What is in this repository

| Area | What it provides |
|---|---|
| CLI | Human and deterministic JSON output for auth, workspaces, nodes, diagnostics, and lifecycle operations |
| Control API | TypeScript services for authentication, workspace membership, node enrollment, broker operations, and persistence |
| Contracts | Versioned OpenAPI and JSON Schema contracts for clients, nodes, proxy events, policies, and receipts |
| Node runtime | Signed enrollment plans, transactional installation, recovery, identity, daemon, and heartbeat support |
| AI proxy foundation | Normalized OpenAI and Anthropic request, response, error, stream, policy, and activation contracts |
| Infrastructure | Reviewed deployment, database, backup, MicroK8s issuer, and Agent Sandbox qualification assets |

## Quick start

### Install the current signed release

The installer requires an immutable version and verifies the signed checksum manifest before installing:

```bash
curl -fL --progress-bar --show-error \
  https://github.com/blazncloud/blazn/releases/download/v0.1.0-poc.3/install.sh |
  BLAZN_VERSION=v0.1.0-poc.3 sh
```

The default destination is `~/.local/bin`. When needed, the installer adds that
directory to the detected zsh, bash, or POSIX shell profile, so new terminals
can invoke `blazn` directly. Set `BLAZN_NO_PATH_UPDATE=1` when a managed
environment owns shell configuration. During installation, named stages are
printed and the release archive displays a terminal progress bar. Set
`BLAZN_NO_PROGRESS=1` to hide only the bar or `BLAZN_QUIET=1` to suppress all
non-error installer progress.

Verify the installation and run offline diagnostics:

```bash
blazn version
blazn doctor
blazn help
```

### Build from source

```bash
git clone https://github.com/blazncloud/blazn.git
cd blazn
go build -o ./bin/blazn ./cmd/blazn
./bin/blazn version
```

## Examples

### Machine-readable diagnostics

Non-streaming CLI commands support deterministic output for scripts and CI:

```bash
blazn doctor --output json
blazn version --output json
```

### Connect to a deployed control plane

Authenticated commands require an HTTPS Blazn API origin. Loopback HTTP is available only for explicit local development.

```bash
export BLAZN_API_URL="https://blazn.example.com"
blazn auth login
blazn auth status
blazn auth devices --output json
```

### Create and select a workspace

Mutating workspace commands require a caller-provided request ID so retries are safe and idempotent:

```bash
blazn workspace create "Acme AI" \
  --slug acme-ai \
  --request-id onboarding-20260822

blazn workspace list
blazn workspace use acme-ai
blazn workspace members
```

### Invite a teammate safely

Invitation tokens are accepted from standard input, never as command-line arguments where shell history or process listings could expose them:

```bash
blazn workspace invite \
  --role member \
  --expires-in 24h \
  --request-id invite-alex-20260822

printf '%s\n' "$BLAZN_INVITE_TOKEN" |
  blazn workspace join \
    --invite-stdin \
    --request-id join-alex-20260822
```

## CLI overview

```text
blazn auth       Authenticate this device and manage sessions
blazn workspace  Create, select, and manage workspaces
blazn node       Enroll, install, recover, and heartbeat a node
blazn doctor     Run offline readiness checks
blazn version    Show build and contract versions
blazn uninstall  Remove a receipt-owned direct installation
```

Run `blazn help <command>` for the exact options supported by your installed release.

## Development

The primary validation command checks formatting, generated contracts and clients, Go tests, control API tests, release packaging, and installer behavior:

```bash
make ci
```

Useful focused commands:

```bash
make test
make test-control-api
make test-release
make test-install
```

## Documentation

- [Product overview](docs/product-overview.md) — vision, surfaces, system model, and product principles
- [POC execution plan](docs/poc-execution-plan.md) — phased implementation and qualification plan
- [Milestone 1](docs/milestone-1.md) — CLI distribution and release trust
- [Milestone 2](docs/milestone-2.md) — control plane and authentication
- [Milestone 2A contract](docs/milestone-2a-contract.md) — workspace and membership semantics
- [Authentication](docs/authentication.md) — self-hosted ZITADEL, branded sign-in, MFA, and device approval
- [Node contract](docs/node-contract.md) — enrollment, installation, receipts, and lifecycle
- [Proxy contract](docs/proxy-contract.md) — compatible request routing and activation policy
- [Brand assets](docs/assets/README.md) — flame mark and palette

## Product surfaces

- Desktop application for macOS, Windows, and Linux
- CLI for people, agents, automation, and CI
- Blazn cloud for managed models, environments, collaboration, and orchestration
- Blazn Button for embedding real-time agent work into products

## Project status

Blazn is in proof-of-concept development. The repository currently includes the signed standalone CLI release path, device-bound authentication, workspace contracts and services, node enrollment and broker infrastructure, proxy contracts, and deployment qualification assets. The complete desktop workspace, managed cloud experience, and all product-vision surfaces are not yet generally available.
