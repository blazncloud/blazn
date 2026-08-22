#!/usr/bin/env python3
"""Create, append to, finalize, and verify fail-closed qualification evidence."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import uuid
from typing import Any

CANONICAL_REMOTE = "https://github.com/blazncloud/blazn.git"
CORRELATION = re.compile(r"^nodequal-[a-z0-9][a-z0-9.-]{6,63}$")
STEP = re.compile(r"^[a-z][a-z0-9-]{1,63}$")
DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA = re.compile(r"^[0-9a-f]{40}$")
EMPTY_STATUS_DIGEST = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
FORBIDDEN_ARTIFACT_MARKERS = (
    b'"accesstoken"', b'"refreshtoken"', b'"enrollmenttoken"',
    b'"joincredential"', b'authorization: bearer ', b'private key-----',
)
FRESH_GATES = {
    "source-provenance", "baseline-invariants", "target-baseline", "ubuntu-preflight",
    "service-identity", "no-input-sudo-observe", "install", "idempotent-install",
    "repair", "expired-observe", "expired-repair-denied", "expired-uninstall",
    "install-crash-resume", "cleanup-crash-resume", "reinstall",
    "kubernetes-uid-rv", "kubernetes-stale-cas-denied",
    "kubernetes-quarantine-noschedule", "target-post-uninstall", "zero-residue", "post-invariants",
}
MAC_GATES = {
    "source-provenance", "baseline-invariants", "target-baseline", "native-mac-preflight",
    "service-identity", "no-input-sudo-observe", "adopt-install", "idempotent-install",
    "repair", "expired-observe", "expired-repair-denied", "expired-uninstall",
    "cleanup-crash-resume", "reinstall", "kubernetes-uid-rv",
    "kubernetes-stale-cas-denied", "kubernetes-quarantine-noschedule",
    "target-post-uninstall", "zero-residue", "post-invariants",
}


def die(message: str) -> None:
    raise SystemExit(f"node qualification evidence: {message}")


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def digest_bytes(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


def digest_file(path: pathlib.Path) -> str:
    return digest_bytes(path.read_bytes())


def artifact_has_forbidden_marker(path: pathlib.Path) -> bytes | None:
    # Evidence logs are bounded operational artifacts. Scan in chunks while
    # preserving enough overlap to catch a marker split at a chunk boundary.
    overlap = max(map(len, FORBIDDEN_ARTIFACT_MARKERS)) - 1
    prior = b""
    with path.open("rb") as stream:
        while chunk := stream.read(1 << 20):
            value = prior + chunk.lower()
            for marker in FORBIDDEN_ARTIFACT_MARKERS:
                if marker in value:
                    return marker
            prior = value[-overlap:]
    return None


def artifact_json(path: pathlib.Path, step: str) -> Any:
    try:
        return json.loads(path.read_text())
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        die(f"step {step} stdout is not one machine-readable JSON document: {exc}")


def gate_semantics(step: str, value: Any) -> bool:
    """Validate the minimum machine-produced assertion for each pass gate."""
    if not isinstance(value, dict):
        return False
    receipt = value.get("receipt") if isinstance(value.get("receipt"), dict) else value
    observation = value.get("observation") if isinstance(value.get("observation"), dict) else {}
    checks = {
        "source-provenance": lambda: value.get("status") == "passed" and isinstance(value.get("source"), dict),
        "baseline-invariants": lambda: value.get("phase") == "before" and isinstance(value.get("protected"), dict),
        "post-invariants": lambda: value.get("phase") == "after" and isinstance(value.get("protected"), dict),
        "target-baseline": lambda: value.get("phase") == "before" and isinstance(value.get("state"), dict),
        "target-post-uninstall": lambda: value.get("phase") == "after" and isinstance(value.get("state"), dict),
        "ubuntu-preflight": lambda: value.get("os") == "ubuntu" and value.get("osVersion") == "26.04",
        "native-mac-preflight": lambda: value.get("status") == "passed" and value.get("host") in ("mac-mini-3", "mac-mini-3.local") and value.get("architecture") == "arm64",
        "service-identity": lambda: isinstance(value.get("service"), dict) and value["service"].get("accountUid") not in (None, "", "0", "absent") and value["service"].get("processUid") == value["service"].get("accountUid"),
        "no-input-sudo-observe": lambda: value.get("noInputRootObservation") == "allowed",
        "install": lambda: receipt.get("state") == "active" and receipt.get("currentStage") == "complete" and receipt.get("residues") == [],
        "idempotent-install": lambda: receipt.get("state") == "active" and receipt.get("currentStage") == "complete" and receipt.get("residues") == [],
        "repair": lambda: receipt.get("state") == "active" and receipt.get("currentStage") == "complete" and receipt.get("residues") == [],
        "reinstall": lambda: receipt.get("state") == "active" and receipt.get("currentStage") == "complete" and receipt.get("residues") == [],
        "expired-observe": lambda: value.get("schemaVersion") == "blazn.dev/node-root-helper/v1" and value.get("ok") is True and bool(observation),
        "expired-repair-denied": lambda: value.get("status") == "passed" and value.get("expiredRepairDenied") is True and isinstance(value.get("denial"), dict) and value["denial"].get("exitCode") == 1 and value["denial"].get("error", {}).get("code") == "node_failed",
        "expired-uninstall": lambda: receipt.get("state") == "removed" and receipt.get("currentStage") == "complete" and receipt.get("residues") == [],
        "install-crash-resume": lambda: value.get("status") == "passed" and value.get("crash", {}).get("lifecycle") == "install" and value.get("recovery", {}).get("state") == "active" and value.get("recovery", {}).get("residues") == [],
        "cleanup-crash-resume": lambda: value.get("status") == "passed" and value.get("crash", {}).get("lifecycle") == "cleanup" and value.get("recovery", {}).get("state") == "removed" and value.get("recovery", {}).get("residues") == [],
        "kubernetes-uid-rv": lambda: isinstance(value.get("node"), dict) and bool(value["node"].get("uid")) and bool(value["node"].get("resourceVersion")),
        "kubernetes-stale-cas-denied": lambda: value.get("status") == "passed" and value.get("staleCASDenied") is True and value.get("stateUnchanged") is True and value.get("rejection", {}).get("classification") in ("kubernetes-status-invalid-422-jsonpatch-test", "kubectl-invalid-jsonpatch-test") and value.get("rejection", {}).get("reason") == "Invalid",
        "kubernetes-quarantine-noschedule": lambda: value.get("status") == "passed" and value.get("quarantineNoSchedule") is True and value.get("ordinaryWorkloads") == 0,
        "zero-residue": lambda: value.get("status") == "passed" and value.get("guestResidue") == 0 and value.get("kubernetesResidue") == 0 and value.get("protectedInvariants") is True,
    }
    check = checks.get(step)
    return check is not None and bool(check())


def valid_timestamp(value: Any) -> bool:
    try:
        parsed = dt.datetime.fromisoformat(str(value).replace("Z", "+00:00"))
        return parsed.tzinfo is not None
    except (ValueError, TypeError):
        return False


def git(repo: pathlib.Path, *args: str) -> str:
    return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()


def atomic_json(path: pathlib.Path, value: Any, *, create: bool = False) -> None:
    encoded = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()
    target = path if create else path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp")
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, "wb", closefd=False) as stream:
            stream.write(encoded)
            stream.flush()
        os.fsync(fd)
    finally:
        os.close(fd)
    if not create:
        os.replace(target, path)
    directory_fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def load_run(root: pathlib.Path) -> tuple[pathlib.Path, dict[str, Any]]:
    path = root / "run.json"
    if path.is_symlink() or not path.is_file():
        die(f"run manifest is missing or a symlink: {path}")
    try:
        value = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        die(f"cannot load {path}: {exc}")
    return path, value


def require_env(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        die(f"{name} is required")
    return value


def init(args: argparse.Namespace) -> None:
    root = pathlib.Path(args.output).resolve()
    repo = pathlib.Path(args.repo).resolve()
    correlation = require_env("BLAZN_QUALIFICATION_CORRELATION_ID")
    target = require_env("BLAZN_QUALIFICATION_TARGET")
    profile = require_env("BLAZN_QUALIFICATION_PROFILE")
    if not CORRELATION.fullmatch(correlation):
        die("invalid correlation ID")
    if root.exists():
        die("evidence directory already exists")
    remote = git(repo, "remote", "get-url", "origin")
    if remote != CANONICAL_REMOTE:
        die(f"noncanonical source remote: {remote}")
    status = subprocess.check_output(
        ["git", "-C", str(repo), "status", "--porcelain=v1", "--untracked-files=all"]
    )
    binary_input = pathlib.Path(args.binary)
    binary = binary_input.resolve()
    if binary_input.is_symlink() or not binary.is_file() or not os.access(binary, os.X_OK):
        die("released binary must be an executable regular non-symlink file")
    binary_digest = digest_file(binary)
    version = subprocess.check_output([str(binary), "--output=json", "version"], text=True).strip()
    if not version:
        die("released binary returned empty version provenance")
    root.mkdir(mode=0o700, parents=True)
    (root / "artifacts").mkdir(mode=0o700)
    run = {
        "schemaVersion": 1,
        "correlationId": correlation,
        "scope": args.scope,
        "profile": profile,
        "target": target,
        "source": {
            "remote": remote,
            "head": git(repo, "rev-parse", "HEAD"),
            "tree": git(repo, "rev-parse", "HEAD^{tree}"),
            "statusDigest": digest_bytes(status),
            "binaryDigest": binary_digest,
            "binaryVersion": version,
        },
        "startedAt": now(),
        "status": "running",
        "steps": [],
    }
    atomic_json(root / "run.json", run, create=True)


def record(args: argparse.Namespace) -> None:
    root = pathlib.Path(args.output).resolve()
    path, run = load_run(root)
    correlation = require_env("BLAZN_QUALIFICATION_CORRELATION_ID")
    if run.get("status") != "running" or run.get("correlationId") != correlation:
        die("run is finalized or correlation differs")
    if not STEP.fullmatch(args.step):
        die("invalid step ID")
    if any(item.get("id") == args.step for item in run["steps"]):
        die("step IDs are immutable and unique")
    stdout = pathlib.Path(args.stdout).resolve()
    stderr = pathlib.Path(args.stderr).resolve()
    artifacts = (root / "artifacts").resolve()
    for candidate in (stdout, stderr):
        if not candidate.is_file() or artifacts not in candidate.parents:
            die("step artifacts must be regular files beneath evidence/artifacts")
        forbidden = artifact_has_forbidden_marker(candidate)
        if forbidden is not None:
            die(f"step artifact contains prohibited credential material marker: {forbidden.decode(errors='replace')}")
    status = "passed" if args.expect_exit == args.exit_code else "failed"
    rel_stdout = stdout.relative_to(root).as_posix()
    rel_stderr = stderr.relative_to(root).as_posix()
    metadata = json.loads(args.metadata)
    if not isinstance(metadata, dict):
        die("metadata must be a JSON object")
    semantic = artifact_json(stdout, args.step)
    if not gate_semantics(args.step, semantic):
        die(f"step {args.step} stdout does not satisfy its gate-specific semantic contract")
    lowered_metadata = json.dumps(metadata, sort_keys=True).lower()
    if any(marker in lowered_metadata for marker in (
        "accesstoken", "refreshtoken", "enrollmenttoken", "joincredential",
        "authorization", "privatekey", "credential",
    )):
        die("metadata contains a prohibited credential-related marker")
    run["steps"].append({
        "id": args.step,
        "status": status,
        "observedAt": now(),
        "exitCode": args.exit_code,
        "stdout": {"path": rel_stdout, "digest": digest_file(stdout), "bytes": stdout.stat().st_size},
        "stderr": {"path": rel_stderr, "digest": digest_file(stderr), "bytes": stderr.stat().st_size},
        "metadata": metadata,
    })
    atomic_json(path, run)
    if status != "passed":
        die(f"step {args.step} observed exit {args.exit_code}, expected {args.expect_exit}")


def validate(root: pathlib.Path, run: dict[str, Any], require_complete: bool) -> list[str]:
    errors: list[str] = []
    allowed_run = {"schemaVersion", "correlationId", "scope", "profile", "target", "source", "startedAt", "completedAt", "status", "steps"}
    if not isinstance(run, dict) or set(run) - allowed_run:
        return ["run is not an object with only allowed fields"]
    if run.get("schemaVersion") != 1:
        errors.append("schemaVersion is not 1")
    if not CORRELATION.fullmatch(str(run.get("correlationId", ""))):
        errors.append("correlationId is invalid")
    source = run.get("source", {})
    if not isinstance(source, dict) or set(source) - {"remote", "head", "tree", "statusDigest", "binaryDigest", "binaryVersion"}:
        errors.append("source has invalid fields")
        source = {}
    if source.get("remote") != CANONICAL_REMOTE:
        errors.append("source remote is not canonical")
    if not SHA.fullmatch(str(source.get("head", ""))) or not SHA.fullmatch(str(source.get("tree", ""))):
        errors.append("source HEAD/tree is invalid")
    if not DIGEST.fullmatch(str(source.get("statusDigest", ""))):
        errors.append("source status digest is invalid")
    if "binaryDigest" in source and not DIGEST.fullmatch(str(source["binaryDigest"])):
        errors.append("binary digest is invalid")
    if "binaryVersion" in source and (not isinstance(source["binaryVersion"], str) or not source["binaryVersion"]):
        errors.append("binary version is invalid")
    scope = run.get("scope")
    profile = run.get("profile")
    target = run.get("target")
    if scope == "fresh-linux" and (profile != "lxd-ubuntu-26.04" or not isinstance(target, str) or not target.startswith("blazn-q-")):
        errors.append("fresh-linux target/profile binding is invalid")
    elif scope == "native-mac" and (profile != "native-mac" or not isinstance(target, str) or not re.fullmatch(r"mac-mini-3(?:\..+)?", target)):
        errors.append("native-mac target/profile binding is invalid")
    elif scope not in ("fresh-linux", "native-mac"):
        errors.append("scope is invalid")
    if run.get("status") not in ("running", "passed", "failed"):
        errors.append("run status is invalid")
    for time_field in ("startedAt", "completedAt"):
        if time_field in run and not valid_timestamp(run[time_field]):
            errors.append(f"{time_field} is not a timezone-qualified RFC 3339 timestamp")
    steps = run.get("steps")
    if not isinstance(steps, list):
        return errors + ["steps is not an array"]
    ids: list[str] = []
    for item in steps:
        if not isinstance(item, dict) or set(item) != {"id", "status", "observedAt", "exitCode", "stdout", "stderr", "metadata"}:
            errors.append("step has invalid fields")
            continue
        sid = item.get("id", "")
        ids.append(sid)
        if not STEP.fullmatch(str(sid)):
            errors.append(f"step ID is invalid: {sid}")
        if item.get("status") != "passed":
            errors.append(f"step {sid} is not passed")
        if not isinstance(item.get("exitCode"), int) or not 0 <= item["exitCode"] <= 255:
            errors.append(f"step {sid} exit code is invalid")
        if not isinstance(item.get("metadata"), dict):
            errors.append(f"step {sid} metadata is not an object")
        elif any(marker in json.dumps(item["metadata"], sort_keys=True).lower() for marker in (
            "accesstoken", "refreshtoken", "enrollmenttoken", "joincredential",
            "authorization", "privatekey", "credential",
        )):
            errors.append(f"step {sid} metadata contains a prohibited credential marker")
        if not valid_timestamp(item.get("observedAt")):
            errors.append(f"step {sid} observedAt is invalid")
        for stream in ("stdout", "stderr"):
            artifact = item.get(stream, {})
            if not isinstance(artifact, dict) or set(artifact) != {"path", "digest", "bytes"}:
                errors.append(f"step {sid} {stream} descriptor is invalid")
                continue
            relative = artifact.get("path", "")
            if not isinstance(relative, str) or not relative.startswith("artifacts/") or ".." in pathlib.PurePosixPath(relative).parts:
                errors.append(f"step {sid} {stream} path is invalid")
                continue
            raw_candidate = root / relative
            candidate = raw_candidate.resolve()
            if raw_candidate.is_symlink() or root not in candidate.parents or not candidate.is_file():
                errors.append(f"step {sid} {stream} artifact is missing or escapes evidence root")
            elif not DIGEST.fullmatch(str(artifact.get("digest", ""))) or not isinstance(artifact.get("bytes"), int) or artifact["bytes"] < 0:
                errors.append(f"step {sid} {stream} digest/size descriptor is invalid")
            elif digest_file(candidate) != artifact.get("digest") or candidate.stat().st_size != artifact.get("bytes"):
                errors.append(f"step {sid} {stream} artifact digest/size differs")
            elif (forbidden := artifact_has_forbidden_marker(candidate)) is not None:
                errors.append(f"step {sid} {stream} contains prohibited credential marker {forbidden.decode(errors='replace')}")
        stdout_descriptor = item.get("stdout", {})
        relative_stdout = stdout_descriptor.get("path", "") if isinstance(stdout_descriptor, dict) else ""
        raw_stdout = root / relative_stdout
        if raw_stdout.is_file():
            try:
                semantic = json.loads(raw_stdout.read_text())
            except (OSError, UnicodeDecodeError, json.JSONDecodeError):
                errors.append(f"step {sid} stdout is not one machine-readable JSON document")
            else:
                if not gate_semantics(str(sid), semantic):
                    errors.append(f"step {sid} stdout fails its gate-specific semantic contract")
    if len(ids) != len(set(ids)):
        errors.append("duplicate step IDs")
    if require_complete:
        required = FRESH_GATES if run.get("scope") == "fresh-linux" else MAC_GATES if run.get("scope") == "native-mac" else set()
        errors.extend(f"required gate is missing: {gate}" for gate in sorted(required - set(ids)))
        if run.get("status") != "passed" or not run.get("completedAt"):
            errors.append("run is not finalized as passed")
        if "binaryDigest" not in source or "binaryVersion" not in source:
            errors.append("final evidence lacks exact released binary provenance")
        if source.get("statusDigest") != EMPTY_STATUS_DIGEST:
            errors.append("final evidence source worktree was not clean")
    return errors


def finalize(args: argparse.Namespace) -> None:
    root = pathlib.Path(args.output).resolve()
    path, run = load_run(root)
    errors = validate(root, run, False)
    required = FRESH_GATES if run.get("scope") == "fresh-linux" else MAC_GATES if run.get("scope") == "native-mac" else set()
    ids = {item.get("id") for item in run.get("steps", [])}
    errors.extend(f"required gate is missing: {gate}" for gate in sorted(required - ids))
    if errors:
        die("; ".join(errors))
    run["completedAt"] = now()
    run["status"] = "passed"
    atomic_json(path, run)
    verify(args)


def verify(args: argparse.Namespace) -> None:
    root = pathlib.Path(args.output).resolve()
    _, run = load_run(root)
    errors = validate(root, run, True)
    if errors:
        die("; ".join(errors))
    print(json.dumps({"status": "passed", "runDigest": digest_file(root / "run.json")}, sort_keys=True))


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser()
    commands = result.add_subparsers(dest="command", required=True)
    p = commands.add_parser("init")
    p.add_argument("--output", required=True)
    p.add_argument("--repo", required=True)
    p.add_argument("--scope", choices=("fresh-linux", "native-mac"), required=True)
    p.add_argument("--binary", required=True)
    p.set_defaults(func=init)
    p = commands.add_parser("record")
    p.add_argument("--output", required=True)
    p.add_argument("--step", required=True)
    p.add_argument("--stdout", required=True)
    p.add_argument("--stderr", required=True)
    p.add_argument("--exit-code", type=int, required=True)
    p.add_argument("--expect-exit", type=int, default=0)
    p.add_argument("--metadata", default="{}")
    p.set_defaults(func=record)
    for name, function in (("finalize", finalize), ("verify", verify)):
        p = commands.add_parser(name)
        p.add_argument("--output", required=True)
        p.set_defaults(func=function)
    return result


if __name__ == "__main__":
    parsed = parser().parse_args()
    parsed.func(parsed)
