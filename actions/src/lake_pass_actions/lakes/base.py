"""Destination rules consumed by booking provider adapters."""

from dataclasses import dataclass
from typing import Mapping

from ..pass_types import PassPreference


@dataclass(frozen=True)
class Lake:
    id: str
    label: str
    provider_id: str
    pass_preferences: Mapping[str, PassPreference]
