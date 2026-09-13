#!/usr/bin/env python3
"""Point latest at an accepted digest only for the newest stable GitHub release."""

from __future__ import annotations

import json
import os
import re
import subprocess
from pathlib import Path


STABLE_TAG = re.compile(r"(?:lake-pass-bot|buntzen-pass-bot)-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def stable_version(tag: str) -> tuple[int, ...] | None:
    match = STABLE_TAG.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None


def command(*args: str) -> str:
    return subprocess.check_output(args, text=True)


def promote(repository: str, tag: str, digest: str) -> str:
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("invalid GitHub repository")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError("invalid accepted release digest")
    version = stable_version(tag)
    if version is None:
        return "latest unchanged: prereleases do not update the stable image."

    pages = json.loads(command(
        "gh", "api", "--paginate", "--slurp",
        f"repos/{repository}/releases?per_page=100",
    ))
    if not isinstance(pages, list) or not all(isinstance(page, list) for page in pages):
        raise ValueError("GitHub returned an invalid release inventory")
    stable = {}
    for page in pages:
        for release in page:
            if not isinstance(release, dict):
                raise ValueError("GitHub returned an invalid release")
            release_tag = release.get("tag_name")
            if not isinstance(release_tag, str):
                raise ValueError("GitHub returned a release without a tag")
            candidate = stable_version(release_tag)
            if (candidate is not None and release.get("draft") is False
                    and release.get("prerelease") is False):
                stable[release_tag] = candidate

    # Check again inside the shared promotion lock, after all publication gates.
    # Publishing an old release manually must not move the update channel back.
    if tag not in stable:
        return "latest unchanged: this is not a published stable release."
    newest = max(stable, key=stable.__getitem__)
    if stable[newest] > version:
        return f"latest unchanged: {newest} is newer than {tag}."

    image = f"ghcr.io/{repository.lower()}"
    latest = f"{image}:latest"
    command("docker", "buildx", "imagetools", "create", "--tag", latest, f"{image}@{digest}")
    manifest = json.loads(command(
        "docker", "buildx", "imagetools", "inspect", latest, "--format", "{{json .Manifest}}",
    ))
    if not isinstance(manifest, dict) or manifest.get("digest") != digest:
        raise ValueError("latest did not resolve to the accepted release digest")
    return f"Published `{latest}` at `{digest}` ({tag}). Update and re-pull the image in Portainer to deploy it."


def main() -> None:
    message = promote(os.environ["GITHUB_REPOSITORY"], os.environ["RELEASE_TAG"], os.environ["RELEASE_DIGEST"])
    print(message)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with Path(summary).open("a", encoding="utf-8") as output:
            output.write(f"\n{message}\n")


if __name__ == "__main__":
    main()
