# Blazn

Blazn is a cross-platform workspace for people and teams to run, coordinate, and improve AI agents as a shared workforce.

It brings agent conversations, execution environments, models, knowledge, tools, schedules, project work, artifacts, and operational insight into one product—creating a unified company brain that can work across local machines and the Blazn cloud.

Start with the [product overview](docs/product-overview.md), then use the [POC execution plan](docs/poc-execution-plan.md) and [Milestone 1 guide](docs/milestone-1.md).

## Product surfaces

- Desktop application for macOS, Windows, and Linux
- CLI for people, agents, automation, and CI
- Blazn cloud for managed models, environments, collaboration, and orchestration
- Blazn Button for embedding real-time agent work into products

## Status

The product vision and initial system designs are documented. Milestone 1 implements the standalone CLI, signed cross-platform release artifacts, and secure curl installation lifecycle. Authentication and workspace services are the next milestone.

## Install

```bash
curl -fsSL https://github.com/KingJammin/blazn/releases/download/v0.1.0-poc.3/install.sh |
  BLAZN_VERSION=v0.1.0-poc.3 sh
```

The installer puts `blazn` in `$HOME/.local/bin` and, when necessary, adds that
directory to the detected zsh, bash, or POSIX shell profile. New terminals can
then run commands such as `blazn auth login` without an absolute path. Set
`BLAZN_NO_PATH_UPDATE=1` to opt out of shell-profile changes.
