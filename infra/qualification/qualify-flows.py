#!/usr/bin/env python3
"""End-to-end qualification of Blazn user flows against api.blazn.frontro.com.

Runs from the coordinator Mac. The CLI runs on ben5 with an isolated HOME, sign-in
codes for qualification-*@blazn.invalid are read from the private Mailpit capture
inbox in blazn-identity-dev through a kubectl port-forward on ben1. No secret or
code is printed; results are emitted as JSON.
"""
import json, re, shlex, subprocess, sys, time, uuid

API = "https://api.blazn.frontro.com"
VERSION = "v0.1.0-poc.132"
RUN = time.strftime("%Y%m%d%H%M%S")
ROOT = f"/tmp/blazn-qual-{RUN}"
results = []


def sh(host, command, stdin=b"", timeout=180, check=True):
    r = subprocess.run(["ssh", "-o", "BatchMode=yes", host, command], input=stdin, capture_output=True, timeout=timeout)
    if check and r.returncode:
        raise RuntimeError(f"{host}: rc={r.returncode} {r.stderr.decode()[-400:]}")
    return r.stdout.decode()


def record(step, ok, detail=""):
    results.append({"step": step, "ok": bool(ok), "detail": detail})
    print(f"{'PASS' if ok else 'FAIL'} {step} {detail}", file=sys.stderr, flush=True)
    if not ok:
        raise SystemExit(1)


# Any image with uid 1000 owning /home/node works: the CLI is a static binary
# mounted into the container. This is the control API's pinned base image.
IMAGE = "node:22.19.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90"


def docker_cli(user, args, detach_name=None):
    home = f"{ROOT}/home-{user}"
    base = (f"mkdir -p {home} && chmod 0777 {home} && sudo -n docker run --rm -i --network host --user 1000:1000 "
            f"-v {home}:/home/node -v {ROOT}/bin:/opt/blazn:ro -v /etc/ssl/certs:/etc/ssl/certs:ro "
            f"-e HOME=/home/node --entrypoint /opt/blazn/blazn")
    if detach_name:
        base = base.replace("docker run --rm -i", f"docker run -d --name {detach_name}")
    return f"{base} {IMAGE} {args}"


def cli(user, args, stdin=b"", check=True):
    out = sh("ben5", docker_cli(user, f"--output json {args}"), stdin, check=False)
    if check and out.strip() == "":
        err = sh("ben5", docker_cli(user, args) + " 2>&1 | tail -3", stdin, check=False)
        raise RuntimeError(f"blazn {args}: {err.strip()}")
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return out.strip()


def read_code(address, since):
    script = r"""
import json, re, socket, subprocess, sys, time, urllib.request, urllib.parse
address, since = sys.argv[1], sys.argv[2]
with socket.socket() as probe:
    probe.bind(("127.0.0.1", 0)); port = probe.getsockname()[1]
forward = subprocess.Popen(["timeout", "120", "sudo", "-n", "microk8s", "kubectl", "-n", "blazn-identity-dev", "port-forward", "deploy/identity-mail", f"{port}:8025"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    deadline = time.time() + 75
    while time.time() < deadline:
        time.sleep(2)
        try:
            data = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/api/v1/search?query=" + urllib.parse.quote(f"to:{address}"), timeout=5))
        except Exception:
            continue
        fresh = [m for m in data.get("messages") or [] if m.get("Created", "") >= since]
        if fresh:
            text = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/api/v1/message/{fresh[0]['ID']}", timeout=5)).get("Text", "")
            found = re.search(r"sign-in code is (\d{6})", text)
            if found:
                print(found.group(1)); sys.exit(0)
    sys.exit(3)
finally:
    forward.terminate()
"""
    out = sh("ben1", f"python3 - {shlex.quote(address)} {shlex.quote(since)}", script.encode(), timeout=150, check=False).strip()
    if not re.fullmatch(r"\d{6}", out):
        raise RuntimeError("no captured sign-in code arrived")
    return out


def form(path, fields):
    args = " ".join(f"--data-urlencode {shlex.quote(k + '=' + v)}" for k, v in fields.items())
    return sh("ben5", f"curl -s -m 20 -w '\\n%{{http_code}}' -X POST {API}{path} {args}", check=False)


