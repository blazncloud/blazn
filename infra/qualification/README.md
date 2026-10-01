# Self-serve user-flow qualification

`qualify-flows.py` runs the real sign-up, sign-in, workspace, project,
invitation, membership, role change, leave, member removal, last-owner
protection, invitation-revocation, node-list and logout flows
against `https://api.blazn.frontro.com`. It needs no human inbox and no shared
credentials.

It runs from an operator machine with SSH to `ben1` (cluster control plane) and
`ben5` (Docker build host):

```sh
python3 infra/qualification/qualify-flows.py > run.json
```

How it works:

- It installs the published CLI release on ben5 through the signed installer.
  Each test identity then runs in its own throwaway container, because the Linux
  CLI stores credentials under the passwd home directory and ignores `$HOME`.
- It completes real `blazn auth login` device requests through the public
  activation page using `qualification-<role>-<run>@blazn.invalid` addresses.
  The API routes only that reserved domain to the private Mailpit capture inbox
  (`EMAIL_CAPTURE_SMTP` / `EMAIL_CAPTURE_DOMAIN`); every other address uses
  Resend. The script reads the code through a bounded `kubectl port-forward` on
  ben1, and never prints codes or tokens.
- Every run creates new accounts and a new workspace, so runs are independent.
  Test users and workspaces remain in the development database.
- It emits JSON with one entry per step and exits after the first failure.

## Sandbox lifecycle

`sandbox-lifecycle.py` qualifies sandboxes on a registered node (plan item
M4.2). With a CLI that is already signed in, it runs

    create -> ready -> exec -> upload -> download -> stop -> delete

and compares the SHA-256 of the downloaded file with the uploaded one. It then
proves nothing is left behind: no Pod or Sandbox object labeled with the
sandbox ID (read-only `kubectl`), and no access grant still `active`
(read-only `psql` on the hosted database).

```sh
python3 infra/qualification/sandbox-lifecycle.py \
  --workspace WORKSPACE_ID --template NAME@VERSION --source REPOSITORY=COMMIT \
  --ssh "-J ben1 blazn@NODE_ADDRESS" --blazn .local/bin/blazn \
  --psql "ssh DATABASE_HOST sudo -n -u postgres psql -d blazn_test -At" \
  --repeat 2 > report.json
```

The test file is written into the sandbox's first source checkout, because
transfers need a mounted workspace volume. The checkout's destination is read
from the sandbox (the template sets it, not the `--source` name); a sandbox
without sources uses `/workspace/artifacts`. Pass `--remote-path` to choose
another mounted path. The grant query is sent to the
`--psql` command on standard input, so an `ssh` prefix works as written.

Omit `--ssh` to use a CLI on the local machine. The report lists every step
per iteration and the node each sandbox ran on. A failed run stops its sandbox, waits
for it to stop, deletes it, and reports `cleanedUp`, unless `--keep-failed` is
given.

`test-sandbox-lifecycle.sh` checks the script itself against a fake CLI,
cluster and database: a healthy run passes, and a leftover Pod, an active
grant, or a missing grant check each fail.

## Agent Run

`agent-run.py` qualifies a conversation with an Agent (plan items M6.1, M6.2,
M6.5 and M6.7). With a CLI that is signed in and an Agent created by
`blazn agent quickstart`, it runs

    run create -> prompt -> reply -> follow-up -> reply -> finished
        -> patch and summary Artifacts -> Sandbox released

```sh
python3 infra/qualification/agent-run.py \
  --agent-version AGENT_VERSION_ID --harness-profile HARNESS_PROFILE_ID \
  --ssh CLI_HOST --blazn .local/bin/blazn --expect-in-patch BLAZN_AGENT_PROOF.md \
  --psql "ssh DATABASE_HOST sudo -n -u postgres psql -d blazn_test -At" > report.json
```

`--expect-in-patch` checks that the downloaded patch holds the Agent's change;
the value above is the file the stand-in model route writes. The database is
read only to find the Run's Sandbox and to count active access grants. A failed
qualification cancels its Run unless `--keep-failed` is given.

The Run finishes after the controller's idle time (five minutes on the hosted
environment), so a passing qualification takes about ten minutes.
