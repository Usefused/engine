"""Remove build/install-only files from the optional portable runtime."""
from pathlib import Path
import shutil


def prune_runtime(root: Path, executable: str):
    def remove(path):
        if path.is_symlink() or path.is_file():
            path.unlink()
        elif path.is_dir():
            shutil.rmtree(path)

    python = root / "python"
    # CPython's bootstrap installers are unrelated to the locked --target packages.
    for path in (python / "include", python / "Include", python / "libs",
                 python / "share/man", python / "share/doc", python / "Scripts",
                 root / "packages/bin", root / "packages/Scripts"):
        remove(path)
    for library in (python / "lib/python3.12", python / "Lib"):
        remove(library / "ensurepip")
        for pattern in ("pip", "pip-*.dist-info", "setuptools", "setuptools-*.dist-info",
                        "pkg_resources", "_distutils_hack", "distutils-precedence.pth",
                        "wheel", "wheel-*.dist-info"):
            for path in (library / "site-packages").glob(pattern):
                remove(path)
    # The launcher uses this exact interpreter, so copied aliases need not each
    # occupy a full executable in our link-free archives.
    binary = root / executable
    for path in (python / "bin").glob("*"):
        if path != binary and (path.name in ("python", "python3") or
                               path.name.endswith("-config") or
                               path.name.startswith(("pip", "idle", "pydoc", "2to3"))):
            remove(path)
    for path in sorted(root.rglob("*"), key=lambda p: len(p.parts), reverse=True):
        if path.is_dir() and path.name in {
            "test", "tests", "__pycache__", ".pytest_cache", ".mypy_cache",
            ".ruff_cache", ".cache", "pkgconfig",
        }:
            remove(path)
        elif path.is_file() and path.suffix in {".pyc", ".pyo", ".a"}:
            remove(path)
    # Keep .dist-info, licenses, provider data, .so/.pyd/.dll and sysconfig data:
    # these are used by imports and runtime version/entry-point discovery.
