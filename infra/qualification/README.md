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
