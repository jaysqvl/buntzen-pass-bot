#!/usr/bin/env python3
from __future__ import annotations

import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
PACKAGE_NAME = "buntzen-actions"


def fail(message: str) -> None:
    raise SystemExit(f"release metadata: {message}")


def read_json(path: str) -> object:
    return json.loads((ROOT / path).read_text(encoding="utf-8"))


def main() -> int:
    manifest = read_json(".release-please-manifest.json")
    config = read_json("release-please-config.json")
    pyproject_text = (ROOT / "actions/pyproject.toml").read_text(encoding="utf-8")
    lock_text = (ROOT / "actions/uv.lock").read_text(encoding="utf-8")

    if not isinstance(manifest, dict) or not isinstance(config, dict):
        fail("manifest and configuration must be JSON objects")

    manifest_version = manifest.get(".")
    project = re.search(
        r"(?ms)^\[project\]\n(?P<body>.*?)(?=^\[|\Z)",
        pyproject_text,
    )
    if project is None:
        fail("actions/pyproject.toml has no project table")
    project_version_match = re.search(
        r'(?m)^version = "(?P<version>[^"]+)"$',
        project.group("body"),
    )
    if project_version_match is None:
        fail("actions/pyproject.toml has no project version")
    project_version = project_version_match.group("version")

    package_blocks = re.findall(
        r"(?ms)^\[\[package\]\]\n(?P<body>.*?)(?=^\[\[package\]\]|\Z)",
        lock_text,
    )
    locked_matches = [
        block
        for block in package_blocks
        if re.search(rf'(?m)^name = "{re.escape(PACKAGE_NAME)}"$', block)
    ]
    if len(locked_matches) != 1:
        fail(f"actions/uv.lock must contain exactly one {PACKAGE_NAME} package")
    package_block = locked_matches[0]
    lock_version_match = re.search(
        r'(?m)^version = "(?P<version>[^"]+)"(?: # x-release-please-version)?$',
        package_block,
    )
    if lock_version_match is None:
        fail(f"actions/uv.lock has no version for {PACKAGE_NAME}")
    lock_version = lock_version_match.group("version")

    if not (
        isinstance(manifest_version, str)
        and manifest_version == project_version == lock_version
    ):
        fail("manifest, Python project, and lockfile versions do not match")

    packages_config = config.get("packages")
    root_config = packages_config.get(".") if isinstance(packages_config, dict) else None
    extra_files = root_config.get("extra-files") if isinstance(root_config, dict) else None
    if not isinstance(extra_files, list):
        fail("Release Please has no root extra-files list")

    expected_entries = (
        {
            "type": "toml",
            "path": "actions/pyproject.toml",
            "jsonpath": "$.project.version",
        },
        {"type": "generic", "path": "actions/uv.lock"},
    )
    for expected in expected_entries:
        if expected not in extra_files:
            fail(f"Release Please does not update {expected['path']}")

    if not re.search(
        r'(?m)^version = "[^"]+" # x-release-please-version$',
        package_block,
    ):
        fail("actions/uv.lock lacks the package version update marker")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
