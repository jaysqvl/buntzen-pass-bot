"""Canonical operator environment names with upgrade-compatible aliases."""

import os


def operator_env(suffix: str, default: str = "") -> str:
    # Presence matters: an explicit empty new value clears a legacy setting.
    key = f"LAKE_PASS_{suffix}"
    if key in os.environ:
        return os.environ[key]
    return os.environ.get(f"BUNTZEN_{suffix}", default)
