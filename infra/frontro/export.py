#!/usr/bin/env python3
"""Export the Blazn hosting objects from the Frontro cluster as reviewed manifests.

The cluster is read with the command in $KUBECTL (default: kubectl), for example

    KUBECTL="ssh ben1 sudo -n microk8s kubectl" infra/frontro/export.py

Runtime-only fields are stripped so the files are stable and re-applicable.
Secret values are never read into the repository: only each Secret's name,
type and key names are recorded, in secrets.json.

    export.py           rewrite the manifests under infra/frontro/
    export.py --check   fail if the live cluster differs from the repository
"""
import json
import os
import pathlib
import shlex
import subprocess
import sys
import tempfile

import yaml

ROOT = pathlib.Path(__file__).resolve().parent
NAMESPACES = ["blazn-test", "blazn-identity-dev"]
KINDS = "deployment,service,configmap,networkpolicy,serviceaccount,role,rolebinding,persistentvolumeclaim"
SKIP = {("ConfigMap", "kube-root-ca.crt"), ("ServiceAccount", "default")}
DROP_ANNOTATION_PREFIXES = (
    "kubectl.kubernetes.io/", "deployment.kubernetes.io/", "pv.kubernetes.io/",
    "volume.beta.kubernetes.io/", "volume.kubernetes.io/",
)
EDGE_CONFIGMAP = ("moments-direct", "moments-direct-gateway", "dynamic.yml")


class Dumper(yaml.SafeDumper):
    pass


def _str(dumper, value):
    style = "|" if "\n" in value else None
    return dumper.represent_scalar("tag:yaml.org,2002:str", value, style=style)


Dumper.add_representer(str, _str)


def kubectl(*args):
    command = shlex.split(os.environ.get("KUBECTL", "kubectl")) + list(args)
    return subprocess.run(command, check=True, capture_output=True, text=True).stdout


def clean_metadata(metadata, namespaced=True):
    kept = {"name": metadata["name"]}
    if namespaced:
        kept["namespace"] = metadata["namespace"]
    labels = {k: v for k, v in (metadata.get("labels") or {}).items() if k != "kubernetes.io/metadata.name"}
    annotations = {k: v for k, v in (metadata.get("annotations") or {}).items() if not k.startswith(DROP_ANNOTATION_PREFIXES)}
    if labels:
        kept["labels"] = labels
    if annotations:
        kept["annotations"] = annotations
    return kept


def clean(item):
    kind = item["kind"]
    out = {"apiVersion": item["apiVersion"], "kind": kind, "metadata": clean_metadata(item["metadata"], kind != "Namespace")}
    for key in ("data", "binaryData", "rules", "roleRef", "subjects", "automountServiceAccountToken"):
        if key in item:
            out[key] = item[key]
    spec = json.loads(json.dumps(item.get("spec"))) if "spec" in item else None
    if kind == "Service":
        for key in ("clusterIP", "clusterIPs"):
            spec.pop(key, None)
    if kind == "PersistentVolumeClaim":
        spec.pop("volumeName", None)
    if kind == "Namespace":
        spec = None
    if kind == "Deployment":
        template = spec["template"]["metadata"]
        template.pop("creationTimestamp", None)
        annotations = {k: v for k, v in (template.get("annotations") or {}).items() if not k.startswith(DROP_ANNOTATION_PREFIXES)}
        template.pop("annotations", None)
        if annotations:
            template["annotations"] = annotations
    if spec is not None:
        out["spec"] = spec
    return out


def dump(value):
    return yaml.dump(value, Dumper=Dumper, sort_keys=True, default_flow_style=False, width=1000)


def blazn_edge_routes(dynamic):
    document = yaml.safe_load(dynamic)
    selected = {}
    for section, groups in document.items():
        for group, entries in (groups or {}).items():
            if not isinstance(entries, dict):
                continue
            for name, value in entries.items():
                if name.startswith("blazn-"):
                    selected.setdefault(section, {}).setdefault(group, {})[name] = value
    return selected


def render(target):
    namespaces = json.loads(kubectl("get", "namespace", *NAMESPACES, "-o", "json"))["items"]
    (target / "namespaces.yaml").write_text("---\n".join(dump(clean(n)) for n in sorted(namespaces, key=lambda n: n["metadata"]["name"])))
    inventory = {}
    for namespace in NAMESPACES:
        directory = target / namespace
        directory.mkdir(parents=True, exist_ok=True)
        names = []
        items = json.loads(kubectl("-n", namespace, "get", KINDS, "-o", "json"))["items"]
        for item in sorted(items, key=lambda i: (i["kind"], i["metadata"]["name"])):
            if (item["kind"], item["metadata"]["name"]) in SKIP:
                continue
            name = f'{item["kind"].lower()}-{item["metadata"]["name"]}.yaml'
            (directory / name).write_text(dump(clean(item)))
            names.append(name)
        (directory / "kustomization.yaml").write_text(dump({
            "apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": names}))
        secrets = json.loads(kubectl("-n", namespace, "get", "secret", "-o", "json"))["items"]
        inventory[namespace] = [
            {"name": s["metadata"]["name"], "type": s["type"], "keys": sorted((s.get("data") or {}).keys())}
            for s in sorted(secrets, key=lambda s: s["metadata"]["name"])]
    (target / "secrets.json").write_text(json.dumps(inventory, indent=2) + "\n")
    namespace, name, key = EDGE_CONFIGMAP
    dynamic = json.loads(kubectl("-n", namespace, "get", "configmap", name, "-o", "json"))["data"][key]
    (target / "edge").mkdir(exist_ok=True)
    (target / "edge" / "blazn-routes.yaml").write_text(
        "# Reference only: the blazn-* entries of ConfigMap moments-direct/moments-direct-gateway (dynamic.yml).\n"
        "# That gateway is shared Frontro infrastructure; change it there, then re-export.\n" + dump(blazn_edge_routes(dynamic)))


def generated(root):
    paths = [root / "namespaces.yaml", root / "secrets.json", root / "edge" / "blazn-routes.yaml"]
    for namespace in NAMESPACES:
        paths += sorted((root / namespace).glob("*.yaml"))
    return {str(p.relative_to(root)): p.read_text() for p in paths if p.exists()}


def main():
    if sys.argv[1:] == ["--check"]:
        with tempfile.TemporaryDirectory() as scratch:
            render(pathlib.Path(scratch))
            live, committed = generated(pathlib.Path(scratch)), generated(ROOT)
        drift = sorted(name for name in set(live) | set(committed) if live.get(name) != committed.get(name))
        for name in drift:
            print(f"drift: {name}")
        print("live cluster matches infra/frontro" if not drift else f"{len(drift)} file(s) differ")
        return 1 if drift else 0
    if sys.argv[1:]:
        print(__doc__)
        return 64
    for namespace in NAMESPACES:
        for stale in (ROOT / namespace).glob("*.yaml"):
            stale.unlink()
    render(ROOT)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
