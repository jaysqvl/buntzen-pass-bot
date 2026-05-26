from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class PassPreference:
    key: str
    label: str
    url_kind: str
    text_patterns: tuple[str, ...]


ALL_DAY = PassPreference(
    key="all_day",
    label="All-day",
    url_kind="all_day",
    text_patterns=("All-day", "All Day", "8 a.m. to 8:00 p.m."),
)
AFTERNOON = PassPreference(
    key="afternoon",
    label="Afternoon",
    url_kind="half_day",
    text_patterns=("Afternoon",),
)
MORNING = PassPreference(
    key="morning",
    label="Morning",
    url_kind="half_day",
    text_patterns=("Morning",),
)


def build_pass_order(config) -> list[PassPreference]:
    order: list[PassPreference] = []
    if config.check_all_day:
        order.append(ALL_DAY)
    if config.check_afternoon:
        order.append(AFTERNOON)
    if config.check_morning:
        order.append(MORNING)
    return order
