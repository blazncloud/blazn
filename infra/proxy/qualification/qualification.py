#!/usr/bin/env python3
"""Fail-closed coordinator for serialized per-user proxy qualification.

The checked-in profiles are planning inputs. Native mutation requires a separately
reviewed, immutable qualification profile and explicit action-bound approval.
This slice executes only the fake-state adapter used by static/unit tests.
"""

from __future__ import annotations

import argparse
import contextlib
import ctypes
import datetime as dt
import fcntl
import hashlib
import json
import os
import pathlib
import re
import socket
import stat
import subprocess
import sys
import uuid
from typing import Any, Iterator

import evidence

CANONICAL_REMOTE = "https://github.com/blazncloud/blazn.git"
SCHEMA_VERSION = evidence.SCHEMA_VERSION
DIGEST = evidence.DIGEST
CORRELATION = evidence.CORRELATION
EXACT_ENVIRONMENT = evidence.EXACT_ENVIRONMENT
CLIENT_PATHS = ("codex", "claude", "hermes")
MATRIX = evidence.MATRIX
MUTATING_ACTIONS = {"capture-before", "cycle", "recovery", "route-proof", "cleanup"}


class QualificationError(ValueError):
    pass


def canonical(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def digest(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


def digest_file(path: pathlib.Path) -> str:
    return digest(path.read_bytes())


def read_json(path: pathlib.Path, label: str) -> Any:
    if not path.is_absolute():
        raise QualificationError(f"{label} path must be absolute")
    if path.is_symlink() or not path.is_file():
        raise QualificationError(f"{label} must be a regular non-symlink file")
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise QualificationError(f"{label} is not valid JSON: {exc}") from exc


def strict_keys(value: Any, required: set[str], label: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != required:
        raise QualificationError(f"{label} must contain exactly {', '.join(sorted(required))}")
    return value


def safe_identifier(value: Any, label: str) -> str:
    text = str(value)
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}", text):
        raise QualificationError(f"{label} is invalid")
    return text


def clean_absolute_path(value: Any, label: str, within: pathlib.Path | None = None) -> pathlib.Path:
    text = str(value)
    path = pathlib.Path(text)
    if not path.is_absolute() or ".." in path.parts or "\x00" in text:
        raise QualificationError(f"{label} must be a normalized absolute path")
    normalized = pathlib.Path(os.path.normpath(text))
    if normalized != path or normalized == pathlib.Path("/"):
        raise QualificationError(f"{label} is not a safe normalized path")
    if within is not None:
        try:
            normalized.relative_to(within)
        except ValueError as exc:
            raise QualificationError(f"{label} must stay beneath the approved home") from exc
        if normalized == within:
            raise QualificationError(f"{label} cannot be the entire approved home")
    return normalized


def reject_existing_symlink_path(path: pathlib.Path, floor: pathlib.Path, label: str) -> None:
    current = path
    while True:
        if current.exists() and current.is_symlink():
            raise QualificationError(f"{label} cannot traverse an existing symlink")
        if current == floor:
            return
        if floor not in current.parents:
            raise QualificationError(f"{label} escaped its approved path floor")
        current = current.parent


def approval_time(value: str, label: str) -> dt.datetime:
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise QualificationError(f"nativeApproval.{label} must be an RFC 3339 timestamp") from exc
    if parsed.tzinfo is None:
        raise QualificationError(f"nativeApproval.{label} must include a UTC offset")
    return parsed.astimezone(dt.timezone.utc)


def lock_key(host_id: str, user_id: str) -> str:
    return hashlib.sha256(f"{host_id}\n{user_id}".encode()).hexdigest()[:24]


def validate_profile(value: Any) -> dict[str, Any]:
    required = {
        "schemaVersion", "profileId", "platform", "publicationMechanism", "supportStatus",
        "mutationEnabled", "source", "binary", "policy", "owner", "configTrees", "locks",
        "nativeApproval",
    }
    profile = strict_keys(value, required, "qualification profile")
    if profile["schemaVersion"] != SCHEMA_VERSION:
        raise QualificationError("qualification profile schemaVersion is unsupported")
    safe_identifier(profile["profileId"], "profileId")
    platform = profile["platform"]
    mechanism = profile["publicationMechanism"]
    if platform == "linux" and mechanism != "systemd_user_environment":
        raise QualificationError("Linux qualification requires systemd_user_environment")
    if platform == "darwin" and mechanism != "launchd_user_environment":
        raise QualificationError("macOS qualification requires launchd_user_environment")
    if platform not in {"linux", "darwin"}:
        raise QualificationError("qualification platform must be linux or darwin")
    if profile["supportStatus"] not in {"pending_native_qualification", "unsupported_until_launchd", "approved_candidate"}:
        raise QualificationError("supportStatus is invalid")
    if not isinstance(profile["mutationEnabled"], bool):
        raise QualificationError("mutationEnabled must be boolean")
    if platform == "darwin" and (profile["supportStatus"] != "unsupported_until_launchd" or profile["mutationEnabled"]):
        raise QualificationError("macOS must fail closed as unsupported until a launchd adapter is reviewed")

    source = strict_keys(profile["source"], {"remote", "head", "tree"}, "source")
    if source["remote"] != CANONICAL_REMOTE or not evidence.SHA.fullmatch(str(source["head"])) or not evidence.SHA.fullmatch(str(source["tree"])):
        raise QualificationError("source must bind the canonical remote and exact commit/tree")
    binary = strict_keys(profile["binary"], {"path", "digest"}, "binary")
    policy = strict_keys(profile["policy"], {"path", "digest"}, "policy")
    clean_absolute_path(binary["path"], "binary.path")
    clean_absolute_path(policy["path"], "policy.path")
    if not DIGEST.fullmatch(str(binary["digest"])) or not DIGEST.fullmatch(str(policy["digest"])):
        raise QualificationError("binary and policy must use exact SHA-256 digests")

    owner = strict_keys(profile["owner"], {"hostId", "userId", "sessionId", "home"}, "owner")
    for name in ("hostId", "userId", "sessionId"):
        safe_identifier(owner[name], f"owner.{name}")
    home = clean_absolute_path(owner["home"], "owner.home")
    if not isinstance(profile["configTrees"], list) or len(profile["configTrees"]) != 3:
        raise QualificationError("configTrees must contain exactly Codex, Claude, and Hermes sentinels")
    seen_clients: set[str] = set()
    seen_paths: set[pathlib.Path] = set()
    for index, item in enumerate(profile["configTrees"]):
        tree = strict_keys(item, {"client", "path"}, f"configTrees[{index}]")
        if tree["client"] not in CLIENT_PATHS or tree["client"] in seen_clients:
            raise QualificationError("configTrees clients must be exactly codex, claude, and hermes")
        path = clean_absolute_path(tree["path"], f"configTrees[{index}].path", home)
        reject_existing_symlink_path(path, home, f"configTrees[{index}].path")
        if path in seen_paths:
            raise QualificationError("configTrees paths must be distinct")
        seen_clients.add(tree["client"])
        seen_paths.add(path)
    if seen_clients != set(CLIENT_PATHS):
        raise QualificationError("configTrees clients must be exactly codex, claude, and hermes")

    locks = strict_keys(profile["locks"], {"coordinator", "session"}, "locks")
    coordinator = clean_absolute_path(locks["coordinator"], "locks.coordinator")
    session = clean_absolute_path(locks["session"], "locks.session")
    if coordinator == session:
        raise QualificationError("coordinator and session locks must be distinct")
    key = lock_key(owner["hostId"], owner["userId"])
    if coordinator.name != f"proxy-coordinator-{key}.lock" or session.name != f"proxy-session-{key}.lock":
        raise QualificationError("lock basenames are not bound to the exact host/user exclusivity key")

    approval = strict_keys(profile["nativeApproval"], {"ticket", "approvedAt", "expiresAt", "approvedBy", "scope", "sourceHead", "binaryDigest", "policyDigest", "hostId", "userId", "sessionId"}, "nativeApproval")
    if profile["mutationEnabled"]:
        if profile["supportStatus"] != "approved_candidate" or platform != "linux":
            raise QualificationError("mutation is permitted only for an approved Linux candidate")
        if not all(isinstance(approval[name], str) and approval[name] for name in approval):
            raise QualificationError("mutation requires a complete immutable nativeApproval")
        bindings = {
            "sourceHead": source["head"], "binaryDigest": binary["digest"], "policyDigest": policy["digest"],
            "hostId": owner["hostId"], "userId": owner["userId"], "sessionId": owner["sessionId"],
        }
        if any(approval[name] != wanted for name, wanted in bindings.items()):
            raise QualificationError("nativeApproval differs from the qualified source/artifact/owner identity")
        if approval["scope"] != "phase6a-native-proxy-qualification":
            raise QualificationError("nativeApproval scope is invalid")
        approved_at = approval_time(approval["approvedAt"], "approvedAt")
        expires_at = approval_time(approval["expiresAt"], "expiresAt")
        current = dt.datetime.now(dt.timezone.utc)
        if approved_at > current or expires_at <= current or expires_at <= approved_at or expires_at - approved_at > dt.timedelta(hours=24):
            raise QualificationError("nativeApproval must be current, ordered, and valid for no more than 24 hours")
        safe_identifier(approval["ticket"], "nativeApproval.ticket")
        safe_identifier(approval["approvedBy"], "nativeApproval.approvedBy")
    elif any(approval.values()):
        raise QualificationError("disabled profiles cannot carry partial or stale native approval")
    return profile


def load_profile(raw: str) -> tuple[pathlib.Path, dict[str, Any], str]:
    path = pathlib.Path(raw).expanduser()
    value = read_json(path, "qualification profile")
    profile = validate_profile(value)
    return path, profile, digest(canonical(profile))


def repo_root() -> pathlib.Path:
    return pathlib.Path(__file__).resolve().parents[3]


def git_output(*arguments: str) -> str:
    result = subprocess.run(["git", "-C", str(repo_root()), *arguments], check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    return result.stdout.strip()


def identity(profile: dict[str, Any], correlation: str, locks: dict[str, dict[str, Any]]) -> dict[str, Any]:
    return {
        "correlationId": correlation,
        "sourceHead": profile["source"]["head"],
        "sourceTree": profile["source"]["tree"],
        "binaryDigest": profile["binary"]["digest"],
        "policyDigest": profile["policy"]["digest"],
        "hostId": profile["owner"]["hostId"],
        "userId": profile["owner"]["userId"],
        "sessionId": profile["owner"]["sessionId"],
        "locks": locks,
    }


def validate_lock_info(info: os.stat_result, expected_uid: int | None = None) -> None:
    if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) not in {0o600, 0o640, 0o644}:
        raise QualificationError("approval-bound lock must be a regular non-symlink file with mode 0600, 0640, or 0644")
    if info.st_mode & (stat.S_IWGRP | stat.S_IWOTH):
        raise QualificationError("approval-bound lock cannot be writable by group or other")
    if expected_uid is not None and info.st_uid not in {0, expected_uid}:
        raise QualificationError("approval-bound lock has an unexpected owner")


def lock_identity_from_info(path: pathlib.Path, info: os.stat_result) -> dict[str, Any]:
    return {
        "path": str(path),
        "device": info.st_dev,
        "inode": info.st_ino,
        "uid": info.st_uid,
        "mode": f"{stat.S_IMODE(info.st_mode):04o}",
    }


def open_at(directory_fd: int, component: str, flags: int) -> int:
    if os.open in os.supports_dir_fd:
        return os.open(component, flags, dir_fd=directory_fd)
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        function = libc.openat
        function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_uint]
        function.restype = ctypes.c_int
    except (AttributeError, OSError) as exc:
        raise QualificationError("platform cannot enforce no-follow lock traversal") from exc
    descriptor = function(directory_fd, os.fsencode(component), flags, 0)
    if descriptor < 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), component)
    return descriptor


