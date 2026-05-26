from __future__ import annotations

import os
from pathlib import Path


def appdata_dir() -> Path:
    path = Path(os.getenv("APPDATA_DIR", "./appdata")).expanduser()
    path.mkdir(parents=True, exist_ok=True)
    return path


def database_path() -> Path:
    return appdata_dir() / "buntzen.db"


def profiles_dir() -> Path:
    path = appdata_dir() / "profiles"
    path.mkdir(parents=True, exist_ok=True)
    return path


def artifacts_dir() -> Path:
    path = appdata_dir() / "artifacts"
    path.mkdir(parents=True, exist_ok=True)
    return path


def max_concurrent_jobs() -> int:
    raw = os.getenv("MAX_CONCURRENT_JOBS", "2")
    try:
        value = int(raw)
    except ValueError:
        value = 2
    return max(1, min(value, 8))
