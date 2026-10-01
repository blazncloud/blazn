#!/usr/bin/env python3
"""Qualify an Agent Run against a hosted Blazn API.

The script drives the CLI through one conversation with an Agent that runs in
a Sandbox on a registered Node:

    run create -> prompt -> reply -> follow-up -> reply -> finished
        -> patch and summary Artifacts -> Sandbox released

It then proves that the Run left nothing behind: the Run's Sandbox has no Pod
and no access grant is still active.

The CLI must already be signed in as a member who may create Runs in the
selected project, and the Agent must exist (`blazn agent quickstart`). The CLI
runs locally, or on another host with --ssh. Cluster and database checks are
read-only.

    agent-run.py --agent-version ID --harness-profile ID \\
        --ssh ben5 --blazn /tmp/blazn \\
        --psql "ssh DATABASE_HOST sudo -n -u postgres psql -d blazn_test -At" > report.json

Progress goes to stderr; the JSON report goes to stdout. Message content is
reported only as a short prefix; no token or grant is printed.
"""
import argparse
import json
import os
import re
import shlex
import subprocess
import sys
import time
import uuid

DEFAULT_KUBECTL = "ssh ben1 sudo -n microk8s kubectl"
SANDBOX_NAMESPACE = "blazn-poc-sandboxes"
SANDBOX_LABEL = "blazn.dev/sandbox-id"
UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
TERMINAL = {"succeeded", "failed", "cancelled"}


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
        command = self.options.blazn_prefix + shlex.join([self.options.blazn, "--output", "json", *arguments])
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

    def query(self, sql):
        # The query goes on stdin: a --psql prefix that runs over ssh would
        # re-split a -c argument in the remote shell.
        result = subprocess.run(self.psql, input=sql + ";\n", capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise StepFailed(f"database query failed: {result.stderr.strip()[-200:]}")
        return result.stdout.strip()


def run_of(value):
    return value.get("run", value) if isinstance(value, dict) else {}


def messages(runner, run_id):
    code, value = runner.blazn("run", "messages", run_id)
    if code != 0 or not isinstance(value, dict):
        return []
    return value.get("items", [])


def wait_for_reply(runner, run_id, parent_id, timeout):
    """Waits for the Agent's reply to one message. Fails early if the Run ends."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        for message in messages(runner, run_id):
            if message.get("kind") == "reply" and message.get("parentMessageId") == parent_id:
                return message
        code, value = runner.blazn("run", "get", run_id)
        status = run_of(value).get("status", "") if code == 0 else ""
        if status in TERMINAL:
            raise StepFailed(f"the Run ended as {status} before replying")
        time.sleep(runner.options.poll)
    raise StepFailed("timed out waiting for the Agent's reply")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--agent-version", required=True, help="Agent Version ID (blazn agent quickstart prints it)")
    parser.add_argument("--harness-profile", required=True, help="Harness Profile ID")
    parser.add_argument("--arch", default="amd64", choices=["amd64", "arm64"])
    parser.add_argument("--expires", type=int, default=1800, help="Sandbox lifetime in seconds")
    parser.add_argument("--prompt", default="Record that the qualification prompt arrived.")
    parser.add_argument("--followup", default="Record that the qualification follow-up arrived.")
    parser.add_argument("--expect-in-patch", default="", help="text the patch Artifact must contain (the stub model writes BLAZN_AGENT_PROOF.md)")
    parser.add_argument("--ssh", default="", help="ssh options and target where the CLI runs; omit to run it locally")
    parser.add_argument("--blazn", default="blazn", help="CLI path on that host")
    parser.add_argument("--blazn-prefix", default="", help="shell text placed before the CLI command, for example 'HOME=/tmp/qual '")
    parser.add_argument("--scratch", default="/tmp", help="directory on that host for the downloaded patch")
    parser.add_argument("--kubectl", default=os.environ.get("KUBECTL", DEFAULT_KUBECTL), help="read-only cluster access")
    parser.add_argument("--psql", default=os.environ.get("BLAZN_QUAL_PSQL", ""), help="command prefix for a read-only psql on the hosted database (psql -At ...)")
    parser.add_argument("--skip-release-check", action="store_true", help="allow running without --psql; the report marks the Sandbox checks as skipped")
    parser.add_argument("--reply-timeout", type=int, default=900, help="seconds to wait for a reply; the first one includes Sandbox start")
    parser.add_argument("--finish-timeout", type=int, default=900, help="seconds to wait for the idle Run to finish")
    parser.add_argument("--release-timeout", type=int, default=300)
    parser.add_argument("--poll", type=float, default=5)
    parser.add_argument("--keep-failed", action="store_true", help="do not cancel a failed Run")
    options = parser.parse_args()
    if not options.psql and not options.skip_release_check:
        parser.error("--psql is required (or pass --skip-release-check)")
    for name in ("agent_version", "harness_profile"):
        if not UUID.match(getattr(options, name)):
            parser.error(f"--{name.replace('_', '-')} must be an ID")

    runner, steps, tag = Runner(options), [], uuid.uuid4().hex[:12]
    run_id, finished, patch_file = None, False, f"{options.scratch}/blazn-agent-run-{tag}.patch"

    def step(name, ok, detail=""):
        steps.append({"step": name, "ok": bool(ok), "detail": detail})
        print(f"{'PASS' if ok else 'FAIL'} {name} {detail}", file=sys.stderr, flush=True)
        if not ok:
            raise StepFailed(name)

    def skip(name, why):
        steps.append({"step": name, "ok": None, "detail": f"skipped: {why}"})

    try:
        code, value = runner.blazn("run", "create", "--agent-version", options.agent_version, "--harness-profile", options.harness_profile,
                                   "--arch", options.arch, "--expires", str(options.expires), "--request-id", f"qual-run-{tag}")
        run_id = run_of(value).get("id")
        step("run accepted", code == 0 and run_id, run_id or str(value)[:200])

        conversation = [("prompt", options.prompt, None), ("followup", options.followup, "prompt")]
        sent = {}
        for kind, content, parent in conversation:
            arguments = ["run", "send", run_id, "--kind", kind, "--content", content, "--request-id", f"qual-{kind}-{tag}"]
            if parent:
                arguments += ["--parent", sent[parent]]
            code, value = runner.blazn(*arguments)
            message_id = value.get("message", {}).get("id") if isinstance(value, dict) else None
            step(f"{kind} queued", code == 0 and message_id, "" if code == 0 else str(value)[:200])
            sent[kind] = message_id
            started = time.time()
            reply = wait_for_reply(runner, run_id, message_id, options.reply_timeout)
            text = str(reply.get("content", ""))
            step(f"reply to {kind}", text.strip() != "", f"after {int(time.time() - started)}s: {text[:80]!r}")

        delivered = {m.get("id"): m.get("status") for m in messages(runner, run_id)}
        step("messages delivered", all(delivered.get(i) == "delivered" for i in sent.values()), json.dumps({k: delivered.get(i) for k, i in sent.items()}))

        deadline, status = time.time() + options.finish_timeout, ""
        while time.time() < deadline and status not in TERMINAL:
            code, value = runner.blazn("run", "get", run_id)
            status = run_of(value).get("status", status) if code == 0 else status
            if status not in TERMINAL:
                time.sleep(options.poll)
        finished = status in TERMINAL
        step("run succeeded", status == "succeeded", status or "no status")

        code, value = runner.blazn("run", "artifacts", run_id)
        artifacts = {a.get("name"): a for a in value.get("items", [])} if isinstance(value, dict) else {}
        ready = all(artifacts.get(name, {}).get("status") == "ready" for name in ("patch", "summary"))
        step("patch and summary Artifacts are ready", code == 0 and ready, ", ".join(sorted(str(name) for name in artifacts)))

        code, value = runner.blazn("run", "download", artifacts["patch"]["id"], patch_file)
        size = value.get("sizeBytes", 0) if isinstance(value, dict) else 0
        step("patch downloads", code == 0 and size > 0, f"{size} bytes")
        if options.expect_in_patch:
            found = runner.host(f"grep -c -F -- {shlex.quote(options.expect_in_patch)} {shlex.quote(patch_file)}").stdout.strip()
            step("patch holds the Agent's change", found.isdigit() and int(found) > 0, options.expect_in_patch)

        if runner.psql:
            sandbox_id = runner.query(f"select sandbox_id from agent_run_executions where run_id = '{run_id}'")
            step("run has one Sandbox", bool(UUID.match(sandbox_id)), sandbox_id)
            deadline, residue = time.time() + options.release_timeout, None
            while time.time() < deadline:
                residue = runner.cluster("-n", SANDBOX_NAMESPACE, "get", "pods", "-l", f"{SANDBOX_LABEL}={sandbox_id}", "-o", "name").split()
                if not residue:
                    break
                time.sleep(options.poll)
            step("Sandbox Pod is gone", not residue, ", ".join(residue or []))
            active = runner.query(f"select count(*) from sandbox_access_grants where sandbox_id = '{sandbox_id}' and state = 'active'")
            step("no active access grant remains", active == "0", f"{active} active")
        else:
            skip("Sandbox Pod is gone", "no --psql")
            skip("no active access grant remains", "no --psql")
        passed = True
    except StepFailed as failure:
        passed = False
        if steps and steps[-1]["ok"] is not False:
            steps.append({"step": "harness", "ok": False, "detail": str(failure)})
            print(f"FAIL {failure}", file=sys.stderr, flush=True)
        if run_id and not finished and not options.keep_failed:
            code, value = runner.blazn("run", "get", run_id)
            version = run_of(value).get("version")
            if code == 0 and version and run_of(value).get("status") not in TERMINAL:
                runner.blazn("run", "cancel", run_id, "--expected-version", str(version), "--request-id", f"qual-cancel-{tag}")
    finally:
        runner.host(f"rm -f {shlex.quote(patch_file)}")

    print(json.dumps({"passed": passed, "runId": run_id, "agentVersion": options.agent_version, "harnessProfile": options.harness_profile,
                      "steps": steps, "skipped": [s["step"] for s in steps if s["ok"] is None]}, indent=1))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
