# Blazn development delivery ledger

Updated 2026-09-28. Acceptance means direct evidence for the named scenario; source, passing unit tests, and synthetic sessions are separate evidence. Development targets: https://blazn.frontro.com and https://api.blazn.frontro.com. Preserve existing test API, production identity issuer, Moments and unrelated checkouts/data.

## Baseline

- Core repository: https://github.com/blazncloud/blazn at `7b24bb675a26cf78efbfea91b0a721f82444bd65`, branch `codex/blazn-test-deployment`. Existing uncommitted CORS/deployment changes preserved.
- Candidate API image: `registry.blazn-test.internal/blazn/control-api@sha256:6610a46f1e602fa85e564df9ac01b4e31aba262c0ba9fbe3abba400346a5b047`. Isolated namespace `blazn-test`, Deployment `api-dev`; previous `api` preserved.
- API health200, DB/objectstore healthy, identityProvider disabled. Website HTTPS404. Existing issuer discovery returns400 ERR_NGROK_6008; separate local identity containers are healthy.
- Installed CLI `v0.1.0-poc.118`, commit `04c2ec385f0f31ed7f171a6ddccaf6a068534755`. Latest published release metadata `v0.1.0-poc.121`; not freshly installed/signature-qualified here.
- Authenticated discovery: seven accessible blazncloud repositories plus KingJammin/blaze-proxy. All326 branch histories had no commits associated with kevindalvi. This does not cover unaccepted invitations.
- Frontend source access remains unresolved. The specifically authorized invitation was accepted and disappeared from the pending list, but its repository still returns404. A separately accessible AI Studio uses Supabase auth/data and its own Express API; it is not a drop-in Blazn frontend. Private access details remain in the local handoff, not in this public ledger.
- Dispatcher usage checked; recent cloud authentication failures and existing dispatcher rejection of non-FrontRowXP repository establish scoped direct fallback. Builds/tests stay bounded on ben5; no broad local checks/install.

## Ordered tasks and acceptance gates

| Gate | Status | Work | Required acceptance proof |
|---|---|---|---|
| M0 source/baseline | In progress; frontend access pending | Resolve Kevin frontend invitation/source, record branch+SHA/framework/API contract; preserve existing state | Reproducible source mapping, current deployment revision, runnable intended UI; no activation page substitute |
| M1 identity | In progress; design/qualification | Isolated development identity route/client preserving existing issuer; real browser session distinct from CLI device flow; registration, verification, MFA, recovery, login/logout/revocation | Real user registration→verified identity→MFA→browser session; CLI device login; revoke denies prior session; recovery works; no synthetic session acceptance |
| M2 workspaces/projects | Source implemented; qualification pending | Preserve roles owner/administrator/operator/member/viewer; qualify existing invitation/role/last-owner transactions and project management; repair concrete failures | Signed-in user creates workspace/project; second user joins invitation; roles enforce access; revoked/expired invite rejected; concurrent last-owner operations preserve owner; browser+CLI agree |
| M3 nodes | Deferred until M1/M2 pass | Qualified enrollment/install/heartbeat/revoke using exact CLI release | Real node enrollment and restart/revoke proof without affecting unrelated machines |
| M4 isolated environments | Deferred | Approved template, bounded execution/storage/network, lifecycle and cleanup | Real isolated workload, policy denials, stop/delete and retained artifacts |
| M5 one provider | Deferred | One scoped provider credential with explicit workload authority | Direct provider request plus denial/budget controls; no broad credentials |
| M6 execution | Deferred | Agent/harness/run execution bound to qualified M3–M5 resources | Real run progress/output/cancel/recovery, not synthetic executor proof |
| M7 release/recovery | Deferred | Review, signed CLI release/install, deployment automation, backups and rollback | Exact revision validation, fresh install/update, DB/object restore and tested rollback |

## Current evidence

Local evidence directory: `outputs/blazn-test-deployment-20260928` in the surrounding workspace (not published product content). `STATE.md` records source/resources/rollback. `dev-hosts-build.log`: build+10 focused tests. `dev-hosts-public-proof.json`: HTTPS/CORS/protected denial and preserved existing routes. `api-dev-persistence-proof.json`: fixture-session workspace/project/idempotency/isolation/restart proof; explicitly not real authentication. `storage-proof.json`: persistence across actual Pod replacement and scoped storage denial.

## Execution rules

1. Read current source/state; check dispatcher usage before new submissions. Use cloud Codex only when available; never cycle identities/models after auth/usage failure.
2. Test narrowly on bounded remote resources. Record exact command, revision, result and limits below.
3. Commit/push/PR and isolated development deployment are authorized; no production issuer/domain/data cutover. Review returned patches and preserve unrelated changes.
4. Do not mark a milestone complete because a predecessor is blocked, a UI exists, or tests use synthetic identity. Recheck external gates and continue independent safe qualification.
5. API `/activate` is device approval; it is not a browser control-panel session. Preserve sealed OIDC capability and MFA assurance requirements.
6. Before later provider/environment work, complete real signup→workspace/project acceptance.

## Execution log

- 2026-09-28: Read-only baseline refreshed; pending Kevin invitation found; repository source inaccessible404; existing five-role workspace contract retained. Only the specifically authorized frontend invitation was accepted; no production identity changes.

- 2026-09-28: Added revoked/expired invitation and concurrent immutable-owner leave/remove/demotion acceptance coverage. Bounded disposable PostgreSQL qualification:38/38 tests passed,0skips, all38migrations; existing DBs untouched; tmpfs database and containers removed. Initial contract-fixture mount omission corrected before the authoritative passing run.
