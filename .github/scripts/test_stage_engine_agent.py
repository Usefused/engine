"""Exercise release staging against a clean tagged-checkout equivalent."""
import hashlib
import io
import json
from pathlib import Path
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64")


class ReleaseStagingTest(unittest.TestCase):
    """Guard the release workflow's clean-tree and checksum boundaries."""

    def setUp(self):
        """Use synthetic native artifacts and tracked fallbacks in an isolated Git repository."""
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for name in (".gitignore", ".github/scripts/stage-engine-agent.py",
                     "internal/agentbundle/assets/agent.tar.gz",
                     "internal/agentbundle/assets/runtimes.json"):
            destination = self.root / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, destination)
        self.run_command(["git", "init", "-q"])
        self.run_command(["git", "add", "."])
        self.run_command(["git", "-c", "user.name=Fused test", "-c",
                          "user.email=test@example.invalid", "-c", "core.hooksPath=/dev/null",
                          "commit", "--no-gpg-sign", "-qm", "fixture"])
        for platform in PLATFORMS:
            target = platform.replace("/", "-")
            directory = self.root / "agent-artifacts" / f"agent-runtime-{target}"
            directory.mkdir(parents=True)
            data = f"synthetic runtime for {platform}".encode()
            name = f"runtime-{target}.tar.gz"
            (directory / name).write_bytes(data)
            # Windows runtime manifests use their native executable path.
            python = "python/python.exe" if platform.startswith("windows/") else "python/bin/python3.12"
            entry = {"archive": name, "python": python, "sha256": hashlib.sha256(data).hexdigest()}
            (directory / f"manifest-{target}.json").write_text(json.dumps({platform: entry}))
        agent = self.root / "agent-artifacts/agent-runtime-linux-amd64/agent.tar.gz"
        with tarfile.open(agent, "w:gz") as archive:
            for name in ("launch.py", "harnest-manifest.json"):
                data = b"synthetic entry point"
                header = tarfile.TarInfo(name)
                header.size = len(data)
                archive.addfile(header, io.BytesIO(data))
        # Run the actual workflow command so regressions in its destination are covered.
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        commands = [line.strip().removeprefix("run: ") for line in workflow.splitlines()
                    if line.strip().startswith("run: python3 .github/scripts/stage-engine-agent.py")]
        self.assertEqual(len(commands), 1)
        self.command = shlex.split(commands[0])

    def run_command(self, command):
        """Capture failures without mixing fixture output with the developer's checkout."""
        return subprocess.run(command, cwd=self.root, check=True, text=True, capture_output=True)

    def test_release_staging_keeps_checkout_clean(self):
        """Verified release inputs must be available without changing tracked fallback assets."""
        self.run_command(self.command)
        self.assertEqual(self.run_command(["git", "status", "--porcelain"]).stdout, "")
        assets = self.root / "internal/agentbundle/release-assets"
        manifest = json.loads((assets / "runtimes.json").read_text())
        self.assertEqual(set(manifest), set(PLATFORMS))
        self.assertEqual((assets / "agent.tar.gz").read_bytes(),
                         (self.root / "agent-artifacts/agent-runtime-linux-amd64/agent.tar.gz").read_bytes())
        for entry in manifest.values():
            data = (self.root / "agent-release" / entry["archive"]).read_bytes()
            self.assertEqual(hashlib.sha256(data).hexdigest(), entry["sha256"])

    def test_corrupt_runtime_cannot_be_staged(self):
        """Keep the checksum guard intact while moving generated files away from source inputs."""
        archive = self.root / "agent-artifacts/agent-runtime-linux-amd64/runtime-linux-amd64.tar.gz"
        archive.write_bytes(b"corrupt")
        result = subprocess.run(self.command, cwd=self.root, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime checksum mismatch", result.stderr)
        self.assertFalse((self.root / "internal/agentbundle/release-assets").exists())
        self.assertEqual(self.run_command(["git", "status", "--porcelain"]).stdout, "")
