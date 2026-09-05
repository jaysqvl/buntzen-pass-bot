from __future__ import annotations

import unittest
from datetime import date

from buntzen_actions.calendar_dates import resolve_button_date


class CalendarDateTests(unittest.TestCase):
    def test_live_zero_padded_days_use_the_scoped_month_and_year(self) -> None:
        self.assertEqual(
            resolve_button_date("06", {"aria-label": "Sunday 06"}, ["September-2026"]),
            date(2026, 9, 6),
        )

    def test_complete_metadata_can_supply_missing_heading(self) -> None:
        self.assertEqual(
            resolve_button_date("15", {"data-date": "2030-01-15"}, []),
            date(2030, 1, 15),
        )

    def test_full_textual_date_in_accessible_label_is_supported(self) -> None:
        self.assertEqual(
            resolve_button_date("06", {"aria-label": "Sunday, September 6, 2026"}, []),
            date(2026, 9, 6),
        )

    def test_day_prefix_is_never_used_as_a_date_match(self) -> None:
        self.assertEqual(
            resolve_button_date("January 10", {}, ["January-2030"]),
            date(2030, 1, 10),
        )

    def test_duplicate_identical_month_headings_are_consistent(self) -> None:
        self.assertEqual(
            resolve_button_date("06", {}, ["September-2026", "Sep 2026"]),
            date(2026, 9, 6),
        )

    def test_missing_ambiguous_contradictory_and_invalid_dates_are_rejected(self) -> None:
        cases = (
            ("day only", "06", {"aria-label": "Sunday 06"}, []),
            ("missing year", "06", {}, ["September"]),
            ("multiple months", "06", {}, ["September-2026", "October-2026"]),
            ("wrong heading month", "15", {"data-date": "2030-01-15"}, ["February-2030"]),
            ("wrong heading year", "15", {"data-date": "2030-01-15"}, ["January-2031"]),
            ("conflicting numeric day", "14", {"data-date": "2030-01-15"}, []),
            ("conflicting label", "15", {"data-date": "2030-01-15", "aria-label": "January 16, 2030"}, []),
            ("conflicting metadata", "15", {"data-date": "2030-01-15", "datetime": "2030-02-15"}, []),
            ("invalid day", "30", {}, ["February-2030"]),
            ("unknown numeric format", "01/02/2030", {}, []),
            ("partial metadata", "06", {"data-date": "September 6"}, ["September-2026"]),
            ("missing metadata", "", {}, []),
        )
        for label, text, attributes, headings in cases:
            with self.subTest(case=label):
                self.assertIsNone(resolve_button_date(text, attributes, headings))


if __name__ == "__main__":
    unittest.main()
