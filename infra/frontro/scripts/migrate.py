#!/usr/bin/env python3
"""Apply control-api migrations to the hosted database from a built image.

Runs on the build host. The migration credential arrives as JSON on stdin
(the OpenBao secret apps/data/blazn/test/migration). The database URL is
mounted read-only from a private tmpfs directory into a locked-down,
short-lived container; failures are reported with the credential redacted.

    ... | migrate.py IMAGE POSTGRES_CA_FILE
"""
import json
import pathlib
import shutil
import subprocess
import sys
import tempfile

DATABASE_HOST = "frontro-db-1"
DATABASE_ADDRESS = "192.168.0.105"
DATABASE = "blazn_test"


def main():
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 64
    image, ca_file = sys.argv[1:]
    password = json.load(sys.stdin)["migration_password"]
    files = {
        "database_url": f"postgresql://blazn_migration:{password}@{DATABASE_HOST}:5432/{DATABASE}?sslmode=verify-full&sslrootcert=/run/migrate/ca.crt",
        "ca.crt": pathlib.Path(ca_file).read_text(),
    }
    directory = pathlib.Path(tempfile.mkdtemp(prefix="blazn-migrate-", dir="/dev/shm"))
    directory.chmod(0o700)
    try:
        for name, content in files.items():
            (directory / name).write_text(content)
            (directory / name).chmod(0o400)
        result = subprocess.run([
            "sudo", "-n", "docker", "run", "--rm", "--cpus=0.5", "--memory=512m", "--pids-limit=64",
            "--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
            "--add-host", f"{DATABASE_HOST}:{DATABASE_ADDRESS}",
            "--mount", f"type=bind,src={directory},dst=/run/migrate,readonly",
            "-e", "MIGRATION_DATABASE_URL_FILE=/run/migrate/database_url",
            image, "node", "dist/migrate.js"], capture_output=True, timeout=300)
    finally:
        shutil.rmtree(directory)
    if result.returncode:
        diagnostic = result.stderr.decode(errors="replace").replace(files["database_url"], "[redacted]").replace(password, "[redacted]")
        print(diagnostic[-2000:], file=sys.stderr)
        print("migrations failed", file=sys.stderr)
        return 1
    print("migrations applied")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
