#!/usr/bin/env python3
"""Qualify the sandbox lifecycle against a hosted Blazn API.

For each iteration the script drives the published CLI through

    create -> ready -> exec -> upload -> download (SHA-256 compared) -> stop -> delete

and then proves that nothing is left behind: no Pod or Sandbox object carries
the sandbox's label, and no access grant for it is still active.

The CLI must already be signed in as a member who may create sandboxes in the
workspace. It runs locally, or on another host with --ssh. Cluster and database
checks are read-only.

    sandbox-lifecycle.py --workspace WORKSPACE --template NAME@VERSION \\
        --ssh "-J ben1 blazn@NODE" --blazn .local/bin/blazn --repeat 2 > report.json

Progress goes to stderr; the JSON report goes to stdout. No token, grant or
file content is printed.
"""
import argparse
import base64
import json
import os
import shlex
import subprocess
import sys
import time
import uuid

DEFAULT_KUBECTL = "ssh ben1 sudo -n microk8s kubectl"
SANDBOX_NAMESPACE = "blazn-poc-sandboxes"
SANDBOX_LABEL = "blazn.dev/sandbox-id"


class StepFailed(Exception):
    pass


class Runner:
    def __init__(self, options):
        self.options = options
        self.ssh = shlex.split(options.ssh) if options.ssh else None
        self.kubectl = shlex.split(options.kubectl)
        self.psql = shlex.split(options.psql) if options.psql else None

    def host(self, command, timeout=120):
        """Run a shell command where the CLI runs."""
        argv = ["ssh", "-o", "BatchMode=yes", *self.ssh, command] if self.ssh else ["sh", "-c", command]
        return subprocess.run(argv, capture_output=True, text=True, timeout=timeout)

    def blazn(self, *arguments, timeout=180):
        """Run the CLI with JSON output. Returns (exit code, parsed JSON or raw text)."""
        command = shlex.join([self.options.blazn, "--output", "json", *arguments])
        result = self.host(command, timeout=timeout)
        try:
            return result.returncode, json.loads(result.stdout)
        except json.JSONDecodeError:
            return result.returncode, (result.stdout or result.stderr).strip()[-300:]

    def cluster(self, *arguments):
        result = subprocess.run([*self.kubectl, *arguments], capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise StepFailed(f"kubectl failed: {result.stderr.strip()[-200:]}")
        return result.stdout

    def active_grants(self, sandbox_id):
        query = f"select count(*) from sandbox_access_grants where sandbox_id = '{sandbox_id}' and state = 'active'"
        result = subprocess.run([*self.psql, "-c", query], capture_output=True, text=True, timeout=60)
        if result.returncode or not result.stdout.strip().isdigit():
            raise StepFailed(f"grant query failed: {result.stderr.strip()[-200:]}")
        return int(result.stdout.strip())


def sandbox_of(value):
    if isinstance(value, dict):
        return value.get("sandbox", value)
    return {}


def error_code(value):
    if isinstance(value, dict) and isinstance(value.get("error"), dict):
        return value["error"].get("code", "")
    return ""


def wait_for_state(runner, sandbox_id, wanted, timeout, absent_is=None):
    deadline = time.time() + timeout
    state = "unknown"
    while time.time() < deadline:
        code, value = runner.blazn("sandbox", "get", sandbox_id)
        if absent_is and error_code(value) in ("not_found", "sandbox_not_found"):
            return absent_is
        state = sandbox_of(value).get("state", state) if code == 0 else state
        if state in wanted:
            return state
        if state == "failed" and "failed" not in wanted:
            raise StepFailed("sandbox entered the failed state")
        time.sleep(runner.options.poll)
    raise StepFailed(f"timed out in state {state!r} waiting for {'/'.join(sorted(wanted))}")


def digest_command(path):
    quoted = shlex.quote(path)
    return f"(sha256sum {quoted} 2>/dev/null || shasum -a 256 {quoted}) | cut -d' ' -f1"


def iteration(runner, number, steps):
    options = runner.options
    tag = uuid.uuid4().hex[:12]
    local = f"{options.scratch}/blazn-qual-{tag}"
    sandbox_id = None

    def step(name, ok, detail=""):
        steps.append({"iteration": number, "step": name, "ok": bool(ok), "detail": detail})
        print(f"{'PASS' if ok else 'FAIL'} [{number}] {name} {detail}", file=sys.stderr, flush=True)
        if not ok:
            raise StepFailed(name)

    try:
        create = ["sandbox", "create", "--template", options.template, "--arch", options.arch, "--mode", "direct",
                  "--expires", options.expires, "--approved-non-sensitive", "--workspace", options.workspace,
                  "--request-id", f"qual-create-{tag}"]
        for source in options.source:
            create += ["--source", source]
        code, value = runner.blazn(*create)
        sandbox_id = sandbox_of(value).get("id")
        step("create accepted", code == 0 and sandbox_id, sandbox_id or str(value)[:200])

        started = time.time()
        state = wait_for_state(runner, sandbox_id, {"ready", "running"}, options.ready_timeout)
        step("ready", True, f"{state} after {int(time.time() - started)}s")

        pods = json.loads(runner.cluster("-n", SANDBOX_NAMESPACE, "get", "pods", "-l", f"{SANDBOX_LABEL}={sandbox_id}", "-o", "json"))["items"]
        node = pods[0]["spec"].get("nodeName", "") if pods else ""
        step("runs as exactly one Pod", len(pods) == 1 and node, f"node {node}")

        code, value = runner.blazn("sandbox", "exec", sandbox_id, "--", *shlex.split(options.exec_command))
        output = base64.b64decode(value.get("stdoutBase64", "")).decode(errors="replace").strip() if isinstance(value, dict) else ""
        step("exec", code == 0 and isinstance(value, dict) and value.get("remoteExitCode") == 0 and output, output[:120])

        made = runner.host(f"head -c {options.size} /dev/urandom > {shlex.quote(local)}.up && {digest_command(local + '.up')}")
        expected = made.stdout.strip()
        step("test file created", made.returncode == 0 and len(expected) == 64, f"{options.size} bytes")

        code, value = runner.blazn("sandbox", "upload", sandbox_id, local + ".up", options.remote_path)
        step("upload", code == 0 and isinstance(value, dict) and value.get("sha256", "").split(":")[-1] == expected, "digest matches")

        code, value = runner.blazn("sandbox", "download", sandbox_id, options.remote_path, local + ".down")
        fetched = runner.host(digest_command(local + ".down")).stdout.strip()
        step("download", code == 0 and fetched == expected, "downloaded bytes match the uploaded file")

        code, value = runner.blazn("sandbox", "stop", sandbox_id, "--request-id", f"qual-stop-{tag}")
        step("stop accepted", code == 0, "" if code == 0 else str(value)[:200])
        step("stopped", wait_for_state(runner, sandbox_id, {"stopped"}, options.stop_timeout) == "stopped")

        code, value = runner.blazn("sandbox", "delete", sandbox_id, "--request-id", f"qual-delete-{tag}")
        step("delete accepted", code == 0, "" if code == 0 else str(value)[:200])
        step("deleted", wait_for_state(runner, sandbox_id, {"deleted"}, options.stop_timeout, absent_is="deleted") == "deleted")

        deadline, residue = time.time() + options.residue_timeout, None
        while time.time() < deadline:
            residue = runner.cluster("-n", SANDBOX_NAMESPACE, "get", "pods,sandboxes.agents.x-k8s.io", "-l",
                                     f"{SANDBOX_LABEL}={sandbox_id}", "-o", "name").split()
            if not residue:
                break
            time.sleep(options.poll)
        step("no Pod or Sandbox object remains", not residue, ", ".join(residue or []))

        if runner.psql:
            active = runner.active_grants(sandbox_id)
            step("no active access grant remains", active == 0, f"{active} active")
        else:
            steps.append({"iteration": number, "step": "no active access grant remains", "ok": None, "detail": "skipped: no --psql"})
        return {"iteration": number, "sandboxId": sandbox_id, "node": node, "ok": True}
    except StepFailed as failure:
        if steps and steps[-1]["ok"] is not False:
            steps.append({"iteration": number, "step": "harness", "ok": False, "detail": str(failure)})
            print(f"FAIL [{number}] {failure}", file=sys.stderr, flush=True)
        if sandbox_id and not options.keep_failed:
            runner.blazn("sandbox", "stop", sandbox_id, "--request-id", f"qual-cleanup-stop-{tag}")
            runner.blazn("sandbox", "delete", sandbox_id, "--request-id", f"qual-cleanup-delete-{tag}")
        return {"iteration": number, "sandboxId": sandbox_id, "ok": False}
    finally:
        runner.host(f"rm -f {shlex.quote(local)}.up {shlex.quote(local)}.down")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--workspace", required=True, help="workspace ID the signed-in user may create sandboxes in")
    parser.add_argument("--template", required=True, help="published template, NAME or NAME@VERSION")
    parser.add_argument("--arch", default="amd64", choices=["amd64", "arm64"])
    parser.add_argument("--source", action="append", default=[], help="REPOSITORY=COMMIT, repeatable")
    parser.add_argument("--expires", default="30m")
    parser.add_argument("--ssh", default="", help="ssh options and target where the CLI runs; omit to run it locally")
    parser.add_argument("--blazn", default="blazn", help="CLI path on that host")
    parser.add_argument("--scratch", default="/tmp", help="directory on that host for the test file")
    parser.add_argument("--kubectl", default=os.environ.get("KUBECTL", DEFAULT_KUBECTL), help="read-only cluster access")
    parser.add_argument("--psql", default=os.environ.get("BLAZN_QUAL_PSQL", ""), help="command prefix for a read-only psql on the hosted database (psql -At ...)")
    parser.add_argument("--skip-grant-check", action="store_true", help="allow running without --psql; the report marks the check as skipped")
    parser.add_argument("--exec-command", default="uname -sm")
    parser.add_argument("--remote-path", default="/workspace/tmp/blazn-qualification.bin")
    parser.add_argument("--size", type=int, default=65536)
    parser.add_argument("--repeat", type=int, default=1)
    parser.add_argument("--ready-timeout", type=int, default=600)
    parser.add_argument("--stop-timeout", type=int, default=300)
    parser.add_argument("--residue-timeout", type=int, default=180)
    parser.add_argument("--poll", type=float, default=5)
    parser.add_argument("--keep-failed", action="store_true", help="leave a failed sandbox in place for inspection")
    options = parser.parse_args()
    if not options.psql and not options.skip_grant_check:
        parser.error("--psql is required (or pass --skip-grant-check)")
    if options.repeat < 1 or options.size < 1:
        parser.error("--repeat and --size must be positive")

    runner, steps, iterations = Runner(options), [], []
    for number in range(1, options.repeat + 1):
        iterations.append(iteration(runner, number, steps))
        if not iterations[-1]["ok"]:
            break
    passed = len(iterations) == options.repeat and all(item["ok"] for item in iterations)
    print(json.dumps({
        "passed": passed, "template": options.template, "workspace": options.workspace, "repeat": options.repeat,
        "iterations": iterations, "steps": steps,
        "skipped": [s["step"] for s in steps if s["ok"] is None],
    }, indent=1))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
