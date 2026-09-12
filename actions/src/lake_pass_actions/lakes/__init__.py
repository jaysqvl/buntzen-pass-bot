"""Allowlisted destinations, independent of browser implementation details."""

from types import MappingProxyType

from ..errors import ProtocolError
from .base import Lake
from .buntzen import LAKE as BUNTZEN


LEGACY_LAKE_ID = BUNTZEN.id
LEGACY_PROVIDER_ID = BUNTZEN.provider_id
LAKES = MappingProxyType({BUNTZEN.id: BUNTZEN})


def resolve_lake(lake_id: str) -> Lake:
    if not isinstance(lake_id, str) or lake_id not in LAKES:
        raise ProtocolError("unsupported lake_id")
    return LAKES[lake_id]