def open_lock_file(raw: str | pathlib.Path, expected_uid: int | None = None):
    path = clean_absolute_path(str(raw), "lock")
    required_flags = ("O_CLOEXEC", "O_NOFOLLOW", "O_DIRECTORY")
    if any(not hasattr(os, name) for name in required_flags):
        raise QualificationError("platform cannot enforce no-follow lock traversal")
    directory_flags = os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_DIRECTORY
    final_flags = os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW
    directory_fd = os.open("/", directory_flags)
    try:
        for component in path.parts[1:-1]:
            next_fd = open_at(directory_fd, component, directory_flags)
            os.close(directory_fd)
            directory_fd = next_fd
        descriptor = open_at(directory_fd, path.name, final_flags)
    except FileNotFoundError as exc:
        raise QualificationError("approval-bound lock must be pre-created") from exc
    except OSError as exc:
        raise QualificationError("approval-bound lock could not be opened without following links") from exc
    finally:
        os.close(directory_fd)
    try:
        info = os.fstat(descriptor)
        validate_lock_info(info, expected_uid)
        return os.fdopen(descriptor, "r+"), lock_identity_from_info(path, info)
    except Exception:
        os.close(descriptor)
        raise


def lock_identity(path: pathlib.Path) -> dict[str, Any]:
    stream, identity_value = open_lock_file(path)
    stream.close()
    return identity_value


