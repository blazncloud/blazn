# Runtime persistence

This bundle makes the live POC dependencies that sit outside Kubernetes
reboot-persistent without embedding credentials. The controller relay exposes
only the exact ben1 LAN address and forwards to the loopback-only PostgreSQL
and object-store ports; object-store traffic terminates TLS with the existing
root-only certificate and key. The identity tunnel runs as the existing
non-login `blazn-ngrok` user and reads the existing protected ngrok config.

`blazn-runtime.tmpfiles` recreates the authoritative live-mutation lock
directory before a post-reboot deployment operation needs it. Run the static
test before installing, then install on ben1 as root:

```text
./infra/runtime-persistence/test-static.sh
sudo ./infra/runtime-persistence/install.sh
```

The installer replaces the two transient units with enabled persistent units;
it does not read, copy, print, or modify credentials.

The helpers start before the control plane and are enabled in its wants directory.
They must not require or start after the control plane: API startup checks the
identity provider through the tunnel, and that reverse dependency deadlocks
startup. The helpers stay available while the control plane restarts. The static
unit transaction includes this health dependency so a reversed ordering fails.
