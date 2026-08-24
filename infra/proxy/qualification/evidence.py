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
REQUIRED_CLIENTS = {
    "hermes": "0.19.0",
    "codex": "0.147.0",
    "generic": "proxy-fixture/v1",
}
FORBIDDEN_KEYS = {
    "prompt", "prompts", "message", "messages", "tool", "tools", "toolpayload",
    "tool_payload", "token", "bearertoken", "cookie", "cookies", "privatekey",
    "private_key", "listenercredential", "listener_token", "credentialvalue",
}
FORBIDDEN_TEXT = (
    b"authorization: bearer ", b"proxy-authorization:", b"-----begin private key-----",
    b"-----begin rsa private key-----", b"-----begin ec private key-----",
    b'"prompt"', b'"messages"', b'"tool_payload"', b'"bearerToken"',
    b'"privateKey"', b'"listenerToken"', b'"credentialValue"',
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
            normalized = re.sub(r"[^a-z0-9_]", "", key.lower())
            if normalized in FORBIDDEN_KEYS:
                raise EvidenceError(f"{location} contains forbidden secret/content key {key}")
            assert_redacted(item, f"{location}.{key}")
    elif isinstance(value, list):
        for index, item in enumerate(value):
            assert_redacted(item, f"{location}[{index}]")
    elif isinstance(value, str):
        lowered = value.lower()
        if "bearer " in lowered or "-----begin " in lowered or "sk-proj-" in lowered:
            raise EvidenceError(f"{location} contains forbidden secret material")


def scan_file(path: pathlib.Path) -> None:
    data = path.read_bytes().lower()
    for marker in FORBIDDEN_TEXT:
        if marker.lower() in data:
            raise EvidenceError(f"artifact {path.name} contains forbidden marker")


def validate_identity(identity: Any) -> None:
    required = {"correlationId", "sourceHead", "sourceTree", "binaryDigest", "policyDigest", "hostId", "userId", "sessionId"}
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


def validate_receipt(receipt: Any, identity: dict[str, Any] | None = None) -> None:
    required = {"schemaVersion", "receiptId", "action", "observedAt", "identity", "profileDigest", "status", "result", "checksum"}
    if not isinstance(receipt, dict) or set(receipt) != required:
        raise EvidenceError("receipt has unknown or missing fields")
    if receipt["schemaVersion"] != SCHEMA_VERSION or not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f-]{27,35}", str(receipt["receiptId"])):
        raise EvidenceError("receipt identity is invalid")
    if receipt["action"] not in {"preflight", "capture-before", "cycle", "recovery", "route-proof", "cleanup"}:
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
    captures = [item for item in values if item["action"] == "capture-before" and item["status"] == "passed"]
    if len(captures) != 1:
        raise EvidenceError("exactly one passed capture-before receipt is required")
    baseline = captures[0]["result"]
    environment = baseline.get("environment", [])
    if [item.get("name") for item in environment] != list(EXACT_ENVIRONMENT):
        raise EvidenceError("baseline must snapshot exactly the five frozen environment variables in order")
    if any(set(item) != {"name", "present", "valueDigest"} or not DIGEST.fullmatch(str(item.get("valueDigest", ""))) for item in environment):
        raise EvidenceError("baseline environment entry is invalid")
    trees = baseline.get("configTrees")
    if not isinstance(trees, list) or {item.get("client") for item in trees} != {"codex", "claude", "hermes"}:
        raise EvidenceError("baseline must contain Codex, Claude, and Hermes config tree sentinels")
    if any(set(item) != {"client", "treeDigest"} or not DIGEST.fullmatch(str(item.get("treeDigest", ""))) for item in trees):
        raise EvidenceError("config tree sentinel is invalid")
    if baseline.get("directConnectivity", {}).get("reachable") is not True or not DIGEST.fullmatch(str(baseline.get("directConnectivity", {}).get("proofDigest", ""))):
        raise EvidenceError("authenticated direct-connectivity baseline is required")
    if set(baseline.get("owner", {})) != {"hostId", "userId", "sessionId", "stateDigest", "stateRoot"} or not DIGEST.fullmatch(str(baseline["owner"].get("stateDigest", ""))):
        raise EvidenceError("owner baseline is invalid")
    state_root = baseline["owner"].get("stateRoot")
    if not isinstance(state_root, str) or not pathlib.Path(state_root).is_absolute() or pathlib.Path(os.path.normpath(state_root)) != pathlib.Path(state_root) or state_root == "/":
        raise EvidenceError("owner baseline state root is invalid")

    def require_zero_residue(item: dict[str, Any], label: str) -> None:
        result = item["result"]
        observation = result.get("residueObservation")
        required = {
            "activationId", "sessionId", "stateRoot", "listenerObserved",
            "listenerResidue", "ownedStateObserved", "ownedStateResidue",
        }
        try:
            activation_id = str(uuid.UUID(str(observation.get("activationId"))))
        except (AttributeError, ValueError) as exc:
            raise EvidenceError(f"{label} residue observation activation is invalid") from exc
        if (
            not isinstance(observation, dict)
            or set(observation) != required
            or activation_id != observation["activationId"]
            or observation["sessionId"] != item["identity"]["sessionId"]
            or observation["stateRoot"] != state_root
            or observation["listenerObserved"] is not True
            or observation["ownedStateObserved"] is not True
            or observation["listenerResidue"] is not False
            or observation["ownedStateResidue"] is not False
            or result.get("listenerResidue") is not observation["listenerResidue"]
            or result.get("ownedStateResidue") is not observation["ownedStateResidue"]
        ):
            raise EvidenceError(f"{label} lacks exact activation/session/state-root zero-residue evidence")

    cycles = [item for item in values if item["action"] == "cycle" and item["status"] in {"passed", "recovery_required"}]
    numbers = [item["result"].get("cycle") for item in cycles]
    if sorted(numbers) != list(range(1, 21)) or len(set(numbers)) != 20:
        raise EvidenceError("the exact twenty-cycle matrix is incomplete")
    if any(item["result"].get("configTreesUnchanged") is not True or item["result"].get("exactFiveRestored") is not True or item["result"].get("directConnectivityRestored") is not True for item in cycles):
        raise EvidenceError("a cycle did not prove config/CAS/direct restoration")
    for item in cycles:
        require_zero_residue(item, "cycle")
    cases = {item["result"].get("case") for item in cycles}
    if not REQUIRED_RECOVERY.issubset(cases):
        raise EvidenceError("the required recovery/failure cycle cases are incomplete")
    ambiguous = [item for item in cycles if item["result"].get("case") == "ambiguous-recovery"]
    if len(ambiguous) != 1 or ambiguous[0]["status"] != "recovery_required" or ambiguous[0]["result"].get("userStateChanged") is not False:
        raise EvidenceError("ambiguous ownership must be RECOVERY_REQUIRED without user-state mutation")

    recovery = {item["result"].get("case") for item in values if item["action"] == "recovery" and item["status"] in {"passed", "recovery_required"}}
    if not REQUIRED_RECOVERY.issubset(recovery):
        raise EvidenceError("standalone recovery evidence is incomplete")

    proofs = [item for item in values if item["action"] == "route-proof"]
    for client, version in REQUIRED_CLIENTS.items():
        decisions = {item["result"].get("decision") for item in proofs if item["result"].get("client") == client and item["result"].get("version") == version and item["result"].get("authenticated") is True}
        if decisions != {"ROUTED", "DIRECT", "BYPASS"}:
            raise EvidenceError(f"authenticated ROUTED/DIRECT/BYPASS proof is incomplete for {client}")
    claude = [item for item in proofs if item["result"].get("client") == "claude" and item["result"].get("version") == "2.1.212"]
    if len(claude) != 1 or claude[0]["status"] != "unsupported" or claude[0]["result"].get("decision") != "UNSUPPORTED" or claude[0]["result"].get("protocol") != "anthropic-native":
        raise EvidenceError("Claude native protocol must be recorded honestly as unsupported")

    cleanup = [item for item in values if item["action"] == "cleanup" and item["status"] == "passed"]
    if len(cleanup) != 1:
        raise EvidenceError("exactly one passed cleanup receipt is required")
    result = cleanup[0]["result"]
    if result.get("directConnectivityRestored") is not True or result.get("configTreesUnchanged") is not True or result.get("listenerResidue") is not False or result.get("ownedStateResidue") is not False:
        raise EvidenceError("cleanup did not prove direct restoration and zero residue")
    require_zero_residue(cleanup[0], "cleanup")
    cas = result.get("compareAndSet")
    if not isinstance(cas, list) or [item.get("name") for item in cas] != list(EXACT_ENVIRONMENT) or any(item.get("outcome") not in {"restored", "unchanged"} for item in cas):
        raise EvidenceError("cleanup lacks exact-five compare-and-set restoration evidence")


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
            expected_lines.append(f"{digest_file(path).removeprefix('sha256:')}  {path.relative_to(root).as_posix()}")
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
    lines = "".join(f"{digest_file(path).removeprefix('sha256:')}  {path.relative_to(root).as_posix()}\n" for path in paths)
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
