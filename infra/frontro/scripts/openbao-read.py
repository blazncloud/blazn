#!/usr/bin/env python3
"""Read one OpenBao KV secret with a Kubernetes service-account token.

Runs on a cluster host. The token is read from stdin and exchanged for a
short-lived OpenBao token that is revoked before exit. The secret is printed
as JSON for the next process in the pipe; it is never written to disk.

    kubectl create token SA -n NS --audience=openbao --duration=10m |
      openbao-read.py CLUSTER_IP CA_FILE ROLE SECRET_PATH
"""
import http.client
import json
import socket
import ssl
import sys

SERVER_NAME = "openbao.frontro-secrets.svc.cluster.local"


def main():
    if len(sys.argv) != 5:
        print(__doc__, file=sys.stderr)
        return 64
    address, ca_file, role, path = sys.argv[1:]
    context = ssl.create_default_context(cafile=ca_file)

    def call(method, route, token=None, body=None):
        raw = socket.create_connection((address, 8200), 20)
        connection = http.client.HTTPSConnection(SERVER_NAME, 8200, timeout=20)
        connection.sock = context.wrap_socket(raw, server_hostname=SERVER_NAME)
        headers = {"content-type": "application/json"}
        if token:
            headers["X-Vault-Token"] = token
        connection.request(method, "/v1/" + route, body=json.dumps(body) if body is not None else None, headers=headers)
        response = connection.getresponse()
        data = response.read()
        connection.close()
        if response.status >= 300:
            raise RuntimeError(f"OpenBao {route} returned HTTP {response.status}")
        return json.loads(data) if data else {}

    token = call("POST", "auth/kubernetes/login", body={"role": role, "jwt": sys.stdin.read().strip()})["auth"]["client_token"]
    try:
        secret = call("GET", path, token)["data"]["data"]
    finally:
        try:
            call("POST", "auth/token/revoke-self", token, {})
        except Exception:
            pass
    print(json.dumps(secret))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