@contextlib.contextmanager
def exclusive_locks(profile: dict[str, Any]) -> Iterator[dict[str, dict[str, Any]]]:
    expected_uid = os.getuid()
    streams = []
    identities = []
    try:
        for name in ("coordinator", "session"):
            stream, identity_value = open_lock_file(profile["locks"][name], expected_uid)
            try:
                fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                stream.close()
                raise QualificationError("host/user proxy qualification is already reserved") from exc
            streams.append(stream)
            identities.append(identity_value)
        yield {"coordinator": identities[0], "session": identities[1]}
    finally:
        for stream in reversed(streams):
            fcntl.flock(stream.fileno(), fcntl.LOCK_UN)
            stream.close()


def operation(args: argparse.Namespace, route_proof: dict[str, Any] | None = None) -> str:
    if args.action == "cycle":
        return f"cycle:{args.cycle:02d}:{args.case}"
    if args.action == "recovery":
        return f"recovery:{args.case}"
    if args.action == "route-proof":
        if route_proof is None:
            route_proof = read_json(pathlib.Path(args.proof), "route proof")
        return f"route-proof:{route_proof.get('client', '')}:{route_proof.get('decision', '')}:{digest(canonical(route_proof))[len('sha256:'):]}"
    return args.action


def approval_digest(action: str, correlation: str, profile_digest: str, profile: dict[str, Any], locks: dict[str, dict[str, Any]]) -> str:
    value = {
        "action": action,
        "correlationId": correlation,
        "profileDigest": profile_digest,
        "source": profile["source"],
        "binaryDigest": profile["binary"]["digest"],
        "policyDigest": profile["policy"]["digest"],
        "owner": profile["owner"],
        "locks": locks,
    }
    return digest(canonical(value))


