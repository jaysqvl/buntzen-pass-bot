from __future__ import annotations

import logging
import random
import time
from datetime import datetime
from typing import Callable
from zoneinfo import ZoneInfo


logger = logging.getLogger("buntzen_pass_bot.scheduler")


def sleep_until(target: datetime, timezone: ZoneInfo, final_poll_seconds: float = 0.25) -> None:
    """Sleep until an aware datetime, using coarse sleeps until the final seconds."""
    while True:
        now = datetime.now(timezone)
        remaining = (target - now).total_seconds()
        if remaining <= 0:
            return
        if remaining > 60:
            sleep_for = min(remaining - 30, 60)
        elif remaining > 5:
            sleep_for = min(remaining - 2, 5)
        else:
            sleep_for = min(remaining, final_poll_seconds)
        time.sleep(max(0.01, sleep_for))


def wait_with_keepalive(
    until: datetime,
    timezone: ZoneInfo,
    keepalive: Callable[[], None],
    min_interval_seconds: int,
    max_interval_seconds: int,
) -> None:
    """Run low-frequency randomized keepalive work until the target time."""
    while True:
        now = datetime.now(timezone)
        remaining = (until - now).total_seconds()
        if remaining <= 0:
            return
        if remaining <= 20:
            time.sleep(min(remaining, 1.0))
            continue

        sleep_for = random.uniform(min_interval_seconds, max_interval_seconds)
        sleep_for = min(sleep_for, max(1.0, remaining - 10))
        logger.debug("Session keepalive sleeping for %.1fs", sleep_for)
        time.sleep(sleep_for)
        if datetime.now(timezone) < until:
            keepalive()
