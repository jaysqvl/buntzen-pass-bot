from __future__ import annotations

import re
import time
from dataclasses import dataclass
from datetime import date
from typing import Any, Callable, Mapping, Sequence


_MONTHS = {
    name: month
    for month, names in enumerate(
        (
            ("january", "jan"), ("february", "feb"), ("march", "mar"),
            ("april", "apr"), ("may",), ("june", "jun"), ("july", "jul"),
            ("august", "aug"), ("september", "sep", "sept"),
            ("october", "oct"), ("november", "nov"), ("december", "dec"),
        ),
        start=1,
    )
    for name in names
}
_WEEKDAY = re.compile(
    r"^(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday|"
    r"mon|tue|wed|thu|fri|sat|sun)[,\s]+",
    re.I,
)


def _date_parts(value: str) -> tuple[int | None, int | None, int] | None:
    value = _WEEKDAY.sub("", " ".join(value.strip().lower().split()))
    match = re.fullmatch(r"(\d{4})-(\d{2})-(\d{2})", value)
    if match:
        year, month, day = map(int, match.groups())
        return year, month, day
    if re.fullmatch(r"\d{1,2}", value):
        return None, None, int(value)
    match = re.fullmatch(r"([a-z]+)\.?\s+(\d{1,2})(?:,?\s+(\d{4}))?", value)
    if match and match[1] in _MONTHS:
        return int(match[3]) if match[3] else None, _MONTHS[match[1]], int(match[2])
    return None


def resolve_button_date(
    text: str, attributes: Mapping[str, str | None], month_labels: Sequence[str]
) -> date | None:
    """Resolve a complete date without guessing from a day or a substring.

    Yodel's live calendar supplies zero-padded days and a separate month/year
    heading. Explicit metadata is also supported, but every date-bearing label
    and the scoped heading must agree before a button can be used.
    """

    months: set[tuple[int, int]] = set()
    for label in month_labels:
        match = re.fullmatch(r"([a-z]+)\.?[\s/-]+(\d{4})", label.strip().lower())
        if not match or match[1] not in _MONTHS:
            return None
        months.add((int(match[2]), _MONTHS[match[1]]))
    if len(months) > 1:
        return None

    parts: list[tuple[int | None, int | None, int]] = []
    for name in ("data-date", "datetime"):
        value = attributes.get(name)
        if value:
            parsed = _date_parts(value)
            if parsed is None or parsed[0] is None or parsed[1] is None:
                return None
            parts.append(parsed)
    for value in (text, attributes.get("aria-label"), attributes.get("title")):
        if not value:
            continue
        parsed = _date_parts(value)
        if parsed is not None:
            parts.append(parsed)
        elif re.search(r"\d", value):
            # An unfamiliar date format must not silently override known data.
            return None
    if not parts:
        return None

    complete = {(year, month, day) for year, month, day in parts if year and month}
    if len(complete) > 1:
        return None
    if complete:
        year, month, _day = next(iter(complete))
        if months and (year, month) not in months:
            return None
    elif months:
        year, month = next(iter(months))
    else:
        return None

    resolved: set[date] = set()
    try:
        for part_year, part_month, day in parts:
            resolved.add(date(part_year or year, part_month or month, day))
    except ValueError:
        return None
    return next(iter(resolved)) if len(resolved) == 1 else None


@dataclass(frozen=True)
class DateButton:
    locator: Any
    value: date
    is_selected: bool
    is_enabled: bool


def _read_calendar(
    container: Any, check_cancelled: Callable[[], None]
) -> list[DateButton] | None:
    """Read only this pass card's dates, failing on ambiguous metadata."""

    from playwright.sync_api import Error as PlaywrightError

    check_cancelled()
    try:
        headings = container.locator(".month")
        if headings.count() > 4:
            return None
        months = [
            headings.nth(index).inner_text(timeout=500).strip()
            for index in range(headings.count())
            if headings.nth(index).is_visible()
        ]
        buttons = container.locator("button.date")
        count = buttons.count()
        if count < 1 or count > 62:
            return None
    except PlaywrightError:
        return None

    entries = []
    for index in range(count):
        check_cancelled()
        try:
            button = buttons.nth(index)
            if not button.is_visible():
                continue
            attributes = {
                name: button.get_attribute(name, timeout=500)
                for name in ("aria-label", "title", "data-date", "datetime")
            }
            value = resolve_button_date(button.inner_text(timeout=500), attributes, months)
            if value is None:
                return None
            selected = (
                "active" in (button.get_attribute("class", timeout=500) or "").split()
                or button.get_attribute("aria-selected", timeout=500) == "true"
                or button.get_attribute("aria-pressed", timeout=500) == "true"
            )
            entries.append(DateButton(button, value, selected, button.is_enabled()))
        except PlaywrightError:
            return None
    return entries


def select_target_date(
    page: Any, container: Any, target: date, check_cancelled: Callable[[], None]
) -> bool:
    """Select and verify a complete date in one independently controlled card."""

    from playwright.sync_api import Error as PlaywrightError

    entries = _read_calendar(container, check_cancelled)
    if entries is None:
        return False
    matches = [entry for entry in entries if entry.value == target]
    if len(matches) != 1 or not matches[0].is_enabled:
        return False
    check_cancelled()
    try:
        matches[0].locator.click(timeout=3_000)
    except PlaywrightError:
        return False

    # A click alone does not prove the card's independent calendar changed.
    deadline = time.monotonic() + 5.0
    while time.monotonic() < deadline:
        check_cancelled()
        entries = _read_calendar(container, check_cancelled)
        if entries is not None:
            matches = [entry for entry in entries if entry.value == target]
            selected = [entry.value for entry in entries if entry.is_selected]
            if len(matches) == 1 and selected == [target]:
                return True
        page.wait_for_timeout(100)
    return False