def verify_artifacts(profile: dict[str, Any]) -> None:
    binary = pathlib.Path(profile["binary"]["path"])
    policy = pathlib.Path(profile["policy"]["path"])
    for path, wanted, label in ((binary, profile["binary"]["digest"], "binary"), (policy, profile["policy"]["digest"], "policy")):
        if path.is_symlink() or not path.is_file() or digest_file(path) != wanted:
            raise QualificationError(f"exact approved {label} digest is unavailable")


def verify_source(profile: dict[str, Any], allow_dirty_fake: bool) -> None:
    if git_output("remote", "get-url", "origin") != CANONICAL_REMOTE:
        raise QualificationError("source origin is not canonical")
    if git_output("rev-parse", "HEAD") != profile["source"]["head"] or git_output("rev-parse", "HEAD^{tree}") != profile["source"]["tree"]:
        raise QualificationError("checked-out source differs from the approved source identity")
    if not allow_dirty_fake and git_output("status", "--porcelain", "--untracked-files=all"):
        raise QualificationError("source is dirty; native qualification would not be reproducible")


def plan_result(action: str, profile: dict[str, Any], profile_digest: str) -> dict[str, Any]:
    correlation = os.environ.get("BLAZN_PROXY_QUALIFICATION_CORRELATION_ID", "proxyqual-REQUIRED")
    return {
        "schemaVersion": SCHEMA_VERSION,
        "status": "planned",
        "action": action,
        "profileId": profile["profileId"],
        "profileDigest": profile_digest,
        "platform": profile["platform"],
        "supportStatus": profile["supportStatus"],
        "mutationEnabled": profile["mutationEnabled"],
        "correlationId": correlation,
        "owner": {name: profile["owner"][name] for name in ("hostId", "userId", "sessionId")},
        "locks": profile["locks"],
        "requirements": [
            "approved immutable qualification profile",
            "exact canonical source HEAD/tree and clean worktree",
            "exact binary and policy SHA-256 digests",
            "pre-created coordinator and host/user session locks",
            "BLAZN_PROXY_QUALIFICATION_MODE=mutate",
            "action-bound BLAZN_PROXY_QUALIFICATION_APPROVAL",
            "exclusive sacrificial host/user session with direct-connectivity baseline",
        ],
        "wouldExecute": False,
    }


