"""Require real uv sync/build to reject altered backend hashes in fresh state."""
from __future__ import annotations

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import tomllib


ROOT = Path(__file__).resolve().parent.parent


def reject_hash_mismatch(command: list[str], environment: dict[str, str]) -> None:
    result = subprocess.run(
        command, text=True, capture_output=True, timeout=180, env=environment,
    )
    if result.returncode == 0 or "hash mismatch" not in result.stderr.lower():
        raise RuntimeError("backend integrity check did not reject changed bytes:\n" + result.stderr)


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="buntzen-backend-integrity-") as directory:
        root = Path(directory)
        for name in ("pyproject.toml", "uv.lock", "README.md"):
            shutil.copyfile(ROOT / "actions" / name, root / name)
        shutil.copytree(
            ROOT / "actions/src", root / "src",
            ignore=shutil.ignore_patterns("__pycache__", "*.egg-info"),
        )
        lock_path = root / "uv.lock"
        original = lock_path.read_text()
        package = next(package for package in tomllib.loads(original)["package"] if package["name"] == "setuptools")
        assert package["wheels"], "backend has no locked wheel"
        changed = original
        artifacts = [*package["wheels"], package["sdist"]]
        for artifact in artifacts:
            changed = changed.replace(artifact["hash"], "sha256:" + "0" * 64)
        assert changed != original
        lock_path.write_text(changed)
        environment = dict(os.environ, UV_PROJECT_ENVIRONMENT=str(root / ".venv"))
        reject_hash_mismatch([
            "uv", "sync", "--locked", "--no-cache", "--project", str(root),
        ], environment)

        lock_path.write_text(original)
        assert re.fullmatch(r"\d+\.\d+\.\d+", package["version"])
        constraints = root / "altered-build-requirements.txt"
        constraints.write_text(f"setuptools=={package['version']} --hash=sha256:{'0' * 64}\n")
        reject_hash_mismatch([
            "uv", "build", str(root), "--no-config", "--no-cache",
            "--build-constraints", str(constraints), "--require-hashes",
            "--out-dir", str(root / "dist"),
        ], environment)
    print("Altered backend hashes rejected by fresh locked sync and isolated build.")


if __name__ == "__main__":
    main()
