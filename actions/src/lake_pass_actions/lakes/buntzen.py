"""Buntzen Lake pass choices; scheduling and URL defaults live in Go's catalog."""

from types import MappingProxyType

from ..pass_types import PassPreference
from .base import Lake


PASS_PREFERENCES = MappingProxyType({
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
})

LAKE = Lake(
    id="buntzen",
    label="Buntzen Lake",
    provider_id="yodel",
    pass_preferences=PASS_PREFERENCES,
)
