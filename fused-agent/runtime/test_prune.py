"""Protect runtime imports and metadata while excluding build-only files."""
from pathlib import Path
import tempfile
import unittest

from prune import prune_runtime


class PruneRuntimeTests(unittest.TestCase):
    def test_posix_and_windows_layouts(self):
        for executable, library in (("python/bin/python3.12", "python/lib/python3.12"),
                                    ("python/python.exe", "python/Lib")):
            with self.subTest(executable=executable), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                removed = [
                    "python/include/Python.h", "python/Include/Python.h", "python/libs/python312.lib",
                    "python/lib/libpython3.12.a", "python/lib/pkgconfig/python.pc",
                    "python/bin/python", "python/bin/python3", "python/bin/pip3.12",
                    "python/bin/python3.12-config", "python/share/man/python.1", "python/Scripts/pip.exe",
                    library + "/ensurepip/_bundled/pip.whl",
                    library + "/site-packages/pip/__init__.py",
                    library + "/site-packages/pip-1.dist-info/METADATA",
                    library + "/site-packages/setuptools/__init__.py",
                    library + "/site-packages/_distutils_hack/__init__.py",
                    library + "/site-packages/distutils-precedence.pth",
                    library + "/test/test_json.py", "packages/provider/tests/fixture.json",
                    "packages/provider/__pycache__/runtime.pyc", "packages/.cache/index.json",
                    "packages/bin/installer", "packages/Scripts/installer.exe",
                ]
                kept = [
                    executable, library + "/json/__init__.py", library + "/_sysconfigdata.py",
                    "python/lib/libpython3.12.so", "python/python312.dll",
                    "python/share/licenses/LICENSE", "packages/provider/runtime.py",
                    "packages/provider/test_client.py", "packages/provider/models/index.json",
                    "packages/provider/native.so", "packages/provider/native.pyd",
                    "packages/provider-1.dist-info/METADATA", "packages/provider-1.dist-info/entry_points.txt",
                    "packages/provider-1.dist-info/licenses/LICENSE", "packages/packaging/__init__.py",
                ]
                for name in removed + kept:
                    path = root / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text("fixture")
                prune_runtime(root, executable)
                for name in removed:
                    self.assertFalse((root / name).exists(), name)
                for name in kept:
                    self.assertEqual((root / name).read_text(), "fixture", name)
                prune_runtime(root, executable)  # Repeatable, including absent optional paths.


if __name__ == "__main__":
    unittest.main()
