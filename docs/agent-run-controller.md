# Agent Run controller

The Agent Run controller executes queued Agent Runs. It is a separate process
(`services/control-api/src/agent-run-controller-main.ts`) that connects to
PostgreSQL as `blazn_agent_run_controller`, a role that can only call the
lease-fenced `agent_run_controller_*` functions.

## How a Run executes

1. **Create.** `blazn run create --agent-version ID --harness-profile ID`
   sends `POST /v1/runs` with an `agent` object. In one transaction the API
   checks the Agent Version and Harness Profile, enqueues the Run, creates a
   Sandbox from the Agent's template as the requesting user, and records the
   pair in `agent_run_executions`. The Run is refused if the Sandbox would be.
2. **Claim.** The controller claims the Run under a lease and renews it while
   it works. Every later database call carries the lease and is rejected once
   the lease is lost, so two controllers never drive one Run.
3. **Bind.** When the Sandbox is ready and the Sandbox controller has recorded
   which Node it runs on, the controller binds the Run to that Sandbox. The
   Run becomes `running`.
4. **Start the harness.** The controller starts `blazn-agent` inside the
   Sandbox. It uses `/opt/blazn/agent` when the template image carries it, and
   otherwise uploads the binary for the Sandbox's architecture.
5. **Relay.** User messages (`blazn run send`) are claimed in order
   (prompt, then steer, then follow-up) and written to the harness inbox. The
   harness's replies are stored as Run messages of kind `reply`, linked to the
   message they answer, and its tool activity becomes Run events.
6. **Model calls.** When the harness needs the model it writes a
   `model.request` event. The controller sends that request to the Run's model
   route and writes the response back. The model credential stays in the
   controller; the Sandbox has no network access and never sees it.
7. **Finish.** After the Run has been idle for `BLAZN_AGENT_RUN_IDLE_SECONDS`,
   or shortly before the Sandbox expires, the controller asks the harness to
   finalize, stores `patch` (a diff against the pinned commit) and `summary`
   as Artifacts, and finalizes the Run as `succeeded`.
8. **Release.** A sweeper stops the Sandbox of every Run that has ended,
   including cancelled and failed ones.

`blazn run cancel` ends the Run; the controller loses its lease on the next
call, stops, and the sweeper releases the Sandbox.

## Sandbox access

The controller reaches the Sandbox only through the Sandbox controller's access
endpoint, with single-use grants issued by
`agent_run_controller_issue_grant`. A Run-bound grant is honoured only while
the Run is running, bound to that Sandbox, and leased. Access therefore ends
with the Run.

Each Sandbox exec is limited to about 60 seconds, so the harness runs detached
and the controller long-polls it with `blazn-agent wait`.

## The harness in the Sandbox

`blazn-agent` (`cmd/blazn-agent`, `internal/sandboxagent`) keeps its state
under `/workspace/artifacts/blazn-agent`:

| Path | Purpose |
| --- | --- |
| `config.json` | Instructions, repository directory, limits |
| `inbox/` | Messages and model responses from the controller, one file each |
| `outbox.jsonl` | Numbered events for the controller |
| `out/patch.diff`, `out/summary.md` | The Run's outputs |

The harness offers the model three tools: `run_command`, `read_file` and
`write_file`. All of them act inside the Sandbox.

## Configuration

| Variable | Meaning |
| --- | --- |
| `BLAZN_AGENT_RUN_DATABASE_URL_FILE` | Connection string for `blazn_agent_run_controller` |
| `BLAZN_SANDBOX_ACCESS_URL` | The Sandbox controller's access endpoint (plain `http` origin) |
| `BLAZN_MODEL_ROUTES_FILE` | Model routes, see below |
| `BLAZN_AGENT_HARNESS_DIR` | Directory with `blazn-agent-linux-amd64` and `-arm64` (default `/app/agent`) |
| `BLAZN_AGENT_RUN_WORKER_ID` | Lease owner name |
| `BLAZN_AGENT_RUN_IDLE_SECONDS` | Idle time before a Run finishes (default 300) |
| `BLAZN_AGENT_RUN_SANDBOX_READY_SECONDS` | How long to wait for the Sandbox (default 1000) |
| `BLAZN_AGENT_RUN_NODE_OBSERVATION_SECONDS` | How long to wait for the Node observation before retrying (default 60) |
| `BLAZN_AGENT_RUN_MAX_CONCURRENT` | Runs driven at once (default 4) |

### Model routes

```json
{ "routes": [
  { "id": "ROUTE-UUID", "version": 1, "kind": "openai-chat", "model": "glm-5.3-flash",
    "baseUrl": "http://MODEL-HOST:8000/v1", "credentialFile": "/var/run/blazn/model/key" },
  { "id": "ROUTE-UUID", "version": 1, "kind": "stub", "model": "stub" }
] }
```

An Agent Version names its route by ID and version. `openai-chat` speaks the
OpenAI chat-completions API. `stub` is a deterministic stand-in for
qualification: it runs one command in the Sandbox and reports its output, so a
Run can be proven end to end without a model server.

## Database role

Migration `040_agent_run_execution.sql` grants the controller its functions.
`infra/node/postgres/ensure-controller-roles.sh` holds the exact allowlist and
fails if the role has any other privilege. On the hosted database the role
needs `LOGIN`, a password and a `pg_hba.conf` entry; the runbook lists them.

## Qualification

`infra/qualification/agent-run.py` drives one conversation through the CLI and
checks the replies, the Artifacts and that the Sandbox was released.
