# Disposable auth/workspace/project qualification

Run only on an approved bounded build host with Docker and Python3. Never provide a real database URL: the runner creates a network-isolated PostgreSQL container with memory-backed data, applies migrations, then executes tests through a second container sharing only its loopback network. Some upstream tests truncate tables; the disposable database is mandatory.

From repository root, build the test image using the API directory as context:

```sh
DOCKER_BUILDKIT=0 docker build --cpu-period=100000 --cpu-quota=100000 --memory=1536m -f infra/local-test/qualification/Dockerfile -t blazn-m2-qualification:20260928 services/control-api
BLAZN_QUALIFICATION_CONTRACTS="$PWD/packages" python3 infra/local-test/qualification/run.py
```

The runner uses `sudo -n docker`, refuses existing task containers, caps PostgreSQL at0.5CPU/512Mi and Node at1CPU/1536Mi, serializes tests, and removes both containers and the tmpfs database in a finally block. It tests real PostgreSQL role permissions, tenancy, invitation races/revocation/expiry, immutable-owner safeguards and project conflicts; unit tests cover OIDC assurance and browser CORS. It does not prove real registration, email delivery, MFA, or browser/CLI login.