def validate_fake_state(value: Any, profile: dict[str, Any]) -> dict[str, Any]:
    state_value = strict_keys(value, {"owner", "environment", "configTrees", "directConnectivity", "proxy"}, "fake adapter state")
    owner = strict_keys(state_value["owner"], {"hostId", "userId", "sessionId", "stateDigest"}, "fake owner")
    for name in ("hostId", "userId", "sessionId"):
        if owner[name] != profile["owner"][name]:
            raise QualificationError("fake adapter owner differs from qualification profile")
    if not DIGEST.fullmatch(str(owner["stateDigest"])):
        raise QualificationError("fake owner state digest is invalid")
    environment = state_value["environment"]
    if not isinstance(environment, dict) or set(environment) != set(EXACT_ENVIRONMENT) or any(value is not None and not isinstance(value, str) for value in environment.values()):
        raise QualificationError("fake adapter must expose exactly five environment values")
    trees = state_value["configTrees"]
    if not isinstance(trees, dict) or set(trees) != set(CLIENT_PATHS) or any(not DIGEST.fullmatch(str(value)) for value in trees.values()):
        raise QualificationError("fake adapter config tree sentinels are invalid")
    direct = state_value["directConnectivity"]
    try:
        evidence.validate_direct_connectivity(direct, "fake direct connectivity")
    except evidence.EvidenceError as exc:
        raise QualificationError(str(exc)) from exc
    proxy = strict_keys(
        state_value["proxy"],
        {"activationId", "sessionId", "stateRoot", "active", "listenerObservation", "ownedStateObservation", "cas"},
        "fake proxy state",
    )
    try:
        activation_id = str(uuid.UUID(str(proxy["activationId"])))
    except (ValueError, AttributeError) as exc:
        raise QualificationError("fake proxy activationId must be a canonical UUID") from exc
    if activation_id != proxy["activationId"]:
        raise QualificationError("fake proxy activationId must be a canonical UUID")
    if proxy["sessionId"] != profile["owner"]["sessionId"]:
        raise QualificationError("fake proxy session differs from qualification profile")
    state_root = clean_absolute_path(proxy["stateRoot"], "fake proxy stateRoot", pathlib.Path(profile["owner"]["home"]))
    platform_root = (
        pathlib.Path(profile["owner"]["home"]) / ".local" / "share" / "blazn" / "proxy"
        if profile["platform"] == "linux"
        else pathlib.Path(profile["owner"]["home"]) / "Library" / "Application Support" / "Blazn" / "proxy"
    )
    if state_root != platform_root:
        raise QualificationError("fake proxy stateRoot differs from the exact account state root")
    if not isinstance(proxy["active"], bool):
        raise QualificationError("fake proxy active flag must be boolean")
    binding = {"activationId": proxy["activationId"], "sessionId": proxy["sessionId"], "stateRoot": proxy["stateRoot"]}
    for name in ("listenerObservation", "ownedStateObservation"):
        observation = strict_keys(
            proxy[name],
            {"activationId", "sessionId", "stateRoot", "available", "residue"},
            f"fake proxy {name}",
        )
        if any(observation[field] != expected for field, expected in binding.items()):
            raise QualificationError(f"fake proxy {name} is not bound to the exact activation/session/state root")
        if not isinstance(observation["available"], bool) or not isinstance(observation["residue"], bool):
            raise QualificationError(f"fake proxy {name} availability and residue must be boolean")
    if not isinstance(proxy["cas"], dict) or set(proxy["cas"]) != set(EXACT_ENVIRONMENT) or any(value not in {"restored", "unchanged", "conflict"} for value in proxy["cas"].values()):
        raise QualificationError("fake compare-and-set results are invalid")
    return state_value


def load_fake_state(args: argparse.Namespace, profile: dict[str, Any]) -> dict[str, Any]:
    if args.adapter != "fake" or os.environ.get("BLAZN_PROXY_QUALIFICATION_TESTING") != "1":
        if profile["platform"] == "darwin":
            raise QualificationError("macOS native qualification is unsupported until the launchd adapter is reviewed")
        raise QualificationError("native Linux systemd execution is intentionally unwired in this static harness slice")
    if not args.adapter_state:
        raise QualificationError("fake adapter requires --adapter-state")
    return validate_fake_state(read_json(pathlib.Path(args.adapter_state), "fake adapter state"), profile)


def baseline_result(state_value: dict[str, Any]) -> dict[str, Any]:
    return {
        "environment": [
            {"name": name, "present": state_value["environment"][name] is not None, "valueDigest": digest((state_value["environment"][name] or "").encode())}
            for name in EXACT_ENVIRONMENT
        ],
        "configTrees": [{"client": client, "treeDigest": state_value["configTrees"][client]} for client in CLIENT_PATHS],
        "directConnectivity": state_value["directConnectivity"],
        "owner": {**state_value["owner"], "stateRoot": state_value["proxy"]["stateRoot"]},
    }


