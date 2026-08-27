#!/usr/bin/env python3
"""Emit the exact Sandbox shape internal/sandboxcontrol/adapter.go renders.

Usage: good-sandbox.py [mutation]
Mutations produce a single deliberately invalid variant for admission tests.
"""
import copy
import json
import sys

NAME = "31000000-0000-4000-8000-000000000001"
WORKSPACE = "32000000-0000-4000-8000-000000000002"
OWNER = "33000000-0000-4000-8000-000000000003"
MAIN_IMAGE = "registry.example/blazn/coding-agent@sha256:" + "a" * 64
HELPER_IMAGE = "registry.example/blazn/sandbox-io@sha256:" + "b" * 64
RESTRICTED = {
    "allowPrivilegeEscalation": False,
    "privileged": False,
    "readOnlyRootFilesystem": True,
    "capabilities": {"drop": ["ALL"]},
}


def helper(name, subcommand, mounts, sidecar):
    container = {
        "name": name,
        "image": HELPER_IMAGE,
        "command": ["/blazn-sandbox-io", subcommand],
        "securityContext": copy.deepcopy(RESTRICTED),
        "resources": {
            "requests": {"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"},
            "limits": {"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"},
        },
        "volumeMounts": mounts,
    }
    if sidecar:
        container["restartPolicy"] = "Always"
    return container


def good():
    labels = {
        "blazn.dev/managed": "true",
        "blazn.dev/workspace": WORKSPACE,
        "blazn.dev/owner": OWNER,
        "blazn.dev/sandbox-id": NAME,
    }
    pod_labels = dict(labels)
    pod_labels["kueue.x-k8s.io/queue-name"] = "blazn-poc"
    return {
        "apiVersion": "agents.x-k8s.io/v1beta1",
        "kind": "Sandbox",
        "metadata": {
            "name": NAME,
            "namespace": "blazn-poc-sandboxes",
            "labels": labels,
            "annotations": {
                "sandboxes.blazn.dev/trust-level": "approved_non_sensitive_poc",
                "sandboxes.blazn.dev/expires-at": "2026-08-27T23:59:59.000000000Z",
                "sandboxes.blazn.dev/artifact-exports": json.dumps(
                    [{"name": "change.patch", "path": "/workspace/artifacts/change.patch"}]
                ),
                "sandboxes.blazn.dev/artifact-contract-digest": "sha256:" + "c" * 64,
                "sandboxes.blazn.dev/create-intent-digest": "sha256:" + "d" * 64,
            },
            "finalizers": ["sandboxes.blazn.dev/artifact-cleanup"],
        },
        "spec": {
            "shutdownPolicy": "Delete",
            "operatingMode": "Running",
            "podTemplate": {
                "metadata": {"labels": pod_labels},
                "spec": {
                    "serviceAccountName": "blazn-sandbox-runner",
                    "automountServiceAccountToken": False,
                    "restartPolicy": "Never",
                    "nodeSelector": {
                        "kubernetes.io/arch": "amd64",
                        "blazn.dev/sandbox-eligible": "true",
                    },
                    "securityContext": {
                        "runAsNonRoot": True,
                        "runAsUser": 65532,
                        "runAsGroup": 65532,
                        "fsGroup": 65532,
                        "seccompProfile": {"type": "RuntimeDefault"},
                    },
                    "volumes": [
                        {"name": "source-00", "emptyDir": {"sizeLimit": "2Gi"}},
                        {"name": "bootstrap-state", "emptyDir": {"medium": "Memory", "sizeLimit": "1Mi"}},
                        {"name": "artifacts", "emptyDir": {"sizeLimit": "2Gi"}},
                    ],
                    "initContainers": [
                        helper(
                            "sandbox-bootstrap",
                            "wait-bootstrap",
                            [
                                {"name": "source-00", "mountPath": "/workspace/src/blazn"},
                                {"name": "bootstrap-state", "mountPath": "/run/blazn-bootstrap"},
                            ],
                            sidecar=False,
                        ),
                        helper(
                            "sandbox-artifact-io",
                            "wait-artifact",
                            [{"name": "artifacts", "mountPath": "/workspace/artifacts", "readOnly": True}],
                            sidecar=True,
                        ),
                    ],
                    "containers": [
                        {
                            "name": "main",
                            "image": MAIN_IMAGE,
                            "command": ["node", "--version"],
                            "securityContext": copy.deepcopy(RESTRICTED),
                            "resources": {
                                "requests": {"cpu": "500m", "memory": "1Gi", "ephemeral-storage": "2Gi"},
                                "limits": {"cpu": "1", "memory": "2Gi", "ephemeral-storage": "4Gi"},
                            },
                            "volumeMounts": [
                                {"name": "source-00", "mountPath": "/workspace/src/blazn"},
                                {"name": "artifacts", "mountPath": "/workspace/artifacts"},
                            ],
                        }
                    ],
                },
            },
        },
    }


