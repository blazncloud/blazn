#!/usr/bin/env python3

import json
import pathlib
import sys

if len(sys.argv) != 3:
    raise SystemExit("usage: compare-invariants.py BEFORE AFTER")
before = json.loads(pathlib.Path(sys.argv[1]).read_text())
after = json.loads(pathlib.Path(sys.argv[2]).read_text())
errors = []
if before.get("correlationId") != after.get("correlationId"):
    errors.append("correlation changed")
if before.get("source") != after.get("source"):
    errors.append("source HEAD/tree changed")
if before.get("protected") != after.get("protected"):
    errors.append("protected existing workload/HomeAI inventory changed")
if errors:
    raise SystemExit("; ".join(errors))
print(json.dumps({"status": "passed", "protected": after["protected"]}, sort_keys=True))
