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
  --workspace WORKSPACE_ID --template NAME@VERSION \
  --ssh "-J ben1 blazn@NODE_ADDRESS" --blazn .local/bin/blazn \
  --psql "ssh DATABASE_HOST sudo -n -u postgres psql -d blazn_test -At" \
  --repeat 2 > report.json
```

Omit `--ssh` to use a CLI on the local machine. The report lists every step
per iteration and the node each sandbox ran on. A failed run stops and deletes
its sandbox unless `--keep-failed` is given.

`test-sandbox-lifecycle.sh` checks the script itself against a fake CLI,
cluster and database: a healthy run passes, and a leftover Pod, an active
grant, or a missing grant check each fail.
