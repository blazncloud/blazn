#!/usr/bin/env python3

import json
import pathlib
import sys

if len(sys.argv) != 3:
    raise SystemExit("usage: compare-target-state.py BEFORE AFTER")
before = json.loads(pathlib.Path(sys.argv[1]).read_text())
after = json.loads(pathlib.Path(sys.argv[2]).read_text())
for field in ("correlationId", "target", "source"):
    if before.get(field) != after.get(field):
        raise SystemExit(f"target inventory {field} changed")
if before.get("state") != after.get("state"):
    changed = sorted(set(before.get("state", {})) | set(after.get("state", {})))
    changed = [key for key in changed if before.get("state", {}).get(key) != after.get("state", {}).get(key)]
    raise SystemExit("target residue/drift in: " + ", ".join(changed))
print(json.dumps({"status": "passed", "state": after["state"]}, sort_keys=True))