def zero_residue_result(state_value: dict[str, Any]) -> dict[str, Any]:
    proxy = state_value["proxy"]
    listener = proxy["listenerObservation"]
    owned_state = proxy["ownedStateObservation"]
    if not listener["available"] or not owned_state["available"]:
        raise QualificationError("proxy residue observation became unavailable")
    if proxy["active"] or listener["residue"] or owned_state["residue"]:
        raise QualificationError("proxy post-state contains an active listener or Blazn-owned residue")
    return {
        "activationId": proxy["activationId"],
        "sessionId": proxy["sessionId"],
        "stateRoot": proxy["stateRoot"],
        "listenerObserved": listener["available"],
        "listenerResidue": listener["residue"],
        "ownedStateObserved": owned_state["available"],
        "ownedStateResidue": owned_state["residue"],
    }


def load_baseline(output: str) -> dict[str, Any]:
    root = evidence.safe_root(output)
    manifest = evidence.load_run(root)
    values = evidence.receipt_values(root, manifest)
    baselines = [item["result"] for item in values if item["action"] == "capture-before" and item["status"] == "passed"]
    if len(baselines) != 1:
        raise QualificationError("one capture-before receipt is required")
    return baselines[0]


def unchanged(state_value: dict[str, Any], baseline: dict[str, Any]) -> tuple[bool, bool]:
    trees = {item["client"]: item["treeDigest"] for item in baseline["configTrees"]}
    tree_ok = trees == state_value["configTrees"]
    environment = {item["name"]: item for item in baseline["environment"]}
    env_ok = all(
        environment[name]["present"] == (state_value["environment"][name] is not None)
        and environment[name]["valueDigest"] == digest((state_value["environment"][name] or "").encode())
        for name in EXACT_ENVIRONMENT
    )
    return tree_ok, env_ok


def direct_restoration(state_value: dict[str, Any], baseline: dict[str, Any]) -> dict[str, Any]:
    current = state_value["directConnectivity"]
    evidence.validate_direct_connectivity(current, "direct connectivity")
    expected = baseline["directConnectivity"]
    if current["authority"] != expected["authority"]:
        raise QualificationError("direct-connectivity authority differs from the captured baseline")
    return dict(current)


def compare_and_set_result(state_value: dict[str, Any], ambiguous: bool) -> list[dict[str, str]]:
    result = [{"name": name, "outcome": state_value["proxy"]["cas"][name]} for name in EXACT_ENVIRONMENT]
    allowed = {"unchanged"} if ambiguous else {"restored", "unchanged"}
    if any(item["outcome"] not in allowed for item in result):
        raise QualificationError("compare-and-set restoration conflicted or changed ambiguous owner state")
    return result


def validate_route_proof(value: Any, profile: dict[str, Any]) -> tuple[str, dict[str, Any]]:
    required = {"client", "version", "decision", "protocol", "authenticated", "requestId", "proofDigest", "policyDigest", "hostId", "userId", "sessionId", "reason"}
    proof = strict_keys(value, required, "route proof")
    versions = {"hermes": "0.19.0", "codex": "0.147.0", "generic": "proxy-fixture/v1", "claude": "2.1.212"}
    if proof["client"] not in versions or proof["version"] != versions[proof["client"]]:
        raise QualificationError("route proof client/version is not the frozen qualification input")
    if proof["authenticated"] is not True or not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f-]{27,35}", str(proof["requestId"])) or not DIGEST.fullmatch(str(proof["proofDigest"])):
        raise QualificationError("route proof lacks authenticated request identity")
    if proof["policyDigest"] != profile["policy"]["digest"]:
        raise QualificationError("route proof policy digest differs")
    for name in ("hostId", "userId", "sessionId"):
        if proof[name] != profile["owner"][name]:
            raise QualificationError("route proof owner/session differs")
    if proof["client"] == "claude":
        if proof["decision"] != "UNSUPPORTED" or proof["protocol"] != "anthropic-native" or proof["reason"] != "native_protocol_unsupported":
            raise QualificationError("Claude native protocol must be reported as unsupported")
        return "unsupported", proof
    if proof["decision"] not in {"ROUTED", "DIRECT", "BYPASS"} or proof["protocol"] not in {"openai-chat", "openai-responses"}:
        raise QualificationError("route decision or protocol is invalid")
    if not isinstance(proof["reason"], str) or not re.fullmatch(r"[a-z][a-z0-9_]{1,63}", proof["reason"]):
        raise QualificationError("route proof reason is invalid")
    return "passed", proof


def make_receipt(action: str, status_value: str, identity_value: dict[str, str], profile_digest: str, result: dict[str, Any]) -> dict[str, Any]:
    receipt = {
        "schemaVersion": SCHEMA_VERSION,
        "receiptId": str(uuid.uuid4()),
        "action": action,
        "observedAt": evidence.now(),
        "identity": identity_value,
        "profileDigest": profile_digest,
        "status": status_value,
        "result": result,
        "checksum": "",
    }
    receipt["checksum"] = evidence.checksum(receipt)
    evidence.validate_receipt(receipt, identity_value)
    return receipt


