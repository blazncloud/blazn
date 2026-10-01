#!/bin/sh
set -eu

# Exercises sandbox-lifecycle.py against a fake CLI, cluster and database, so
# its sequencing and its failure detection are tested without a live sandbox.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/state" "$tmp/scratch"

cat >"$tmp/blazn" <<'PY'
#!/usr/bin/env python3
import base64, hashlib, json, os, pathlib, shutil, sys, uuid
state = pathlib.Path(os.environ["FAKE_STATE"])
args = sys.argv[1:]
assert args[:3] == ["--output", "json", "sandbox"], args
command, rest = args[3], args[4:]
def load(i): return json.loads((state / i).read_text())
def save(i, v): (state / i).write_text(json.dumps(v))
def out(v, code=0): print(json.dumps(v)); sys.exit(code)
if command == "create":
    i = str(uuid.uuid4()); save(i, {"state": "requested", "polls": 0}); out({"sandbox": {"id": i, "state": "requested"}})
i = rest[0]
if not (state / i).exists():
    out({"error": {"code": "not_found", "message": "sandbox not found"}}, 1)
record = load(i)
if command == "get":
    record["polls"] += 1
    order = {"requested": "provisioning", "provisioning": "ready", "stopping": "stopped", "deleting": None}
    if record["polls"] % 2 == 0 and record["state"] in order:
        nxt = order[record["state"]]
        if nxt is None:
            (state / i).unlink()
            if os.environ.get("FAKE_LEAK") != "1": (state / (i + ".pod")).unlink(missing_ok=True)
            out({"error": {"code": "not_found", "message": "sandbox not found"}}, 1)
        record["state"] = nxt
        if nxt == "ready": (state / (i + ".pod")).write_text("x")
    save(i, record); out({"id": i, "state": record["state"]})
if command == "exec":
    out({"sandboxId": i, "remoteExitCode": 0, "stdoutBase64": base64.b64encode(b"Linux x86_64\n").decode(), "stderrBase64": "", "truncated": False})
if command in ("upload", "download"):
    src, dst = rest[1], rest[2]
    stored = state / (i + ".file")
    if command == "upload": shutil.copyfile(src, stored)
    else: shutil.copyfile(stored, dst)
    out({"sandboxId": i, "sha256": hashlib.sha256(stored.read_bytes()).hexdigest(), "size": stored.stat().st_size})
if command == "stop": record["state"] = "stopping"; save(i, record); out({"status": "accepted"})
if command == "delete": record["state"] = "deleting"; save(i, record); out({"status": "accepted"})
out({"error": {"code": "usage"}}, 2)
PY
cat >"$tmp/kubectl" <<'PY'
#!/usr/bin/env python3
import json, os, pathlib, sys
state = pathlib.Path(os.environ["FAKE_STATE"])
args = sys.argv[1:]
sandbox = args[args.index("-l") + 1].split("=", 1)[1]
alive = (state / (sandbox + ".pod")).exists()
if args[args.index("-o") + 1] == "json":
    print(json.dumps({"items": [{"spec": {"nodeName": "fake-node"}}] if alive else []}))
elif alive:
    print("pod/" + sandbox)
PY
printf '#!/bin/sh\nprintf "%%s\\n" "${FAKE_ACTIVE_GRANTS:-0}"\n' >"$tmp/psql"
chmod +x "$tmp/blazn" "$tmp/kubectl" "$tmp/psql"

run() {
  FAKE_STATE="$tmp/state" python3 "$ROOT/sandbox-lifecycle.py" --workspace 11111111-1111-4111-8111-111111111111 \
    --template coding-agent@1 --blazn "$tmp/blazn" --kubectl "$tmp/kubectl" --psql "$tmp/psql" --scratch "$tmp/scratch" \
    --poll 0 --residue-timeout 1 "$@"
}

run --repeat 2 >"$tmp/pass.json" 2>"$tmp/pass.err" || { cat "$tmp/pass.err" >&2; printf 'healthy lifecycle was rejected\n' >&2; exit 1; }
python3 - "$tmp/pass.json" <<'PY'
import json, sys
report = json.load(open(sys.argv[1]))
assert report["passed"] and len(report["iterations"]) == 2 and not report["skipped"], report
assert all(item["node"] == "fake-node" for item in report["iterations"]), report
assert sum(1 for step in report["steps"] if step["ok"]) == 26, report["steps"]
PY
[ -z "$(ls "$tmp/scratch")" ] || { printf 'test files were left behind\n' >&2; exit 1; }

if (export FAKE_ACTIVE_GRANTS=1; run >"$tmp/grant.json" 2>/dev/null); then printf 'an active grant was not detected\n' >&2; exit 1; fi
grep -q '"no active access grant remains"' "$tmp/grant.json"
if (export FAKE_LEAK=1; run >"$tmp/leak.json" 2>/dev/null); then printf 'a leftover Pod was not detected\n' >&2; exit 1; fi
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); assert not r["passed"] and r["steps"][-1]["step"]=="no Pod or Sandbox object remains", r["steps"][-1]' "$tmp/leak.json"
if run --psql "" >/dev/null 2>&1; then printf 'a missing grant check was silently accepted\n' >&2; exit 1; fi
run --psql "" --skip-grant-check >"$tmp/skip.json" 2>"$tmp/skip.err" || { cat "$tmp/skip.err" >&2; exit 1; }
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); assert r["passed"] and r["skipped"]==["no active access grant remains"], r' "$tmp/skip.json"
printf 'sandbox lifecycle qualification self-test passed\n'
