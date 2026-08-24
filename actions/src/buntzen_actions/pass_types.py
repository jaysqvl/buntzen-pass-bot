from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class PassPreference:
    key: str
    label: str
    url_kind: str
    text_patterns: tuple[str, ...]


PASS_PREFERENCES = {
    "all_day": PassPreference(
        key="all_day",
        label="All-day",
        url_kind="all_day",
        text_patterns=("All-day", "All Day", "8 a.m. to 8:00 p.m."),
    ),
    "afternoon": PassPreference(
        key="afternoon",
        label="Afternoon",
        url_kind="half_day",
        text_patterns=("Afternoon",),
    ),
    "morning": PassPreference(
        key="morning",
        label="Morning",
        url_kind="half_day",
        text_patterns=("Morning",),
    ),
}


def build_pass_order(keys: tuple[str, ...]) -> list[PassPreference]:
    return [PASS_PREFERENCES[key] for key in keys]