def execute(args: argparse.Namespace, profile: dict[str, Any], profile_digest: str, correlation: str,
            locks: dict[str, dict[str, Any]], state_value: dict[str, Any], route_proof: dict[str, Any] | None = None) -> dict[str, Any]:
    identity_value = identity(profile, correlation, locks)
    if args.action == "capture-before":
        root = pathlib.Path(args.evidence)
        if not (root / "run.json").exists():
            evidence.init_run(args.evidence, identity_value, profile_digest, profile["platform"], profile["publicationMechanism"])
        receipt = make_receipt("capture-before", "passed", identity_value, profile_digest, baseline_result(state_value))
    elif args.action == "cycle":
        if args.cycle not in MATRIX or args.case != MATRIX[args.cycle]:
            raise QualificationError("cycle number/case must match the frozen twenty-cycle matrix")
        baseline = load_baseline(args.evidence)
        trees_ok, env_ok = unchanged(state_value, baseline)
        if not trees_ok or not env_ok:
            raise QualificationError("cycle post-state differs from the direct baseline")
        direct = direct_restoration(state_value, baseline)
        residue = zero_residue_result(state_value)
        ambiguous = args.case == "ambiguous-recovery"
        cas = compare_and_set_result(state_value, ambiguous)
        result = {
            "cycle": args.cycle,
            "case": args.case,
            "configTreesUnchanged": trees_ok,
            "exactFiveRestored": env_ok,
            "compareAndSet": cas,
            "directConnectivityRestored": direct["reachable"],
            "directConnectivity": direct,
            "listenerResidue": residue["listenerResidue"],
            "ownedStateResidue": residue["ownedStateResidue"],
            "residueObservation": residue,
            "userStateChanged": False if ambiguous else None,
        }
        receipt = make_receipt("cycle", "recovery_required" if ambiguous else "passed", identity_value, profile_digest, result)
    elif args.action == "recovery":
        if args.case not in evidence.REQUIRED_RECOVERY:
            raise QualificationError("recovery case is outside the required matrix")
        baseline = load_baseline(args.evidence)
        trees_ok, env_ok = unchanged(state_value, baseline)
        ambiguous = args.case == "ambiguous-recovery"
        if not trees_ok or not env_ok:
            raise QualificationError("recovery post-state differs from the direct baseline")
        direct = direct_restoration(state_value, baseline)
        cas = compare_and_set_result(state_value, ambiguous)
        residue = zero_residue_result(state_value)
        result = {
            "case": args.case,
            "daemonIndependent": True,
            "compareAndSet": cas,
            "directConnectivityRestored": direct["reachable"],
            "directConnectivity": direct,
            "configTreesUnchanged": trees_ok,
            "exactFiveRestored": env_ok,
            "listenerResidue": residue["listenerResidue"],
            "ownedStateResidue": residue["ownedStateResidue"],
            "residueObservation": residue,
            "userStateChanged": False,
        }
        receipt = make_receipt("recovery", "recovery_required" if ambiguous else "passed", identity_value, profile_digest, result)
    elif args.action == "route-proof":
        if route_proof is None:
            raise QualificationError("route proof was not frozen before approval")
        status_value, result = validate_route_proof(route_proof, profile)
        receipt = make_receipt("route-proof", status_value, identity_value, profile_digest, result)
    elif args.action == "cleanup":
        baseline = load_baseline(args.evidence)
        trees_ok, env_ok = unchanged(state_value, baseline)
        cas = compare_and_set_result(state_value, False)
        direct = direct_restoration(state_value, baseline)
        residue = zero_residue_result(state_value)
        if not trees_ok or not env_ok:
            raise QualificationError("cleanup found config drift, CAS conflict, lost direct connectivity, or Blazn residue")
        result = {
            "compareAndSet": cas,
            "directConnectivityRestored": direct["reachable"],
            "directConnectivity": direct,
            "configTreesUnchanged": trees_ok,
            "exactFiveRestored": env_ok,
            "listenerResidue": residue["listenerResidue"],
            "ownedStateResidue": residue["ownedStateResidue"],
            "residueObservation": residue,
        }
        receipt = make_receipt("cleanup", "passed", identity_value, profile_digest, result)
    else:
        raise QualificationError("action cannot execute through the adapter")
    evidence.record_receipt(args.evidence, receipt)
    return receipt


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser()
    result.add_argument("action", choices=("preflight", "plan", "capture-before", "cycle", "recovery", "route-proof", "verify", "cleanup"))
    result.add_argument("--profile", required=True)
    result.add_argument("--evidence")
    result.add_argument("--adapter", choices=("linux-systemd", "macos-launchd", "fake"))
    result.add_argument("--adapter-state")
    result.add_argument("--cycle", type=int)
    result.add_argument("--case", choices=tuple(sorted(set(MATRIX.values()) | evidence.REQUIRED_RECOVERY)))
    result.add_argument("--proof")
    result.add_argument("--finalize", action="store_true")
    return result


