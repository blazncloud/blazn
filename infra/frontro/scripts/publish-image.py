#!/usr/bin/env python3
"""Push a locally built control-api image to the in-cluster registry.

Runs on the build host. The registry credential arrives as JSON on stdin
(the OpenBao secret apps/data/blazn/test/registry) and lives only in a
private tmpfs directory for the duration of the push. Prints the pushed
image reference pinned by digest.

    ... | publish-image.py LOCAL_IMAGE TAG
"""
import base64
import json
import pathlib
import shutil
import subprocess
import sys
import tempfile

HOST = "registry.blazn-test.internal"
REPOSITORY = HOST + "/blazn/control-api"


def main():
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 64
    local, tag = sys.argv[1:]
    credential = json.load(sys.stdin)
    remote = f"{REPOSITORY}:{tag}"
    config = pathlib.Path(tempfile.mkdtemp(prefix="blazn-publish-", dir="/dev/shm"))
    config.chmod(0o700)
    try:
        auth = base64.b64encode(("publisher:" + credential["push_password"]).encode()).decode()
        (config / "config.json").write_text(json.dumps({"auths": {HOST: {"auth": auth}}}))
        (config / "config.json").chmod(0o600)
        subprocess.run(["sudo", "-n", "docker", "tag", local, remote], check=True, stdout=subprocess.DEVNULL)
        pushed = subprocess.run(["sudo", "-n", "docker", "--config", str(config), "push", remote], capture_output=True, timeout=600)
        if pushed.returncode:
            raise RuntimeError("image publish failed; diagnostics suppressed because they can echo the credential")
        digests = json.loads(subprocess.run(
            ["sudo", "-n", "docker", "image", "inspect", remote, "--format", "{{json .RepoDigests}}"],
            capture_output=True, check=True).stdout)
    finally:
        shutil.rmtree(config)
    pinned = [d for d in digests if d.startswith(REPOSITORY + "@sha256:")]
    if len(pinned) != 1:
        raise RuntimeError("pushed image has no unique registry digest")
    print(pinned[0])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
