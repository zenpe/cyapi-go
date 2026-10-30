"""Run as root on disposable Linux/systemd CI; tests real service credentials."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import urllib.request


def run(*args, ok=True, env=None, cwd=None):
    result = subprocess.run([str(a) for a in args], text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=120, env=env, cwd=cwd)
    print(result.stdout, flush=True)
    if ok and result.returncode:
        raise AssertionError(f"Command failed: {args[0]} ({result.returncode})")
    if not ok and result.returncode == 0:
        raise AssertionError(f"Command unexpectedly succeeded: {args[0]}")
    return result


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def account_env():
    return {**os.environ, "CYAPI_INSTALL_PASSWORD": "InstallSmoke!123"}


def config(root, port):
    (root / "config.yaml").write_text(
        f'server:\n  listen: "127.0.0.1:{port}"\nmaintenance:\n  enabled: false\n')


def unit(root):
    return "cyapi-" + hashlib.sha256(str(root).encode()).hexdigest()[:12] + ".service"


def verify(root, port):
    binary = root / "cyapi"
    run(binary, "status", "--config", root / "config.yaml")
    run(binary, "doctor", "--config", root / "config.yaml")
    request = urllib.request.Request(f"http://127.0.0.1:{port}/auth/login",
        data=json.dumps({"account": "smoke_user", "password": "InstallSmoke!123"}).encode(),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=10) as response:
        body = json.load(response)
        assert body["status"] == 200, body
    uid = pwd.getpwnam("cyapi").pw_uid
    for name in ["config.yaml", "secrets.env", "data/cyapi.db", "logs/server.log"]:
        path = root / name
        assert path.stat().st_uid == uid, (name, "wrong owner")
        assert path.stat().st_mode & 0o077 == 0, (name, "excess permissions")
    assert (root / ".cyapi-install.json").stat().st_uid == 0
    assert (root / "cyapi").stat().st_uid == 0
    run("runuser", "-u", "cyapi", "--", "test", "!", "-w", root / ".cyapi-install.json")
    run("runuser", "-u", "cyapi", "--", "test", "!", "-w", root)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--legacy", type=Path, required=True)
    args = parser.parse_args()
    roots = []
    try:
        # Check that an occupied port is caught before accounts/directories are created.
        blocked = Path(tempfile.mkdtemp(prefix="cyapi-port-")); roots.append(blocked)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0)); sock.listen()
            result = run(args.binary, "install", "--dir", blocked, "--listen",
                         f"127.0.0.1:{sock.getsockname()[1]}", ok=False)
            assert "cannot listen" in result.stdout
            assert not (blocked / "data").exists()

        # Startup failures report the real cause, stop the service, and can resume.
        failed = Path(tempfile.mkdtemp(prefix="cyapi-failure-")); roots.append(failed)
        failed.chmod(0o755)
        failed_port = free_port(); config(failed, failed_port)
        with (failed / "config.yaml").open("a") as handle:
            handle.write("mail:\n  provider: unsupported\n")
        result = run(args.binary, "install", "--dir", failed, "--non-interactive",
                     "--username", "smoke_user", "--email", "alias@example.com", env=account_env(), ok=False)
        assert "unsupported mail provider" in result.stdout
        run("systemctl", "is-active", "--quiet", unit(failed), ok=False)
        key = (failed / "secrets.env").read_bytes()
        config(failed, failed_port)
        run(failed / "cyapi", "install", "--dir", failed, "--non-interactive")
        assert (failed / "secrets.env").read_bytes() == key
        verify(failed, failed_port)

        root = Path(tempfile.mkdtemp(prefix="cyapi-managed-")); roots.append(root)
        root.chmod(0o755)
        # Owner-denying inherited POSIX ACL reproduces the NAS permission trap.
        run("setfacl", "-m", "d:u::---,d:g::---,d:o::---", root)
        port = free_port(); config(root, port)
        install = [args.binary, "install", "--dir", root, "--non-interactive",
                   "--username", "smoke_user", "--email", "alias@example.com"]
        run(*install, env=account_env())
        verify(root, port)
        before = (root / "secrets.env").read_bytes()
        run(root / "cyapi", "install", "--dir", root)
        assert (root / "secrets.env").read_bytes() == before

        # Editing stages/validates the file and restores service ownership.
        editor = root / "test-editor"
        new_port = free_port()
        editor.write_text(f'#!/bin/sh\nsed -i "s/{port}/{new_port}/" "$1"\n')
        editor.chmod(0o755)
        run(root / "cyapi", "config", "edit", "--config", root / "config.yaml",
            env={**os.environ, "EDITOR": str(editor)})
        run("systemctl", "restart", unit(root))
        verify(root, new_port)
        valid = (root / "config.yaml").read_bytes()
        editor.write_text('#!/bin/sh\nprintf "server: [invalid]\\n" > "$1"\n')
        run(root / "cyapi", "config", "edit", "--config", root / "config.yaml",
            env={**os.environ, "EDITOR": str(editor)}, ok=False)
        assert (root / "config.yaml").read_bytes() == valid

        # Sudo business operations must create files as cyapi, including backups.
        backup = root / "backups/manual-smoke"
        run(root / "cyapi", "backup", "--config", root / "config.yaml", "--output", backup, "--include-config")
        assert (backup / "manifest.json").stat().st_uid == pwd.getpwnam("cyapi").pw_uid
        run(root / "cyapi", "upgrade", "--config", root / "config.yaml", "--file", args.binary)
        verify(root, new_port)

        # Actual v0.0.2 installation, then new-binary adoption with account/key/port preserved.
        legacy = Path(tempfile.mkdtemp(prefix="cyapi-legacy-")); roots.append(legacy)
        legacy.chmod(0o755)
        legacy_port = free_port(); config(legacy, legacy_port)
        run(args.legacy, "install", "--dir", legacy, "--non-interactive",
            "--username", "smoke_user", "--email", "123456@qq.com", env=account_env())
        original_key = (legacy / "secrets.env").read_bytes()
        # Legacy user-writable metadata is not a source of privileged executable paths.
        meta = legacy / "data/install.json"
        contents = json.loads(meta.read_text()); contents["binary"] = str(legacy / "data/untrusted")
        meta.write_text(json.dumps(contents))
        run(args.binary, "upgrade", "--config", legacy / "config.yaml", "--file", args.binary)
        assert (legacy / "secrets.env").read_bytes() == original_key
        verify(legacy, legacy_port)
        with sqlite3.connect(legacy / "data/cyapi.db") as db:
            assert db.execute("select count(*) from users").fetchone()[0] == 1
        print("NAS installation, ACL, config edit, ownership and v0.0.2 upgrade checks passed.")
    finally:
        for root in roots:
            subprocess.run(["systemctl", "disable", "--now", unit(root)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            path = Path("/etc/systemd/system") / unit(root)
            path.unlink(missing_ok=True)
        subprocess.run(["systemctl", "daemon-reload"], check=False)
        for root in roots:
            shutil.rmtree(root)


if __name__ == "__main__":
    main()
