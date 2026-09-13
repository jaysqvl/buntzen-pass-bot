"""Yodel browser adapter and provider-specific checkout safeguards."""

from typing import Any


def create_action(**kwargs: Any) -> Any:
    # Import only after the destination/provider pair has been validated.
    from .action import YodelAction

    return YodelAction(**kwargs)