def login(user, email, mode):
    name = f"blazn-qual-{RUN}-{user}"
    sh("ben5", f"sudo -n docker rm -f {name} >/dev/null 2>&1; " + docker_cli(user, "auth login", detach_name=name) + " >/dev/null")
    code = None
    for _ in range(30):
        time.sleep(1)
        out = sh("ben5", f"sudo -n docker logs {name} 2>&1", check=False)
        found = re.search(r"code ([A-Z0-9]{4}-[A-Z0-9]{4})", out)
        if found:
            code = found.group(1)
            break
    record(f"{user}: CLI started device login", code, "")
    since = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(time.time() - 5))
    page = form("/v1/auth/device/email-code", {"user_code": code, "email": email, "mode": mode})
    record(f"{user}: email code requested ({mode})", page.endswith("200") and "Check your email" in page, page[-3:])
    login_code = read_code(email, since)
    record(f"{user}: code captured from inbox", True, "")
    wrong = form("/v1/auth/device/email-verify", {"user_code": code, "email": email, "mode": mode, "code": "000000" if login_code != "000000" else "111111"})
    record(f"{user}: wrong code rejected", wrong.endswith("400") and "incorrect" in wrong, wrong[-3:])
    page = form("/v1/auth/device/email-verify", {"user_code": code, "email": email, "mode": mode, "code": login_code})
    expected = "Account created" if mode == "signup" else "Device authorized"
    record(f"{user}: code verified", page.endswith("200") and expected in page, expected if expected in page else page[-120:])
    for _ in range(20):
        time.sleep(1)
        if sh("ben5", f"sudo -n docker inspect -f '{{{{.State.Running}}}}' {name}", check=False).strip() != "true":
            break
    out = sh("ben5", f"sudo -n docker logs {name} 2>&1; sudo -n docker rm -f {name} >/dev/null 2>&1", check=False)
    record(f"{user}: CLI login completed", "expired" not in out and "error" not in out.lower(), out.strip().splitlines()[-1][:120] if out.strip() else "")


