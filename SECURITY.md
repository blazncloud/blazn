# Security policy

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability.

Report it privately through GitHub: on the repository's **Security** tab choose
**Report a vulnerability**. If that option is not available to you, open an
issue that says only that you have a security report and asks for a private
contact; do not include details.

Include what you found, how to reproduce it, the version (`blazn version`) or
commit, and the impact you expect. We aim to acknowledge a report within three
working days and will keep you informed while we fix it. Please give us a
reasonable time to release a fix before disclosing publicly.

## Supported versions

Blazn is in proof-of-concept development. Only the latest published release
and the `main` branch receive security fixes.

## Scope notes

- Sandboxes are isolated by orchestration only today. They are for approved,
  non-sensitive workloads and are not a boundary for untrusted code.
- The hosted deployment at `api.blazn.frontro.com` is a development
  environment. Do not test against other users' data, and do not run
  denial-of-service or volume-based tests against it.
