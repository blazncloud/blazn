#!/usr/bin/env python3
"""Checksummed, redacted evidence store for serialized proxy qualification."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import tempfile
import uuid
from typing import Any

SCHEMA_VERSION = "proxy-qualification/v1"
DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA = re.compile(r"^[0-9a-f]{40}$")
CORRELATION = re.compile(r"^proxyqual-[a-z0-9][a-z0-9-]{7,55}$")
AUTHORITY = re.compile(r"^(?:[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?|\[[0-9a-f:]+\])(?::[1-9][0-9]{0,4})?$")
EXACT_ENVIRONMENT = (
    "OPENAI_BASE_URL",
    "OPENAI_API_KEY",
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
)
REQUIRED_RECOVERY = {
    "normal-stop", "abrupt-kill", "host-reboot", "journal-corruption",
    "manager-outage", "partial-publication", "ambiguous-recovery",
}
MATRIX = {
    1: "normal-stop", 2: "normal-stop", 3: "normal-stop", 4: "normal-stop",
    5: "abrupt-kill", 6: "abrupt-kill", 7: "host-reboot", 8: "host-reboot",
    9: "journal-corruption", 10: "journal-corruption", 11: "manager-outage",
    12: "partial-publication", 13: "ambiguous-recovery", 14: "repeated-on",
    15: "repeated-off", 16: "receipt-corruption", 17: "both-records-corrupt",
    18: "stale-pid-reuse", 19: "missing-ca", 20: "management-api-outage",
}
REQUIRED_CLIENTS = {
    "hermes": "0.19.0",
    "codex": "0.147.0",
    "generic": "proxy-fixture/v1",
}
FORBIDDEN_KEYS = {
    "prompt", "prompts", "message", "messages", "tool", "tools", "toolpayload",
    "token", "bearertoken", "accesstoken", "refreshtoken", "authorization",
    "proxyauthorization", "apikey", "cookie", "cookies", "setcookie", "privatekey",
    "clientsecret", "secret", "password", "credential", "credentialvalue",
    "listenercredential", "listenertoken", "requestbody", "responsebody", "inputtext",
    "outputtext", "key", "secretkey", "signingkey", "encryptionkey",
}
FORBIDDEN_TEXT = (
    b"authorization: bearer ", b"authorization: basic ", b"proxy-authorization:", b"set-cookie:",
    b"-----begin private key-----",
    b"-----begin rsa private key-----", b"-----begin ec private key-----",
    b'"prompt"', b'"messages"', b'"tool_payload"', b'"bearerToken"',
    b'"privateKey"', b'"listenerToken"', b'"credentialValue"', b'"apiKey"',
    b'"accessToken"', b'"refreshToken"', b'"clientSecret"',
)
FORBIDDEN_SECRET_TEXT = re.compile(
    rb"(?:sk-ant-api03-[a-z0-9_-]{8,}|sk-(?:proj|svcacct)-[a-z0-9_-]{8,}|sk-[a-z0-9]{20,}|gh[pousr]_[a-z0-9]{12,}|xox[baprs]-[a-z0-9-]{8,}|akia[0-9a-z]{12,}|eyj[a-z0-9_-]{7,}\.[a-z0-9_-]{8,}\.[a-z0-9_-]{8,})",
    re.IGNORECASE,
)


class EvidenceError(ValueError):
    pass


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def canonical(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def digest_bytes(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


def digest_file(path: pathlib.Path) -> str:
    return digest_bytes(path.read_bytes())


def canonical_uuid(value: Any, label: str) -> str:
    try:
        parsed = str(uuid.UUID(str(value)))
    except (ValueError, AttributeError) as exc:
        raise EvidenceError(f"{label} must be a canonical UUID") from exc
    if parsed != value:
        raise EvidenceError(f"{label} must be a canonical UUID")
    return parsed


def validate_direct_connectivity(value: Any, label: str) -> dict[str, Any]:
    required = {"reachable", "authenticated", "requestId", "authority", "proofDigest"}
    if not isinstance(value, dict) or set(value) != required:
        raise EvidenceError(f"{label} must contain exact authenticated request/authority evidence")
    if value["reachable"] is not True or value["authenticated"] is not True:
        raise EvidenceError(f"{label} must be reachable and authenticated")
    canonical_uuid(value["requestId"], f"{label} requestId")
    if not isinstance(value["authority"], str) or not AUTHORITY.fullmatch(value["authority"]):
        raise EvidenceError(f"{label} authority is invalid")
    if not DIGEST.fullmatch(str(value["proofDigest"])):
        raise EvidenceError(f"{label} proof digest is invalid")
    return value


def checksum(value: dict[str, Any]) -> str:
    return digest_bytes(canonical({key: item for key, item in value.items() if key != "checksum"}))


def atomic_json(path: pathlib.Path, value: Any) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".qualification-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def safe_root(raw: str, create: bool = False) -> pathlib.Path:
    path = pathlib.Path(raw).expanduser()
    if not path.is_absolute():
        raise EvidenceError("evidence output must be an absolute path")
    for parent in (path, *path.parents):
        if parent.exists() and parent.is_symlink():
            raise EvidenceError("evidence output cannot traverse a symlink")
        if parent == pathlib.Path("/"):
            break
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    if not path.is_dir() or path.is_symlink():
        raise EvidenceError("evidence output must be a real directory")
    return path


def assert_redacted(value: Any, location: str = "receipt") -> None:
    if isinstance(value, dict):
        for key, item in value.items():
            normalized = re.sub(r"[^a-z0-9]", "", key.lower())
            if normalized in FORBIDDEN_KEYS:
                raise EvidenceError(f"{location} contains forbidden secret/content key {key}")
            assert_redacted(item, f"{location}.{key}")
    elif isinstance(value, list):
        for index, item in enumerate(value):
            assert_redacted(item, f"{location}[{index}]")
    elif isinstance(value, str):
        lowered = value.lower()
        if (
            "bearer " in lowered
            or "basic " in lowered
            or "-----begin " in lowered
            or FORBIDDEN_SECRET_TEXT.search(value.encode(errors="ignore"))
        ):
            raise EvidenceError(f"{location} contains forbidden secret material")


def scan_file(path: pathlib.Path) -> None:
    data = path.read_bytes().lower()
    for marker in FORBIDDEN_TEXT:
        if marker.lower() in data:
            raise EvidenceError(f"artifact {path.name} contains forbidden marker")
    if FORBIDDEN_SECRET_TEXT.search(data):
        raise EvidenceError(f"artifact {path.name} contains forbidden secret material")


def validate_identity(identity: Any) -> None:
    required = {"correlationId", "sourceHead", "sourceTree", "binaryDigest", "policyDigest", "hostId", "userId", "sessionId", "locks"}
    if not isinstance(identity, dict) or set(identity) != required:
        raise EvidenceError("identity must contain the exact source/binary/policy/host/user/session fields")
    if not CORRELATION.fullmatch(str(identity["correlationId"])):
        raise EvidenceError("invalid proxy qualification correlation ID")
    if not SHA.fullmatch(str(identity["sourceHead"])) or not SHA.fullmatch(str(identity["sourceTree"])):
        raise EvidenceError("invalid source identity")
    if not DIGEST.fullmatch(str(identity["binaryDigest"])) or not DIGEST.fullmatch(str(identity["policyDigest"])):
        raise EvidenceError("invalid binary or policy digest")
    for name in ("hostId", "userId", "sessionId"):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}", str(identity[name])):
            raise EvidenceError(f"invalid {name}")
    locks = identity["locks"]
    if not isinstance(locks, dict) or set(locks) != {"coordinator", "session"}:
        raise EvidenceError("identity locks must bind coordinator and session")
    expected_key = hashlib.sha256(f"{identity['hostId']}\n{identity['userId']}".encode()).hexdigest()[:24]
    for name, lock in locks.items():
        required_lock = {"path", "device", "inode", "uid", "mode"}
        if not isinstance(lock, dict) or set(lock) != required_lock:
            raise EvidenceError(f"invalid {name} lock identity")
        path = pathlib.Path(str(lock["path"]))
        if "\x00" in str(lock["path"]) or not path.is_absolute() or pathlib.Path(os.path.normpath(str(lock["path"]))) != path or path == pathlib.Path("/"):
            raise EvidenceError(f"invalid {name} lock path")
        if any(isinstance(lock[field], bool) or not isinstance(lock[field], int) or lock[field] < 0 for field in ("device", "inode", "uid")):
            raise EvidenceError(f"invalid {name} lock inode identity")
        if lock["inode"] == 0 or lock["mode"] not in {"0600", "0640", "0644"}:
            raise EvidenceError(f"invalid {name} lock metadata")
        expected_name = f"proxy-{name}-{expected_key}.lock"
        if path.name != expected_name:
            raise EvidenceError(f"invalid {name} lock basename")
    if locks["coordinator"]["path"] == locks["session"]["path"] or (
        locks["coordinator"]["device"], locks["coordinator"]["inode"]
    ) == (locks["session"]["device"], locks["session"]["inode"]):
        raise EvidenceError("coordinator and session lock identities must be distinct")


def strict_result(value: Any, required: set[str], label: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != required:
        raise EvidenceError(f"{label} result has unknown or missing fields")
    return value


def validate_state_root(value: Any, label: str) -> str:
    if not isinstance(value, str):
        raise EvidenceError(f"{label} is invalid")
    path = pathlib.Path(value)
    if not path.is_absolute() or pathlib.Path(os.path.normpath(value)) != path or path == pathlib.Path("/") or "\x00" in value:
        raise EvidenceError(f"{label} is invalid")
    return value


def validate_residue_observation(result: dict[str, Any], identity: dict[str, Any], expected_root: str | None = None) -> None:
    observation = strict_result(
        result.get("residueObservation"),
        {
            "activationId", "sessionId", "stateRoot", "listenerObserved",
            "listenerResidue", "ownedStateObserved", "ownedStateResidue",
        },
        "residue observation",
    )
    canonical_uuid(observation["activationId"], "residue observation activationId")
    root = validate_state_root(observation["stateRoot"], "residue observation stateRoot")
    if (
        observation["sessionId"] != identity["sessionId"]
        or (expected_root is not None and root != expected_root)
        or observation["listenerObserved"] is not True
        or observation["ownedStateObserved"] is not True
        or observation["listenerResidue"] is not False
        or observation["ownedStateResidue"] is not False
        or result.get("listenerResidue") is not observation["listenerResidue"]
        or result.get("ownedStateResidue") is not observation["ownedStateResidue"]
    ):
        raise EvidenceError("receipt lacks exact activation/session/state-root zero-residue evidence")


def validate_compare_and_set(value: Any, ambiguous: bool, label: str) -> None:
    if not isinstance(value, list) or len(value) != len(EXACT_ENVIRONMENT):
        raise EvidenceError(f"{label} compare-and-set evidence is incomplete")
    if any(not isinstance(item, dict) or set(item) != {"name", "outcome"} for item in value):
        raise EvidenceError(f"{label} compare-and-set entry is invalid")
    if [item["name"] for item in value] != list(EXACT_ENVIRONMENT):
        raise EvidenceError(f"{label} compare-and-set names are not exact-five ordered")
    allowed = {"unchanged"} if ambiguous else {"restored", "unchanged"}
    if any(item["outcome"] not in allowed for item in value):
        raise EvidenceError(f"{label} compare-and-set conflicted or changed ambiguous owner state")


def validate_capture_result(result: Any, identity: dict[str, Any]) -> None:
    value = strict_result(result, {"environment", "configTrees", "directConnectivity", "owner"}, "capture-before")
    environment = value["environment"]
    if not isinstance(environment, list) or len(environment) != len(EXACT_ENVIRONMENT):
        raise EvidenceError("baseline environment is incomplete")
    if any(not isinstance(item, dict) or set(item) != {"name", "present", "valueDigest"} for item in environment):
        raise EvidenceError("baseline environment entry is invalid")
    if [item["name"] for item in environment] != list(EXACT_ENVIRONMENT) or any(not isinstance(item["present"], bool) or not DIGEST.fullmatch(str(item["valueDigest"])) for item in environment):
        raise EvidenceError("baseline must snapshot exactly the five frozen environment variables in order")
    trees = value["configTrees"]
    if not isinstance(trees, list) or len(trees) != 3 or any(not isinstance(item, dict) or set(item) != {"client", "treeDigest"} for item in trees):
        raise EvidenceError("baseline config tree sentinels are invalid")
    if [item["client"] for item in trees] != ["codex", "claude", "hermes"] or any(not DIGEST.fullmatch(str(item["treeDigest"])) for item in trees):
        raise EvidenceError("baseline must contain ordered Codex, Claude, and Hermes tree digests")
    validate_direct_connectivity(value["directConnectivity"], "baseline direct connectivity")
    owner = strict_result(value["owner"], {"hostId", "userId", "sessionId", "stateDigest", "stateRoot"}, "baseline owner")
    if any(owner[name] != identity[name] for name in ("hostId", "userId", "sessionId")) or not DIGEST.fullmatch(str(owner["stateDigest"])):
        raise EvidenceError("owner baseline differs from run identity")
    validate_state_root(owner["stateRoot"], "owner baseline stateRoot")


def validate_cycle_result(result: Any, identity: dict[str, Any], status: str) -> None:
    value = strict_result(
        result,
        {
            "cycle", "case", "configTreesUnchanged", "exactFiveRestored", "compareAndSet",
            "directConnectivityRestored", "directConnectivity", "listenerResidue",
            "ownedStateResidue", "residueObservation", "userStateChanged",
        },
        "cycle",
    )
    number = value["cycle"]
    if isinstance(number, bool) or number not in MATRIX or value["case"] != MATRIX[number]:
        raise EvidenceError("cycle number/case differs from the exact frozen matrix")
    ambiguous = value["case"] == "ambiguous-recovery"
    expected_status = "recovery_required" if ambiguous else "passed"
    if status != expected_status or value["userStateChanged"] is not (False if ambiguous else None):
        raise EvidenceError("cycle status or user-state result is invalid")
    if value["configTreesUnchanged"] is not True or value["exactFiveRestored"] is not True or value["directConnectivityRestored"] is not True:
        raise EvidenceError("cycle did not prove config/CAS/direct restoration")
    validate_compare_and_set(value["compareAndSet"], ambiguous, "cycle")
    validate_direct_connectivity(value["directConnectivity"], "cycle direct connectivity")
    validate_residue_observation(value, identity)


def validate_recovery_result(result: Any, identity: dict[str, Any], status: str) -> None:
    value = strict_result(
        result,
        {
            "case", "daemonIndependent", "compareAndSet", "directConnectivityRestored",
            "directConnectivity", "configTreesUnchanged", "exactFiveRestored", "listenerResidue",
            "ownedStateResidue", "residueObservation", "userStateChanged",
        },
        "recovery",
    )
    if value["case"] not in REQUIRED_RECOVERY:
        raise EvidenceError("recovery case is outside the frozen matrix")
    ambiguous = value["case"] == "ambiguous-recovery"
    if status != ("recovery_required" if ambiguous else "passed"):
        raise EvidenceError("recovery status is invalid")
    if any(value[name] is not True for name in ("daemonIndependent", "directConnectivityRestored", "configTreesUnchanged", "exactFiveRestored")) or value["userStateChanged"] is not False:
        raise EvidenceError("recovery did not prove daemon-independent direct restoration")
    validate_compare_and_set(value["compareAndSet"], ambiguous, "recovery")
    validate_direct_connectivity(value["directConnectivity"], "recovery direct connectivity")
    validate_residue_observation(value, identity)


def validate_route_result(result: Any, identity: dict[str, Any], status: str) -> None:
    value = strict_result(
        result,
        {"client", "version", "decision", "protocol", "authenticated", "requestId", "proofDigest", "policyDigest", "hostId", "userId", "sessionId", "reason"},
        "route-proof",
    )
    versions = {**REQUIRED_CLIENTS, "claude": "2.1.212"}
    if value["client"] not in versions or value["version"] != versions[value["client"]]:
        raise EvidenceError("route proof client/version is not frozen")
    if value["authenticated"] is not True:
        raise EvidenceError("route proof is not authenticated")
    canonical_uuid(value["requestId"], "route proof requestId")
    if not DIGEST.fullmatch(str(value["proofDigest"])) or value["policyDigest"] != identity["policyDigest"]:
        raise EvidenceError("route proof digest or policy binding is invalid")
    if any(value[name] != identity[name] for name in ("hostId", "userId", "sessionId")):
        raise EvidenceError("route proof owner/session differs from run identity")
    if value["client"] == "claude":
        if status != "unsupported" or value["decision"] != "UNSUPPORTED" or value["protocol"] != "anthropic-native" or value["reason"] != "native_protocol_unsupported":
            raise EvidenceError("Claude native protocol must be reported honestly as unsupported")
    elif (
        status != "passed"
        or value["decision"] not in {"ROUTED", "DIRECT", "BYPASS"}
        or value["protocol"] not in {"openai-chat", "openai-responses"}
        or not isinstance(value["reason"], str)
        or not re.fullmatch(r"[a-z][a-z0-9_]{1,63}", value["reason"])
    ):
        raise EvidenceError("route proof decision/protocol/reason is invalid")


def validate_cleanup_result(result: Any, identity: dict[str, Any], status: str) -> None:
    value = strict_result(
        result,
        {
            "compareAndSet", "directConnectivityRestored", "directConnectivity",
            "configTreesUnchanged", "exactFiveRestored", "listenerResidue",
            "ownedStateResidue", "residueObservation",
        },
        "cleanup",
    )
    if status != "passed" or any(value[name] is not True for name in ("directConnectivityRestored", "configTreesUnchanged", "exactFiveRestored")):
        raise EvidenceError("cleanup did not prove config/CAS/direct restoration")
    validate_compare_and_set(value["compareAndSet"], False, "cleanup")
    validate_direct_connectivity(value["directConnectivity"], "cleanup direct connectivity")
    validate_residue_observation(value, identity)


def validate_result(receipt: dict[str, Any]) -> None:
    validators = {
        "capture-before": validate_capture_result,
        "cycle": validate_cycle_result,
        "recovery": validate_recovery_result,
        "route-proof": validate_route_result,
        "cleanup": validate_cleanup_result,
    }
    action = receipt["action"]
    if action not in validators:
        raise EvidenceError("receipt action is invalid")
    validator = validators[action]
    if action == "capture-before":
        if receipt["status"] != "passed":
            raise EvidenceError("capture-before receipt must pass")
        validator(receipt["result"], receipt["identity"])
    else:
        validator(receipt["result"], receipt["identity"], receipt["status"])


def validate_receipt(receipt: Any, identity: dict[str, Any] | None = None) -> None:
    required = {"schemaVersion", "receiptId", "action", "observedAt", "identity", "profileDigest", "status", "result", "checksum"}
    if not isinstance(receipt, dict) or set(receipt) != required:
        raise EvidenceError("receipt has unknown or missing fields")
    if receipt["schemaVersion"] != SCHEMA_VERSION or not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f-]{27,35}", str(receipt["receiptId"])):
        raise EvidenceError("receipt identity is invalid")
    if receipt["action"] not in {"capture-before", "cycle", "recovery", "route-proof", "cleanup"}:
        raise EvidenceError("receipt action is invalid")
    if receipt["status"] not in {"passed", "failed", "recovery_required", "unsupported"}:
        raise EvidenceError("receipt status is invalid")
    if not DIGEST.fullmatch(str(receipt["profileDigest"])):
        raise EvidenceError("receipt profile digest is invalid")
    validate_identity(receipt["identity"])
    if identity is not None and receipt["identity"] != identity:
        raise EvidenceError("receipt identity differs from run identity")
    if receipt["checksum"] != checksum(receipt):
        raise EvidenceError("receipt checksum is invalid")
    validate_result(receipt)
    assert_redacted(receipt)


def init_run(output: str, identity: dict[str, Any], profile_digest: str, platform: str, mechanism: str) -> pathlib.Path:
    validate_identity(identity)
    if not DIGEST.fullmatch(profile_digest):
        raise EvidenceError("profile digest is invalid")
    if platform not in {"linux", "darwin"} or mechanism not in {"systemd_user_environment", "launchd_user_environment"}:
        raise EvidenceError("platform or publication mechanism is invalid")
    root = safe_root(output, create=True)
    manifest_path = root / "run.json"
    if manifest_path.exists():
        raise EvidenceError("evidence run already exists")
    (root / "receipts").mkdir(mode=0o700)
    manifest = {
        "schemaVersion": SCHEMA_VERSION,
        "identity": identity,
        "profileDigest": profile_digest,
        "platform": platform,
        "publicationMechanism": mechanism,
        "startedAt": now(),
        "completedAt": None,
        "status": "running",
        "receipts": [],
        "manifestDigest": None,
    }
    atomic_json(manifest_path, manifest)
    return root


def load_run(root: pathlib.Path) -> dict[str, Any]:
    try:
        value = json.loads((root / "run.json").read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise EvidenceError(f"run manifest is unreadable: {exc}") from exc
    required = {"schemaVersion", "identity", "profileDigest", "platform", "publicationMechanism", "startedAt", "completedAt", "status", "receipts", "manifestDigest"}
    if not isinstance(value, dict) or set(value) != required or value["schemaVersion"] != SCHEMA_VERSION:
        raise EvidenceError("run manifest has unknown or missing fields")
    validate_identity(value["identity"])
    if not DIGEST.fullmatch(str(value["profileDigest"])) or not isinstance(value["receipts"], list):
        raise EvidenceError("run manifest profile or receipts are invalid")
    return value


def record_receipt(output: str, receipt: dict[str, Any]) -> pathlib.Path:
    root = safe_root(output)
    manifest = load_run(root)
    if manifest["status"] != "running":
        raise EvidenceError("cannot append to finalized evidence")
    validate_receipt(receipt, manifest["identity"])
    if receipt["profileDigest"] != manifest["profileDigest"]:
        raise EvidenceError("receipt profile differs from run profile")
    name = f"{len(manifest['receipts']) + 1:03d}-{receipt['action']}-{receipt['receiptId']}.json"
    path = root / "receipts" / name
    atomic_json(path, receipt)
    descriptor = {"path": f"receipts/{name}", "digest": digest_file(path), "bytes": path.stat().st_size}
    manifest["receipts"].append(descriptor)
    atomic_json(root / "run.json", manifest)
    return path


def receipt_values(root: pathlib.Path, manifest: dict[str, Any]) -> list[dict[str, Any]]:
    values: list[dict[str, Any]] = []
    seen: set[str] = set()
    for descriptor in manifest["receipts"]:
        if not isinstance(descriptor, dict) or set(descriptor) != {"path", "digest", "bytes"}:
            raise EvidenceError("receipt descriptor is invalid")
        relative = str(descriptor["path"])
        if not re.fullmatch(r"receipts/[0-9]{3}-[a-z-]+-[0-9a-f-]+\.json", relative) or relative in seen:
            raise EvidenceError("receipt path is invalid or duplicated")
        seen.add(relative)
        path = root / relative
        if path.is_symlink() or not path.is_file() or path.stat().st_size != descriptor["bytes"] or digest_file(path) != descriptor["digest"]:
            raise EvidenceError("receipt artifact digest or size differs")
        scan_file(path)
        value = json.loads(path.read_text())
        validate_receipt(value, manifest["identity"])
        values.append(value)
    actual = {f"receipts/{path.name}" for path in (root / "receipts").glob("*.json")}
    if actual != seen:
        raise EvidenceError("unindexed receipt artifact exists")
    return values


def check_completion(values: list[dict[str, Any]]) -> None:
    captures = [item for item in values if item["action"] == "capture-before"]
    if len(captures) != 1 or captures[0]["status"] != "passed":
        raise EvidenceError("exactly one passed capture-before receipt is required")
    baseline = captures[0]["result"]
    state_root = baseline["owner"]["stateRoot"]
    authority = baseline["directConnectivity"]["authority"]

    cycles = [item for item in values if item["action"] == "cycle"]
    numbers = [item["result"]["cycle"] for item in cycles]
    if len(cycles) != len(MATRIX) or sorted(numbers) != list(MATRIX) or len(set(numbers)) != len(MATRIX):
        raise EvidenceError("the exact twenty-cycle matrix is incomplete")
    for item in cycles:
        if item["result"]["case"] != MATRIX[item["result"]["cycle"]]:
            raise EvidenceError("cycle number/case differs from the exact frozen matrix")
        if item["result"]["directConnectivity"]["authority"] != authority:
            raise EvidenceError("cycle direct-connectivity authority differs from baseline")
        validate_residue_observation(item["result"], item["identity"], state_root)

    recoveries = [item for item in values if item["action"] == "recovery"]
    recovery_cases = [item["result"]["case"] for item in recoveries]
    if len(recoveries) != len(REQUIRED_RECOVERY) or set(recovery_cases) != REQUIRED_RECOVERY or len(set(recovery_cases)) != len(recovery_cases):
        raise EvidenceError("standalone recovery evidence is incomplete")
    for item in recoveries:
        if item["result"]["directConnectivity"]["authority"] != authority:
            raise EvidenceError("recovery direct-connectivity authority differs from baseline")
        validate_residue_observation(item["result"], item["identity"], state_root)

    proofs = [item for item in values if item["action"] == "route-proof"]
    expected_proofs = {(client, version, decision) for client, version in REQUIRED_CLIENTS.items() for decision in ("ROUTED", "DIRECT", "BYPASS")}
    actual_proofs = {(item["result"]["client"], item["result"]["version"], item["result"]["decision"]) for item in proofs if item["result"]["client"] != "claude"}
    if actual_proofs != expected_proofs or len([item for item in proofs if item["result"]["client"] != "claude"]) != len(expected_proofs):
        raise EvidenceError("authenticated route-proof matrix is incomplete or duplicated")
    for client, version in REQUIRED_CLIENTS.items():
        decisions = {item["result"]["decision"] for item in proofs if item["result"]["client"] == client and item["result"]["version"] == version}
        if decisions != {"ROUTED", "DIRECT", "BYPASS"}:
            raise EvidenceError(f"authenticated ROUTED/DIRECT/BYPASS proof is incomplete for {client}")
    claude = [item for item in proofs if item["result"]["client"] == "claude"]
    if len(claude) != 1:
        raise EvidenceError("Claude native protocol must be recorded honestly as unsupported")

    cleanup = [item for item in values if item["action"] == "cleanup"]
    if len(cleanup) != 1 or cleanup[0]["status"] != "passed":
        raise EvidenceError("exactly one passed cleanup receipt is required")
    result = cleanup[0]["result"]
    if result["directConnectivity"]["authority"] != authority:
        raise EvidenceError("cleanup direct-connectivity authority differs from baseline")
    validate_residue_observation(result, cleanup[0]["identity"], state_root)

    if len(values) != 1 + len(MATRIX) + len(REQUIRED_RECOVERY) + len(expected_proofs) + 1 + 1:
        raise EvidenceError("evidence contains duplicate or unexpected receipts")


def verify_run(output: str, require_complete: bool = True) -> dict[str, Any]:
    root = safe_root(output)
    manifest = load_run(root)
    values = receipt_values(root, manifest)
    if require_complete:
        check_completion(values)
        if manifest["status"] != "passed" or manifest["completedAt"] is None:
            raise EvidenceError("evidence is not finalized as passed")
        expected = digest_bytes(canonical({key: value for key, value in manifest.items() if key != "manifestDigest"}))
        if manifest["manifestDigest"] != expected:
            raise EvidenceError("manifest digest is invalid")
        checksum_path = root / "SHA256SUMS"
        if checksum_path.is_symlink() or not checksum_path.is_file():
            raise EvidenceError("SHA256SUMS is missing")
        expected_lines = []
        for path in sorted([root / "run.json", *(root / "receipts").glob("*.json")], key=lambda item: item.relative_to(root).as_posix()):
            expected_lines.append(f"{digest_file(path)[len('sha256:'):]}  {path.relative_to(root).as_posix()}")
        if checksum_path.read_text().splitlines() != expected_lines:
            raise EvidenceError("SHA256SUMS differs from evidence artifacts")
    return {"schemaVersion": SCHEMA_VERSION, "status": "passed", "receiptCount": len(values), "manifestDigest": manifest.get("manifestDigest")}


def finalize_run(output: str) -> dict[str, Any]:
    root = safe_root(output)
    manifest = load_run(root)
    if manifest["status"] != "running":
        raise EvidenceError("run is already finalized")
    values = receipt_values(root, manifest)
    check_completion(values)
    manifest["status"] = "passed"
    manifest["completedAt"] = now()
    manifest["manifestDigest"] = digest_bytes(canonical({key: value for key, value in manifest.items() if key != "manifestDigest"}))
    atomic_json(root / "run.json", manifest)
    paths = sorted([root / "run.json", *(root / "receipts").glob("*.json")], key=lambda item: item.relative_to(root).as_posix())
    lines = "".join(f"{digest_file(path)[len('sha256:'):]}  {path.relative_to(root).as_posix()}\n" for path in paths)
    checksum_path = root / "SHA256SUMS"
    checksum_path.write_text(lines)
    os.chmod(checksum_path, 0o600)
    return verify_run(output)


def main() -> None:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    initialize = commands.add_parser("init")
    initialize.add_argument("--output", required=True)
    initialize.add_argument("--identity", required=True)
    initialize.add_argument("--profile-digest", required=True)
    initialize.add_argument("--platform", required=True)
    initialize.add_argument("--mechanism", required=True)
    record = commands.add_parser("record")
    record.add_argument("--output", required=True)
    record.add_argument("--receipt", required=True)
    for name in ("finalize", "verify"):
        command = commands.add_parser(name)
        command.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        if args.command == "init":
            identity = json.loads(pathlib.Path(args.identity).read_text())
            init_run(args.output, identity, args.profile_digest, args.platform, args.mechanism)
            result = {"status": "running"}
        elif args.command == "record":
            receipt = json.loads(pathlib.Path(args.receipt).read_text())
            result = {"path": str(record_receipt(args.output, receipt))}
        elif args.command == "finalize":
            result = finalize_run(args.output)
        else:
            result = verify_run(args.output)
        print(json.dumps(result, sort_keys=True, separators=(",", ":")))
    except (EvidenceError, OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"proxy qualification evidence: {exc}") from exc


if __name__ == "__main__":
    main()
