from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class PassPreference:
    key: str
    label: str
    url_kind: str
    text_patterns: tuple[str, ...]