def mutate(doc, mutation):
    meta = doc["metadata"]
    pod = doc["spec"]["podTemplate"]["spec"]
    main = pod["containers"][0]
    if mutation == "bad-name":
        meta["name"] = "not-a-uuid"
        meta["labels"]["blazn.dev/sandbox-id"] = "not-a-uuid"
        doc["spec"]["podTemplate"]["metadata"]["labels"]["blazn.dev/sandbox-id"] = "not-a-uuid"
    elif mutation == "missing-managed-label":
        del meta["labels"]["blazn.dev/managed"]
    elif mutation == "wrong-queue":
        doc["spec"]["podTemplate"]["metadata"]["labels"]["kueue.x-k8s.io/queue-name"] = "other-queue"
    elif mutation == "tag-image":
        main["image"] = "registry.example/blazn/coding-agent:latest"
    elif mutation == "host-network":
        pod["hostNetwork"] = True
    elif mutation == "extra-node-selector":
        pod["nodeSelector"]["topology.kubernetes.io/zone"] = "z1"
    elif mutation == "wrong-service-account":
        pod["serviceAccountName"] = "default"
    elif mutation == "token-automount":
        pod["automountServiceAccountToken"] = True
    elif mutation == "over-cpu":
        main["resources"]["limits"]["cpu"] = "8"
    elif mutation == "host-path-volume":
        pod["volumes"].append({"name": "artifacts2", "hostPath": {"path": "/etc"}})
    elif mutation == "extra-container":
        pod["containers"].append(copy.deepcopy(main))
        pod["containers"][1]["name"] = "second"
    elif mutation == "env-injection":
        main["env"] = [{"name": "X", "value": "y"}]
    elif mutation == "runtime-class":
        pod["runtimeClassName"] = "runsc"
    elif mutation == "missing-trust":
        del meta["annotations"]["sandboxes.blazn.dev/trust-level"]
    elif mutation == "priv-escalation":
        main["securityContext"]["allowPrivilegeEscalation"] = True
    elif mutation == "foreign-helper":
        pod["initContainers"][0]["command"] = ["/bin/sh", "-c", "curl evil"]
    elif mutation == "shutdown-retain":
        doc["spec"]["shutdownPolicy"] = "Retain"
    elif mutation == "wrong-workspace-shape":
        meta["labels"]["blazn.dev/workspace"] = "not-a-uuid"
    elif mutation == "mount-traversal":
        main["volumeMounts"][1]["mountPath"] = "/etc"
    elif mutation == "mount-subpath":
        main["volumeMounts"][0]["subPath"] = "../escape"
    elif mutation == "init-ephemeral-oversize":
        pod["initContainers"][0]["resources"]["limits"]["ephemeral-storage"] = "500Gi"
    elif mutation == "volume-size-oversize":
        pod["volumes"][0]["emptyDir"]["sizeLimit"] = "5000Gi"
    else:
        raise SystemExit(f"unknown mutation {mutation}")
    return doc


def many_sources(doc):
    """The adapter contract permits up to 32 sources; render a wide legal shape."""
    pod = doc["spec"]["podTemplate"]["spec"]
    bootstrap = pod["initContainers"][0]
    for index in range(1, 8):
        name = f"source-{index:02d}"
        destination = f"/workspace/src/extra-{index}"
        pod["volumes"].insert(index, {"name": name, "emptyDir": {"sizeLimit": "2Gi"}})
        bootstrap["volumeMounts"].insert(index, {"name": name, "mountPath": destination})
        pod["containers"][0]["volumeMounts"].insert(index, {"name": name, "mountPath": destination})
    return doc


def minimal(doc):
    """The no-source, no-artifact adapter shape, optionally on a real image.

    BLAZN_TEST_MAIN_IMAGE and BLAZN_TEST_MAIN_COMMAND (JSON argv) let a live
    or disposable cluster run the fixture as a real Pod.
    """
    import os

    pod = doc["spec"]["podTemplate"]["spec"]
    del pod["volumes"]
    del pod["initContainers"]
    main = pod["containers"][0]
    del main["volumeMounts"]
    image = os.environ.get("BLAZN_TEST_MAIN_IMAGE")
    if image:
        main["image"] = image
    command = os.environ.get("BLAZN_TEST_MAIN_COMMAND")
    if command:
        main["command"] = json.loads(command)
    main["resources"] = {
        "requests": {"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"},
        "limits": {"cpu": "200m", "memory": "128Mi", "ephemeral-storage": "128Mi"},
    }
    doc["metadata"]["annotations"]["sandboxes.blazn.dev/artifact-exports"] = "[]"
    return doc


doc = good()
if len(sys.argv) > 1:
    if sys.argv[1] == "many-sources":
        doc = many_sources(doc)
    elif sys.argv[1] == "minimal":
        doc = minimal(doc)
    else:
        doc = mutate(doc, sys.argv[1])
json.dump(doc, sys.stdout)
