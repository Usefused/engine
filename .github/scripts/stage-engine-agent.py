#!/usr/bin/env python3
"""Verify native CI runtime artifacts and stage the Engine's embedded release inputs."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tarfile

PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64")


def stage(source: Path, assets: Path, releases: Path):
    manifests = {}
    for platform in PLATFORMS:
        target = platform.replace("/", "-")
        directory = source / f"agent-runtime-{target}"
        manifest = json.loads((directory / f"manifest-{target}.json").read_text())
        if set(manifest) != {platform}:
            raise ValueError(f"unexpected manifest platforms for {target}")
        entry = manifest[platform]
        name = f"runtime-{target}.tar.gz"
        expected_python = "python/python.exe" if platform.startswith("windows/") else "python/bin/python3.12"
        if entry.get("archive") != name or entry.get("python") != expected_python:
            raise ValueError(f"unexpected runtime layout for {target}")
        archive = directory / name
        with archive.open("rb") as stream:
            hasher = hashlib.sha256()
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                hasher.update(chunk)
            digest = hasher.hexdigest()
        if digest != entry.get("sha256"):
            raise ValueError(f"runtime checksum mismatch for {target}")
        manifests[platform] = entry
    agent = source / "agent-runtime-linux-amd64" / "agent.tar.gz"
    with tarfile.open(agent, "r:gz") as archive:
        names = {member.name.removeprefix("./") for member in archive.getmembers() if member.isfile()}
        if not {"harnest-manifest.json", "launch.py"}.issubset(names):
            raise ValueError("compiled agent is missing required entry points")
    assets.mkdir(parents=True, exist_ok=True)
    releases.mkdir(parents=True, exist_ok=True)
    for platform, entry in manifests.items():
        target = platform.replace("/", "-")
        shutil.copy2(source / f"agent-runtime-{target}" / entry["archive"], releases / entry["archive"])
    shutil.copy2(agent, assets / "agent.tar.gz")
    (assets / "runtimes.json").write_text(json.dumps(manifests, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--assets", type=Path, default=Path("internal/agentbundle/assets"))
    parser.add_argument("--release-dir", type=Path, default=Path("agent-release"))
    args = parser.parse_args()
    stage(args.input, args.assets, args.release_dir)
