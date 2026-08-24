#!/usr/bin/env python3
"""Static/fake-adapter tests. These tests never call D-Bus, proxy, CA, or OS APIs."""

from __future__ import annotations

import copy
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
import uuid

QUALIFICATION = pathlib.Path(__file__).resolve().parents[1]
REPO = QUALIFICATION.parents[2]


def load_module(name: str, path: pathlib.Path):
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot import {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


sys.path.insert(0, str(QUALIFICATION))
evidence = load_module("proxy_qualification_evidence", QUALIFICATION / "evidence.py")
sys.modules["evidence"] = evidence
qualification = load_module("proxy_qualification", QUALIFICATION / "qualification.py")


def sha(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


class StaticQualificationTest(unittest.TestCase):
    maxDiff = None

    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="blazn-proxy-qualification-")
        self.root = pathlib.Path(self.temporary.name)
        self.home = self.root / "home"
        self.home.mkdir()
        for name in (".codex", ".claude", ".hermes"):
            (self.home / name).mkdir()
        self.binary = self.root / "blazn"
        self.binary.write_bytes(b"static fake binary\n")
        self.binary.chmod(0o700)
        self.policy = self.root / "policy.json"
        self.policy.write_text('{"fixture":"policy"}\n')
        self.host = "fake-linux-01"
        self.user = "uid-1000"
        self.session = "session-01"
        key = qualification.lock_key(self.host, self.user)
        self.coordinator_lock = self.root / f"proxy-coordinator-{key}.lock"
        self.session_lock = self.root / f"proxy-session-{key}.lock"
        for path in (self.coordinator_lock, self.session_lock):
            path.touch(mode=0o600)
            path.chmod(0o600)
        head = subprocess.check_output(["git", "-C", str(REPO), "rev-parse", "HEAD"], text=True).strip()
        tree = subprocess.check_output(["git", "-C", str(REPO), "rev-parse", "HEAD^{tree}"], text=True).strip()
        current = dt.datetime.now(dt.timezone.utc)
        approved_at = (current - dt.timedelta(minutes=1)).isoformat().replace("+00:00", "Z")
        expires_at = (current + dt.timedelta(hours=1)).isoformat().replace("+00:00", "Z")
        native = {
            "ticket": "test-only", "approvedAt": approved_at, "expiresAt": expires_at,
            "approvedBy": "static-test", "scope": "phase6a-native-proxy-qualification", "sourceHead": head,
            "binaryDigest": qualification.digest_file(self.binary), "policyDigest": qualification.digest_file(self.policy),
            "hostId": self.host, "userId": self.user, "sessionId": self.session,
        }
        self.profile = {
            "schemaVersion": "proxy-qualification/v1", "profileId": "static-fake-linux", "platform": "linux",
            "publicationMechanism": "systemd_user_environment", "supportStatus": "approved_candidate", "mutationEnabled": True,
            "source": {"remote": qualification.CANONICAL_REMOTE, "head": head, "tree": tree},
            "binary": {"path": str(self.binary), "digest": qualification.digest_file(self.binary)},
            "policy": {"path": str(self.policy), "digest": qualification.digest_file(self.policy)},
            "owner": {"hostId": self.host, "userId": self.user, "sessionId": self.session, "home": str(self.home)},
            "configTrees": [
                {"client": "codex", "path": str(self.home / ".codex")},
                {"client": "claude", "path": str(self.home / ".claude")},
                {"client": "hermes", "path": str(self.home / ".hermes")},
            ],
            "locks": {"coordinator": str(self.coordinator_lock), "session": str(self.session_lock)},
            "nativeApproval": native,
        }
        self.profile_path = self.root / "profile.json"
        self.write_json(self.profile_path, self.profile)
        self.state = {
            "owner": {"hostId": self.host, "userId": self.user, "sessionId": self.session, "stateDigest": sha(b"owner-state")},
            "environment": {
                "OPENAI_BASE_URL": None, "OPENAI_API_KEY": "prior-openai",
                "ANTHROPIC_BASE_URL": "https://direct.invalid", "ANTHROPIC_API_KEY": None,
                "ANTHROPIC_AUTH_TOKEN": "prior-anthropic",
            },
            "configTrees": {"codex": sha(b"codex-tree"), "claude": sha(b"claude-tree"), "hermes": sha(b"hermes-tree")},
            "directConnectivity": {"reachable": True, "proofDigest": sha(b"authenticated-direct-proof")},
            "proxy": {
                "active": False, "listenerResidue": False, "ownedStateResidue": False,
                "cas": {name: "restored" for name in qualification.EXACT_ENVIRONMENT},
            },
        }
        self.state_path = self.root / "state.json"
        self.write_json(self.state_path, self.state)
        self.output = self.root / "evidence"
        self.correlation = "proxyqual-static001"

    def tearDown(self) -> None:
        self.temporary.cleanup()

    @staticmethod
    def write_json(path: pathlib.Path, value) -> None:
        path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n")

    def command(self, action: str, *extra: str, approve: bool = True, profile_path: pathlib.Path | None = None, state_path: pathlib.Path | None = None) -> subprocess.CompletedProcess[str]:
        profile_path = profile_path or self.profile_path
        state_path = state_path or self.state_path
        arguments = [sys.executable, str(QUALIFICATION / "qualification.py"), action, "--profile", str(profile_path)]
        if action not in {"preflight", "plan"}:
            arguments.extend(["--evidence", str(self.output)])
        if action not in {"preflight", "plan", "verify"}:
            arguments.extend(["--adapter", "fake", "--adapter-state", str(state_path)])
        arguments.extend(extra)
        environment = os.environ.copy()
        if approve:
            profile = qualification.validate_profile(json.loads(profile_path.read_text()))
            profile_digest = qualification.digest(qualification.canonical(profile))
            namespace = type("Args", (), {"action": action, "cycle": None, "case": None, "proof": None})()
            iterator = iter(extra)
            for item in iterator:
                if item == "--cycle":
                    namespace.cycle = int(next(iterator))
                elif item == "--case":
                    namespace.case = next(iterator)
                elif item == "--proof":
                    namespace.proof = next(iterator)
            action_value = qualification.operation(namespace)
            locks = {
                "coordinator": qualification.lock_identity(self.coordinator_lock),
                "session": qualification.lock_identity(self.session_lock),
            }
            approval_input = qualification.approval_digest(action_value, self.correlation, profile_digest, profile, locks)
            environment.update({
                "BLAZN_PROXY_QUALIFICATION_TESTING": "1",
                "BLAZN_PROXY_QUALIFICATION_MODE": "mutate",
                "BLAZN_PROXY_QUALIFICATION_CORRELATION_ID": self.correlation,
                "BLAZN_PROXY_QUALIFICATION_APPROVAL": f"APPROVE:{self.correlation}:{self.host}:{self.user}:{self.session}:{action_value}:{approval_input}",
            })
        return subprocess.run(arguments, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment)

    def assert_failed(self, result: subprocess.CompletedProcess[str], marker: str) -> None:
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(marker, result.stderr)

    def test_templates_and_schemas_are_strict_and_macos_is_fail_closed(self) -> None:
        for schema in ("profile.schema.json", "receipt.schema.json", "run.schema.json"):
            parsed = json.loads((QUALIFICATION / "schemas" / schema).read_text())
            self.assertFalse(parsed["additionalProperties"])
            self.assertEqual(parsed["$schema"], "https://json-schema.org/draft/2020-12/schema")
        linux = qualification.validate_profile(json.loads((QUALIFICATION / "profiles/linux-systemd-user.template.json").read_text()))
        mac = qualification.validate_profile(json.loads((QUALIFICATION / "profiles/macos-launchd-unsupported.template.json").read_text()))
        self.assertFalse(linux["mutationEnabled"])
        self.assertEqual(mac["supportStatus"], "unsupported_until_launchd")
        changed = copy.deepcopy(mac)
        changed["mutationEnabled"] = True
        changed["supportStatus"] = "approved_candidate"
        with self.assertRaisesRegex(qualification.QualificationError, "macOS"):
            qualification.validate_profile(changed)

    def test_default_is_plan_and_does_not_create_evidence(self) -> None:
        result = self.command("capture-before", approve=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "planned")
        self.assertFalse(self.output.exists())

    def test_malicious_correlation_profile_paths_and_locks_are_rejected(self) -> None:
        environment = os.environ.copy()
        environment.update({"BLAZN_PROXY_QUALIFICATION_TESTING": "1", "BLAZN_PROXY_QUALIFICATION_MODE": "mutate", "BLAZN_PROXY_QUALIFICATION_CORRELATION_ID": "../bad"})
        result = subprocess.run([
            sys.executable, str(QUALIFICATION / "qualification.py"), "capture-before", "--profile", str(self.profile_path),
            "--evidence", str(self.output), "--adapter", "fake", "--adapter-state", str(self.state_path),
        ], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment)
        self.assert_failed(result, "CORRELATION_ID")

        escaped = copy.deepcopy(self.profile)
        escaped["configTrees"][0]["path"] = "/etc"
        escaped_path = self.root / "escaped.json"
        self.write_json(escaped_path, escaped)
        result = self.command("plan", approve=False, profile_path=escaped_path)
        self.assert_failed(result, "approved home")

        symlink = self.root / "proxy-session-symlink.lock"
        symlink.symlink_to(self.session_lock)
        bad_lock = copy.deepcopy(self.profile)
        bad_lock["locks"]["session"] = str(symlink)
        bad_lock_path = self.root / "bad-lock.json"
        self.write_json(bad_lock_path, bad_lock)
        result = self.command("plan", approve=False, profile_path=bad_lock_path)
        self.assert_failed(result, "basenames")

        stream = self.session_lock.open("r+")
        try:
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.command("capture-before")
            self.assert_failed(result, "already reserved")
        finally:
            fcntl.flock(stream.fileno(), fcntl.LOCK_UN)
            stream.close()

    def test_config_mutation_residue_and_redaction_are_rejected(self) -> None:
        self.assertEqual(self.command("capture-before").returncode, 0)
        changed = copy.deepcopy(self.state)
        changed["configTrees"]["codex"] = sha(b"mutated")
        changed_path = self.root / "changed-state.json"
        self.write_json(changed_path, changed)
        result = self.command("cycle", "--cycle", "1", "--case", "normal-stop", state_path=changed_path)
        self.assert_failed(result, "differs from the direct baseline")

        residue = copy.deepcopy(self.state)
        residue["proxy"]["listenerResidue"] = True
        residue_path = self.root / "residue-state.json"
        self.write_json(residue_path, residue)
        result = self.command("cleanup", state_path=residue_path)
        self.assert_failed(result, "residue")

        profile = qualification.validate_profile(self.profile)
        with self.assertRaisesRegex(evidence.EvidenceError, "forbidden"):
            qualification.make_receipt(
                "recovery", "passed", qualification.identity(profile, self.correlation),
                qualification.digest(qualification.canonical(profile)), {"prompt": "must never appear"},
            )

    def proof(self, client: str, version: str, decision: str) -> pathlib.Path:
        protocol = "anthropic-native" if client == "claude" else ("openai-responses" if client == "codex" else "openai-chat")
        reason = "native_protocol_unsupported" if client == "claude" else f"verified_{decision.lower()}"
        value = {
            "client": client, "version": version, "decision": decision, "protocol": protocol, "authenticated": True,
            "requestId": str(uuid.uuid4()), "proofDigest": sha(f"{client}:{decision}".encode()),
            "policyDigest": self.profile["policy"]["digest"], "hostId": self.host, "userId": self.user,
            "sessionId": self.session, "reason": reason,
        }
        path = self.root / f"proof-{client}-{decision}.json"
        self.write_json(path, value)
        return path

    def test_complete_fake_matrix_finalizes_and_tamper_fails(self) -> None:
        result = self.command("capture-before")
        self.assertEqual(result.returncode, 0, result.stderr)
        for number, case in qualification.MATRIX.items():
            result = self.command("cycle", "--cycle", str(number), "--case", case)
            self.assertEqual(result.returncode, 0, result.stderr)
        for case in sorted(evidence.REQUIRED_RECOVERY):
            result = self.command("recovery", "--case", case)
            self.assertEqual(result.returncode, 0, result.stderr)
        versions = {"hermes": "0.19.0", "codex": "0.147.0", "generic": "proxy-fixture/v1"}
        for client, version in versions.items():
            for decision in ("ROUTED", "DIRECT", "BYPASS"):
                proof = self.proof(client, version, decision)
                result = self.command("route-proof", "--proof", str(proof))
                self.assertEqual(result.returncode, 0, result.stderr)
        claude = self.proof("claude", "2.1.212", "UNSUPPORTED")
        result = self.command("route-proof", "--proof", str(claude))
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.command("cleanup")
        self.assertEqual(result.returncode, 0, result.stderr)

        result = self.command("verify", "--finalize", approve=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        verified = json.loads(result.stdout)
        self.assertEqual(verified["receiptCount"], 39)
        self.assertTrue((self.output / "SHA256SUMS").is_file())

        receipt = next((self.output / "receipts").glob("*cycle*.json"))
        value = json.loads(receipt.read_text())
        value["result"]["configTreesUnchanged"] = False
        value["checksum"] = evidence.checksum(value)
        self.write_json(receipt, value)
        manifest = json.loads((self.output / "run.json").read_text())
        descriptor = next(item for item in manifest["receipts"] if item["path"] == f"receipts/{receipt.name}")
        descriptor["digest"] = evidence.digest_file(receipt)
        descriptor["bytes"] = receipt.stat().st_size
        manifest["manifestDigest"] = evidence.digest_bytes(evidence.canonical({key: item for key, item in manifest.items() if key != "manifestDigest"}))
        self.write_json(self.output / "run.json", manifest)
        result = self.command("verify", approve=False)
        self.assert_failed(result, "did not prove")


if __name__ == "__main__":
    unittest.main(verbosity=2)
