#!/usr/bin/env python3
"""Publish verified assets, resuming empty/partial releases without replacing files."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

ASSETS = (
    "cyapi-linux-amd64", "cyapi-linux-arm64", "cyapi-windows-amd64.exe",
    "cyapi-nas-linux-amd64.tar.gz", "cyapi-nas-linux-arm64.tar.gz",
    "cyapi-windows-amd64.zip", "SHA256SUMS",
)


def gh(*args):
    return subprocess.check_output(["gh", *args], stderr=subprocess.PIPE, text=True)


def release_info(repo, tag):
    try:
        return json.loads(gh("api", f"repos/{repo}/releases/tags/{tag}"))
    except subprocess.CalledProcessError as error:
        if "HTTP 404" not in (error.stderr or ""):
            raise
    # GitHub's tag endpoint can hide draft releases, including one just created
    # by this workflow. The authenticated release list includes those drafts.
    pages = json.loads(gh("api", f"repos/{repo}/releases?per_page=100", "--paginate", "--slurp"))
    for page in pages:
        for release in page:
            if release["tag_name"] == tag:
                return release
    return None


def wait_for_release(repo, tag):
    # Draft/list visibility can lag behind a successful create response.
    for delay in (0, 0.5, 1, 2, 4):
        if delay:
            time.sleep(delay)
        info = release_info(repo, tag)
        if info is not None:
            return info
    raise ValueError("Created release is not visible yet; rerun to resume")


def checksum(path):
    with path.open("rb") as handle:
        return hashlib.file_digest(handle, "sha256").hexdigest()


def verify_remote(directory, repo, tag, info, hashes):
    missing = []
    existing = {asset["name"]: asset for asset in info["assets"]}
    for name in ASSETS:
        asset = existing.get(name)
        if asset is None:
            missing.append(name)
            continue
        digest = asset.get("digest")
        if digest is None:
            # Older GitHub assets may not have a server-provided checksum.
            with tempfile.TemporaryDirectory(prefix="cyapi-release-check-") as work:
                gh("release", "download", tag, "--repo", repo, "--pattern", name, "--dir", work)
                digest = "sha256:" + checksum(Path(work) / name)
        if digest != "sha256:" + hashes[name]:
            raise ValueError(f"Existing asset differs: {name}; refusing to overwrite")
    return missing


def publish(directory, repo, tag, sha, keep_draft, prerelease):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?", tag):
        raise ValueError("Invalid release tag")
    hashes = {}
    for name in ASSETS:
        path = directory / name
        if not path.is_file() or path.is_symlink():
            raise ValueError(f"Missing regular asset: {name}")
        hashes[name] = checksum(path)
    declared = {}
    for line in (directory / "SHA256SUMS").read_text(encoding="ascii").splitlines():
        digest, name = line.split("  ", 1)
        if name in declared:
            raise ValueError("Duplicate checksum entry")
        declared[name] = digest
    if declared != {name: hashes[name] for name in ASSETS[:-1]}:
        raise ValueError("SHA256SUMS does not match the release files")
    actual = json.loads(gh("api", f"repos/{repo}/commits/{tag}"))["sha"]
    if actual != sha:
        raise ValueError("Tag changed during the build; refusing to publish")
    info = release_info(repo, tag)
    if info is None:
        gh("release", "create", tag, "--repo", repo, "--verify-tag", "--target", sha,
           "--title", tag, "--generate-notes", "--draft")
        info = wait_for_release(repo, tag)
    missing = verify_remote(directory, repo, tag, info, hashes)
    if missing:
        gh("release", "upload", tag, "--repo", repo, *[str(directory / name) for name in missing])
    info = release_info(repo, tag)
    if verify_remote(directory, repo, tag, info, hashes):
        raise ValueError("Release upload is incomplete; rerun to resume")
    # An existing public release keeps its title, notes, prerelease and public state.
    if info["draft"]:
        gh("release", "edit", tag, "--repo", repo, f"--prerelease={str(prerelease).lower()}")
        if not keep_draft:
            gh("release", "edit", tag, "--repo", repo, "--draft=false")
    print("Release assets verified:", info["html_url"])


if __name__ == "__main__":
    try:
        publish(Path(sys.argv[1]).resolve(), os.environ["GH_REPO"], os.environ["RELEASE_TAG"],
                os.environ["RELEASE_SHA"], os.environ.get("KEEP_DRAFT") == "true",
                os.environ.get("PRERELEASE") == "true")
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.stderr or str(error)) from error
