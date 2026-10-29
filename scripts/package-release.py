#!/usr/bin/env python3
"""Package an explicit set of release files; requires only the Python standard library."""
import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import tarfile
import time
import zipfile


def package(directory: Path) -> None:
    repo = Path(__file__).resolve().parent.parent
    documentation = (repo / "docs" / "10-nas-install.md").read_bytes()
    epoch = int(os.environ.get("SOURCE_DATE_EPOCH", "0"))
    binaries = ["cyapi-linux-amd64", "cyapi-linux-arm64", "cyapi-windows-amd64.exe"]
    for name in binaries:
        binary = directory / name
        if not binary.is_file() or binary.is_symlink():
            raise ValueError(f"Missing regular release binary: {name}")
    archives = []
    for arch in ("amd64", "arm64"):
        name = f"cyapi-nas-linux-{arch}.tar.gz"
        with (directory / name).open("wb") as raw:
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as compressed:
                with tarfile.open(fileobj=compressed, mode="w") as archive:
                    for target, data, mode in (
                        ("cyapi", (directory / f"cyapi-linux-{arch}").read_bytes(), 0o755),
                        ("INSTALL.md", documentation, 0o644),
                    ):
                        info = tarfile.TarInfo(target)
                        info.mode = mode
                        info.mtime = epoch
                        info.size = len(data)
                        archive.addfile(info, io.BytesIO(data))
        archives.append(name)
    name = "cyapi-windows-amd64.zip"
    timestamp = time.gmtime(max(epoch, 315532800))[:6]
    with zipfile.ZipFile(directory / name, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for target, data in (("cyapi.exe", (directory / binaries[2]).read_bytes()), ("INSTALL.md", documentation)):
            info = zipfile.ZipInfo(target, timestamp)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = 0o644 << 16
            archive.writestr(info, data)
    archives.append(name)
    checksums = []
    for name in binaries + archives:
        with (directory / name).open("rb") as handle:
            digest = hashlib.file_digest(handle, "sha256").hexdigest()
        checksums.append(f"{digest}  {name}")
    (directory / "SHA256SUMS").write_text("\n".join(checksums) + "\n", encoding="ascii")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    package(parser.parse_args().directory.resolve())
