#!/usr/bin/env python3
import hashlib, os, stat, sys, tempfile

def fail(message: str) -> None:
    raise SystemExit(f"blazn-worker-issuer-binary: {message}")

if len(sys.argv) != 5:
    fail("usage: SOURCE DESTINATION SHA256 TEST_MODE")
source, destination, expected, test_mode = sys.argv[1:]
if not os.path.isabs(source) or not os.path.isabs(destination) or len(expected) != 71 or not expected.startswith("sha256:"):
    fail("input binding is invalid")
flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
fd = os.open(source, flags)
try:
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        fail("source type or link count is unsafe")
    if test_mode != "1" and (info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o755):
        fail("source owner or mode is unsafe")
    parent = os.path.dirname(destination)
    tmp_fd, tmp = tempfile.mkstemp(prefix=".issuer-binary.", dir=parent)
    digest = hashlib.sha256()
    try:
        with os.fdopen(tmp_fd, "wb", closefd=True) as output:
            while True:
                chunk = os.read(fd, 1024 * 1024)
                if not chunk:
                    break
                digest.update(chunk)
                output.write(chunk)
            output.flush()
            os.fsync(output.fileno())
        if "sha256:" + digest.hexdigest() != expected:
            fail("opened source differs from reviewed digest")
        os.chown(tmp, 0, 0)
        os.chmod(tmp, 0o755)
        os.replace(tmp, destination)
        directory = os.open(parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
finally:
    os.close(fd)
