# Contributing to Blazn

Thanks for helping. Blazn is in proof-of-concept development, so things move
quickly; an issue or a short discussion before a large change saves rework.

## License of contributions

Blazn is licensed under the [Apache License 2.0](LICENSE). By submitting a
contribution you agree that it is licensed under the same terms (section 5 of
the license). Only submit work you have the right to contribute.

## Before you open a pull request

```bash
make ci
```

runs formatting, the generated-code checks, the Go and control API tests,
release packaging and the installer tests. Focused targets while you work:

```bash
make test               # Go unit tests
make test-control-api   # control API tests
make test-infra         # infrastructure and qualification script tests
```

Contracts in `packages/contracts` are the source of truth for the generated
clients in `internal/client`. If you change a contract, regenerate the matching
client (for example `go run ./cmd/generate-node-client`) and commit both; the
generators pin each contract's SHA-256 and CI fails when they disagree.

## Pull requests

- Keep a pull request to one change, and say what it does and how you verified
  it. State plainly what you did not test.
- Add or update tests with behavior changes.
- Never commit secrets, tokens, private keys or customer data. Manifests under
  `infra/frontro` are exported without secret values; keep it that way.
- Changes to what runs on a node, to signing, or to the sandbox admission
  boundary need a reviewer who knows that area.

## Reporting bugs and security issues

Use GitHub issues for bugs. For anything that might be a vulnerability, follow
[SECURITY.md](SECURITY.md) instead of opening a public issue.

## Where things are

- [README](README.md): what Blazn is and how to install the CLI.
- [Try the hosted environment](docs/delivery/try-the-hosted-environment.md).
- [Delivery plan](docs/delivery/end-to-end-plan.md): what is done and what is next.
- [Hosting runbook](docs/delivery/hosting-runbook.md): how the hosted deployment is operated.
