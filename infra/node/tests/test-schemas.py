#!/usr/bin/env python3
import json
import pathlib

try:
    import jsonschema
except ImportError:
    print("Node schema semantic validation skipped: python jsonschema unavailable")
    raise SystemExit(0)

root = pathlib.Path(__file__).resolve().parents[1]
template = json.loads((root / "node-install-plan-template.schema.json").read_text())
issuer = json.loads((root / "microk8s-worker-issuer-receipt.schema.json").read_text())
for schema in (template, issuer):
    jsonschema.Draft202012Validator.check_schema(schema)
jsonschema.Draft202012Validator(template).validate(json.loads((root / "templates" / "node-install-plan-template-v1.json").read_text()))
print("Node JSON Schemas validated")