def main():
    sh("ben5", f"mkdir -p {ROOT}/bin && cd {ROOT} && curl -fsSL -o install.sh https://github.com/blazncloud/blazn/releases/download/{VERSION}/install.sh && BLAZN_VERSION={VERSION} BLAZN_INSTALL_DIR={ROOT}/bin BLAZN_NO_PATH_UPDATE=1 BLAZN_QUIET=1 sh install.sh >/dev/null 2>&1 && {ROOT}/bin/blazn version | head -1", timeout=300)
    record("CLI installed with signature verification", True, VERSION)
    owner = f"qualification-owner-{RUN}@blazn.invalid"
    member = f"qualification-member-{RUN}@blazn.invalid"

    login("owner", owner, "signup")
    status = cli("owner", "auth status")
    record("owner: auth status", isinstance(status, dict) and json.dumps(status).find(owner) >= 0, "")

    ws = cli("owner", f"workspace create 'Qualification {RUN}' --request-id {uuid.uuid4()}")
    ws_obj = ws.get("workspace", ws) if isinstance(ws, dict) else {}
    ws_id = ws_obj.get("id")
    record("workspace create", ws_id, ws_id or str(ws)[:200])
    listed = cli("owner", "workspace list")
    record("workspace list", ws_id in json.dumps(listed), "")
    cli("owner", f"workspace use {ws_id}")
    version = ws_obj.get("version", 1)
    edited = cli("owner", f"workspace edit {ws_id} --name 'Qualification {RUN} renamed' --expected-version {version} --request-id {uuid.uuid4()}")
    record("workspace edit (rename)", "renamed" in json.dumps(edited), "")

    project = cli("owner", f"project create 'Qualification project' --request-id {uuid.uuid4()}")
    record("project create", "Qualification project" in json.dumps(project), "")
    record("project list", "Qualification project" in json.dumps(cli("owner", "project list")), "")

    invite = cli("owner", f"workspace invite {ws_id} --role member --request-id {uuid.uuid4()}")
    token = (invite.get("inviteToken") or invite.get("token")) if isinstance(invite, dict) else None
    record("workspace invite (member)", token, "token issued" if token else str(invite)[:200])

    login("member", member, "signup")
    joined = cli("member", f"workspace join --invite-stdin --request-id {uuid.uuid4()}", stdin=token.encode())
    record("second user joins via invite", ws_id in json.dumps(joined), "")
    members = cli("owner", f"workspace members {ws_id}")
    record("members lists both users", owner in json.dumps(members) and member in json.dumps(members), "")

    denied = cli("member", f"project create 'Member project' --request-id {uuid.uuid4()}", check=False)
    record("member role cannot create projects", "Member project" not in json.dumps(denied), "")

    extra = cli("owner", f"workspace invite {ws_id} --role viewer --request-id {uuid.uuid4()}")
    extra_id = (extra.get("invitation") or {}).get("id") if isinstance(extra, dict) else None
    extra_version = (extra.get("invitation") or {}).get("version", 1) if isinstance(extra, dict) else 1
    revoked = cli("owner", f"workspace revoke-invite {extra_id} --expected-version {extra_version} --request-id {uuid.uuid4()}", check=False)
    record("revoke invitation", extra_id and "revoked" in json.dumps(revoked).lower(), "")

    def membership(email):
        listing = cli("owner", f"workspace members {ws_id}")
        for item in listing.get("items", []) if isinstance(listing, dict) else []:
            if email in json.dumps(item) and item.get("status", "active") == "active":
                return item
        return None

    def denied(result):
        return isinstance(result, dict) and "error" in result

    entry = membership(member)
    changed = cli("owner", f"workspace set-role {entry['user']['id']} --role viewer --expected-version {entry['version']} --workspace {ws_id} --request-id {uuid.uuid4()}", check=False)
    entry = membership(member)
    record("owner changes a member's role", not denied(changed) and entry and entry.get("role") == "viewer", "")

    self_entry = membership(owner)
    demoted = cli("owner", f"workspace set-role {self_entry['user']['id']} --role member --expected-version {self_entry['version']} --workspace {ws_id} --request-id {uuid.uuid4()}", check=False)
    record("last owner cannot be demoted", denied(demoted) and membership(owner).get("role") == "owner", "")
    left = cli("owner", f"workspace leave --workspace {ws_id} --request-id {uuid.uuid4()}", check=False)
    record("last owner cannot leave", denied(left) and membership(owner) is not None, "")

    cli("member", f"workspace leave --workspace {ws_id} --request-id {uuid.uuid4()}", check=False)
    record("member leaves the workspace", membership(member) is None, "")
    after_leave = cli("member", f"workspace members {ws_id}", check=False)
    record("a member who left is denied", denied(after_leave), "")

    again = cli("owner", f"workspace invite {ws_id} --role member --request-id {uuid.uuid4()}")
    again_token = (again.get("inviteToken") or again.get("token")) if isinstance(again, dict) else None
    cli("member", f"workspace join --invite-stdin --request-id {uuid.uuid4()}", stdin=(again_token or "").encode(), check=False)
    entry = membership(member)
    record("member rejoins with a new invitation", entry is not None, "")
    removed = cli("owner", f"workspace remove-member {entry['user']['id']} --expected-version {entry['version']} --workspace {ws_id} --request-id {uuid.uuid4()}", check=False)
    record("owner removes a member", not denied(removed) and membership(member) is None, "")
    after_removal = cli("member", f"workspace members {ws_id}", check=False)
    record("a removed member's next call is denied", denied(after_removal), "")

    nodes = cli("owner", f"node list --workspace {ws_id}", check=False)
    record("node list (API reachable)", isinstance(nodes, (dict, list)), "")

    cli("member", "auth logout", check=False)
    after = cli("member", "auth status", check=False)
    record("logout revokes the session", owner not in json.dumps(after) and member not in json.dumps(after), "")

    cli("owner", "auth logout", check=False)
    record("owner logout", owner not in json.dumps(cli("owner", "auth status", check=False)), "")
    login("owner", owner, "signin")
    again = cli("owner", "workspace list")
    record("owner signs back in and still sees the workspace", ws_id in json.dumps(again), "")


try:
    main()
except SystemExit:
    pass
except Exception as error:
    results.append({"step": "harness", "ok": False, "detail": str(error)[:500]})
finally:
    sh("ben5", f"sudo -n docker ps -aq --filter name=blazn-qual-{RUN} | xargs -r sudo -n docker rm -f >/dev/null; sudo -n rm -rf {ROOT}", check=False)
    print(json.dumps({"run": RUN, "passed": sum(r["ok"] for r in results), "failed": sum(not r["ok"] for r in results), "results": results}, indent=1))