def main() -> None:
    args = parser().parse_args()
    try:
        _, profile, profile_digest = load_profile(args.profile)
        if args.action == "preflight":
            output = plan_result("preflight", profile, profile_digest)
            output["checks"] = {
                "profileValid": True,
                "platformFailClosed": profile["platform"] == "darwin" or not profile["mutationEnabled"],
                "exactEnvironment": list(EXACT_ENVIRONMENT),
                "twentyCycleMatrix": [{"cycle": number, "case": case} for number, case in MATRIX.items()],
            }
            print(json.dumps(output, sort_keys=True, separators=(",", ":")))
            return
        if args.action == "plan":
            print(json.dumps(plan_result("plan", profile, profile_digest), sort_keys=True, separators=(",", ":")))
            return
        if args.action == "verify":
            if not args.evidence:
                raise QualificationError("verify requires --evidence")
            result = evidence.finalize_run(args.evidence) if args.finalize else evidence.verify_run(args.evidence)
            print(json.dumps(result, sort_keys=True, separators=(",", ":")))
            return
        if not args.evidence:
            raise QualificationError(f"{args.action} requires --evidence")
        if args.action == "cycle" and (args.cycle is None or args.case is None):
            raise QualificationError("cycle requires --cycle and --case")
        if args.action == "recovery" and args.case is None:
            raise QualificationError("recovery requires --case")
        if args.action == "route-proof" and args.proof is None:
            raise QualificationError("route-proof requires --proof")
        if os.environ.get("BLAZN_PROXY_QUALIFICATION_MODE", "plan") != "mutate":
            print(json.dumps(plan_result(operation(args), profile, profile_digest), sort_keys=True, separators=(",", ":")))
            return
        if not profile["mutationEnabled"]:
            raise QualificationError("qualification profile does not authorize mutation")
        correlation = os.environ.get("BLAZN_PROXY_QUALIFICATION_CORRELATION_ID", "")
        if not CORRELATION.fullmatch(correlation):
            raise QualificationError("BLAZN_PROXY_QUALIFICATION_CORRELATION_ID must match proxyqual-[a-z0-9][a-z0-9-]{7,55}")
        adapter = args.adapter or ("linux-systemd" if profile["platform"] == "linux" else "macos-launchd")
        if profile["platform"] == "linux" and adapter not in {"linux-systemd", "fake"}:
            raise QualificationError("adapter does not match the Linux systemd profile")
        if profile["platform"] == "darwin" or adapter == "macos-launchd":
            raise QualificationError("macOS native qualification is unsupported until the launchd adapter is reviewed")
        fake = adapter == "fake" and os.environ.get("BLAZN_PROXY_QUALIFICATION_TESTING") == "1"
        verify_source(profile, allow_dirty_fake=fake)
        verify_artifacts(profile)
        args.adapter = adapter
        route_proof = read_json(pathlib.Path(args.proof), "route proof") if args.action == "route-proof" else None
        action_value = operation(args, route_proof)
        with exclusive_locks(profile) as locks:
            approval_input = approval_digest(action_value, correlation, profile_digest, profile, locks)
            expected = f"APPROVE:{correlation}:{profile['owner']['hostId']}:{profile['owner']['userId']}:{profile['owner']['sessionId']}:{action_value}:{approval_input}"
            if os.environ.get("BLAZN_PROXY_QUALIFICATION_APPROVAL") != expected:
                raise QualificationError(f"approval must equal {expected}")
            state_value = load_fake_state(args, profile)
            receipt = execute(args, profile, profile_digest, correlation, locks, state_value, route_proof)
        print(json.dumps(receipt, sort_keys=True, separators=(",", ":")))
    except (QualificationError, evidence.EvidenceError, OSError, subprocess.SubprocessError, json.JSONDecodeError) as exc:
        raise SystemExit(f"proxy qualification: {exc}") from exc


if __name__ == "__main__":
    main()
