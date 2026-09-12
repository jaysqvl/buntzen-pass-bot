"""Trusted provider dispatch; profile data cannot select arbitrary modules."""

from dataclasses import dataclass
from types import MappingProxyType
from typing import Any, Callable

from ..errors import ProtocolError
from ..lakes import resolve_lake
from .yodel import create_action as create_yodel_action


@dataclass(frozen=True)
class Provider:
    id: str
    label: str
    create_action: Callable[..., Any]


PROVIDERS = MappingProxyType({
    "yodel": Provider("yodel", "Yodel", create_yodel_action),
})


def resolve_provider(lake_id: str, provider_id: str) -> Provider:
    lake = resolve_lake(lake_id)
    if not isinstance(provider_id, str) or provider_id not in PROVIDERS:
        raise ProtocolError("unsupported provider_id")
    if provider_id != lake.provider_id:
        raise ProtocolError("provider_id does not support the selected lake")
    return PROVIDERS[provider_id]
