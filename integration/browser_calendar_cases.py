"""Offline Chromium regressions invoked by the Go integration test suite."""

from __future__ import annotations

import json
import os
import threading
import time
import unittest
from contextlib import contextmanager
from datetime import date
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace
from unittest.mock import patch

from playwright.sync_api import sync_playwright

from buntzen_actions.calendar_dates import select_target_date
from buntzen_actions.errors import ActionError, Cancelled, OutcomeUnknown
from buntzen_actions.pass_types import PASS_PREFERENCES
from buntzen_actions.yodel import YodelAction


_SELECT_DATE = """<script>
function chooseDate(button) {
  const card = button.closest('.card');
  card.querySelectorAll('button.date').forEach(item => item.classList.remove('active'));
  button.classList.add('active');
  card.dataset.selected = button.innerText;
  document.body.dataset.dateClicks = String(Number(document.body.dataset.dateClicks || 0) + 1);
}
</script>"""


@contextmanager
def stalled_checkout_server():
    headers_sent = threading.Event()
    release_body = threading.Event()
    response_body = json.dumps(
        {
            "payment": {"succeeded": True, "errorMessage": None, "orderId": 123},
            "walletItems": [{"summaryField1": {"value": "Synthetic pass"}}],
        }
    ).encode()
    markup = b"""<button onclick="document.querySelector('#orderConfirmModal').style.display='block';fetch('/api/orders/checkout',{method:'POST'}).then(response=>response.json())">Yes</button>
      <div id="orderConfirmModal" style="display:none"><h2 class="heading">Confirmed</h2><a>See My Pass</a></div>"""

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(markup)))
            self.end_headers()
            self.wfile.write(markup)

        def do_POST(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(response_body)))
            self.end_headers()
            self.wfile.flush()
            headers_sent.set()
            # The test releases this only after verification has returned. A
            # finite fallback keeps the regression bounded against broken code.
            release_body.wait(timeout=5.0)
            try:
                self.wfile.write(response_body)
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(
        target=lambda: server.serve_forever(poll_interval=0.05), daemon=True
    )
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}", headers_sent
    finally:
        release_body.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=1.0)


def pass_card(kind: str, days: tuple[int, ...], month: str = "September-2026") -> str:
    buttons = "".join(
        f'<button class="date{" active" if index == 0 else ""}" '
        f'aria-label="Sunday {day:02}" onclick="chooseDate(this)">{day:02}</button>'
        for index, day in enumerate(days)
    )
    return f"""<div class="card ImageCard" id="{kind}" data-selected="{days[0]:02}">
      <h2>{kind.title()} Pass</h2><span class="month">{month}</span>
      <div class="datelist">{buttons}</div>
      <a class="smartSelectCustom" onclick="this.nextElementSibling.style.display='block'">Select Vehicle</a>
      <div class="popup smart-select-popup modal-in" style="display:none">
        <label class="item-radio"><input type="radio" name="{kind}-vehicle">
          <span class="item-title">Synthetic Vehicle</span></label>
        <a class="link popup-close" onclick="this.parentElement.style.display='none'">Done</a>
      </div>
      <a onclick="throw new Error('dry-run must never add to cart')">Add To Cart</a>
    </div>"""


class CalendarBrowserTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.playwright = sync_playwright().start()
        launch = {"headless": True}
        if executable := os.environ.get("BUNTZEN_E2E_BROWSER_EXECUTABLE"):
            launch["executable_path"] = executable
        cls.browser = cls.playwright.chromium.launch(**launch)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.browser.close()
        cls.playwright.stop()

    def setUp(self) -> None:
        self.page = self.browser.new_page()
        self.action = object.__new__(YodelAction)
        self.action.page = self.page
        self.action.control = SimpleNamespace(
            inbox=SimpleNamespace(check_cancelled=lambda: None),
            status=lambda *args: None,
        )
        self.action.config = SimpleNamespace(
            target_date=date(2026, 9, 6),
            half_day_pass_url="https://example.test/half",
            vehicle_keyword="Synthetic Vehicle",
        )
        self.action._goto_allowed = lambda _: None
        self.action._settle_page = lambda **kwargs: None
        self.action._human_pause = lambda *args: None
        self.action.diagnostics = SimpleNamespace(screenshot=lambda *args: None)

    def tearDown(self) -> None:
        self.page.close()

    def test_afternoon_uses_its_own_calendar_with_padded_and_two_digit_days(
        self,
    ) -> None:
        for current, target in ((5, 6), (14, 15)):
            with self.subTest(target=target):
                self.page.set_content(
                    _SELECT_DATE
                    + pass_card("morning", (current, target))
                    + pass_card("afternoon", (current, target))
                )
                self.action.config.target_date = date(2026, 9, target)
                result = self.action._try_pass(PASS_PREFERENCES["afternoon"], "dry-run")
                self.assertTrue(result.success, result.message)
                self.assertEqual(
                    self.page.locator("#morning").get_attribute("data-selected"),
                    f"{current:02}",
                )
                self.assertEqual(
                    self.page.locator("#afternoon").get_attribute("data-selected"),
                    f"{target:02}",
                )
                self.assertTrue(
                    self.page.locator('input[name="afternoon-vehicle"]').is_checked()
                )
                self.assertFalse(
                    self.page.locator('input[name="morning-vehicle"]').is_checked()
                )
                self.assertEqual(
                    self.page.locator("body").get_attribute("data-date-clicks"), "1"
                )

    def test_absent_wrong_month_day_prefix_and_duplicate_dates_never_click(
        self,
    ) -> None:
        cases = (
            ("absent", date(2026, 9, 7), pass_card("afternoon", (5, 6))),
            ("wrong month", date(2026, 10, 6), pass_card("afternoon", (5, 6))),
            (
                "January 1 versus 10",
                date(2030, 1, 1),
                pass_card("afternoon", (10,), "January-2030"),
            ),
            ("duplicate target", date(2026, 9, 6), pass_card("afternoon", (6, 6))),
            (
                "conflicting metadata",
                date(2026, 9, 6),
                pass_card("afternoon", (5, 6)).replace(
                    'aria-label="Sunday 06"',
                    'aria-label="Sunday 06" data-date="2026-10-06"',
                ),
            ),
        )
        for name, target, markup in cases:
            with self.subTest(case=name):
                self.page.set_content(_SELECT_DATE + markup)
                self.action.config.target_date = target
                self.assertFalse(
                    select_target_date(
                        self.page,
                        self.page.locator("#afternoon"),
                        self.action.config.target_date,
                        self.action.control.inbox.check_cancelled,
                    )
                )
                self.assertIsNone(
                    self.page.locator("body").get_attribute("data-date-clicks")
                )

    def test_click_without_selected_state_is_not_verified(self) -> None:
        self.page.set_content(
            pass_card("afternoon", (5, 6)).replace(
                'onclick="chooseDate(this)"', 'onclick="void 0"'
            )
        )
        self.assertFalse(
            select_target_date(
                self.page,
                self.page.locator("#afternoon"),
                self.action.config.target_date,
                self.action.control.inbox.check_cancelled,
            )
        )

    def test_waits_for_delayed_selected_state(self) -> None:
        self.page.set_content(
            _SELECT_DATE
            + pass_card("afternoon", (5, 6)).replace(
                'onclick="chooseDate(this)"',
                'onclick="setTimeout(() => chooseDate(this), 100)"',
            )
        )
        self.assertTrue(
            select_target_date(
                self.page,
                self.page.locator("#afternoon"),
                self.action.config.target_date,
                self.action.control.inbox.check_cancelled,
            )
        )

    def test_visible_locator_skips_hidden_first_match(self) -> None:
        self.page.set_content(
            '<div class="popup" style="display:none">Hidden</div><div class="popup" id="visible">Visible</div>'
        )
        locator = self.action._visible_locator((".popup",), timeout_ms=500)
        self.assertIsNotNone(locator)
        self.assertEqual(locator.get_attribute("id"), "visible")

    def test_cancellation_is_preserved_during_date_selection(self) -> None:
        self.page.set_content(_SELECT_DATE + pass_card("afternoon", (5, 6)))

        def cancelled() -> None:
            raise Cancelled("synthetic cancellation")

        self.action.control.inbox.check_cancelled = cancelled
        with self.assertRaises(Cancelled):
            select_target_date(
                self.page,
                self.page.locator("#afternoon"),
                self.action.config.target_date,
                self.action.control.inbox.check_cancelled,
            )

    def prepare_confirmation(self, markup: str) -> list[str]:
        self.page.set_content(markup)
        events = []
        self.action.config.allows_yodel_url = lambda value: value == "about:blank"

        def ready(*args):
            events.extend(("confirmation.starting", "confirmation.ready"))
            return "synthetic-confirmation"

        self.action.control.await_confirmation_ready = ready
        self.action.control.emit = lambda kind, **kwargs: events.append(kind)
        return events

    def test_confirmation_requires_fresh_response_even_when_dialog_looks_successful(
        self,
    ) -> None:
        events = self.prepare_confirmation("""
          <button onclick="document.body.dataset.clicked='yes';document.querySelector('#orderConfirmModal').style.display='block'">Yes</button>
          <div id="orderConfirmModal" style="display:none"><h2 class="heading">Confirmed</h2><a>See My Pass</a></div>
        """)
        with patch("buntzen_actions.checkout.CONFIRMATION_TIMEOUT_SECONDS", 0.2):
            with self.assertRaises(OutcomeUnknown):
                self.action._click_final_confirmation(
                    self.page.get_by_role("button", name="Yes"),
                    PASS_PREFERENCES["all_day"],
                )
        self.assertTrue(self.page.locator("#orderConfirmModal").is_visible())
        self.assertEqual(self.page.locator("body").get_attribute("data-clicked"), "yes")
        self.assertEqual(events, ["confirmation.starting", "confirmation.ready"])

    def test_stale_confirmation_dialog_prevents_the_click_and_durable_gate(
        self,
    ) -> None:
        events = self.prepare_confirmation("""
          <button onclick="document.body.dataset.clicked='yes'">Yes</button>
          <div id="orderConfirmModal"><h2 class="heading">Confirmed</h2><a>See My Pass</a></div>
        """)
        with self.assertRaises(ActionError) as failure:
            self.action._click_final_confirmation(
                self.page.get_by_role("button", name="Yes"), PASS_PREFERENCES["all_day"]
            )
        self.assertNotIsInstance(failure.exception, OutcomeUnknown)
        self.assertIsNone(self.page.locator("body").get_attribute("data-clicked"))
        self.assertEqual(events, [])

    def test_confirmation_timeout_never_emits_completed(self) -> None:
        events = self.prepare_confirmation(
            "<button onclick=\"document.body.dataset.clicked='yes'\">Yes</button>"
        )
        with patch("buntzen_actions.checkout.CONFIRMATION_TIMEOUT_SECONDS", 0.2):
            with self.assertRaises(OutcomeUnknown) as failure:
                self.action._click_final_confirmation(
                    self.page.get_by_role("button", name="Yes"),
                    PASS_PREFERENCES["all_day"],
                )
        self.assertIn("confirmation timeout", str(failure.exception.__cause__))
        self.assertEqual(events, ["confirmation.starting", "confirmation.ready"])

    def test_confirmation_cancel_after_click_remains_outcome_unknown(self) -> None:
        events = self.prepare_confirmation(
            "<button onclick=\"document.body.dataset.clicked='yes'\">Yes</button>"
        )

        def cancel_after_click():
            if self.page.locator("body").get_attribute("data-clicked"):
                raise Cancelled("synthetic cancellation after submission")

        self.action.control.inbox.check_cancelled = cancel_after_click
        with patch("buntzen_actions.checkout.CONFIRMATION_TIMEOUT_SECONDS", 0.2):
            with self.assertRaises(OutcomeUnknown) as failure:
                self.action._click_final_confirmation(
                    self.page.get_by_role("button", name="Yes"),
                    PASS_PREFERENCES["all_day"],
                )
        self.assertIsInstance(failure.exception.__cause__, Cancelled)
        self.assertEqual(events, ["confirmation.starting", "confirmation.ready"])

    def test_confirmation_cancel_before_click_never_enters_the_gate(self) -> None:
        events = self.prepare_confirmation(
            "<button onclick=\"document.body.dataset.clicked='yes'\">Yes</button>"
        )

        def cancelled():
            raise Cancelled("synthetic cancellation before submission")

        self.action.control.inbox.check_cancelled = cancelled
        with self.assertRaises(Cancelled):
            self.action._click_final_confirmation(
                self.page.get_by_role("button", name="Yes"), PASS_PREFERENCES["all_day"]
            )
        self.assertIsNone(self.page.locator("body").get_attribute("data-clicked"))
        self.assertEqual(events, [])

    def test_stalled_response_body_cannot_bypass_confirmation_timeout(self) -> None:
        events = self.prepare_confirmation("")
        with stalled_checkout_server() as (origin, headers_sent):
            self.page.goto(origin)
            self.action.config.allows_yodel_url = lambda value: value.startswith(
                origin + "/"
            )
            started = time.monotonic()
            with patch("buntzen_actions.checkout.CONFIRMATION_TIMEOUT_SECONDS", 0.2):
                with self.assertRaises(OutcomeUnknown) as failure:
                    self.action._click_final_confirmation(
                        self.page.get_by_role("button", name="Yes"),
                        PASS_PREFERENCES["all_day"],
                    )
            self.assertTrue(
                headers_sent.is_set(), "fixture never sent the checkout headers"
            )
            self.assertLess(
                time.monotonic() - started, 2.0, "stalled body blocked the deadline"
            )
            self.assertIn("confirmation timeout", str(failure.exception.__cause__))
            self.assertEqual(events, ["confirmation.starting", "confirmation.ready"])

    def test_stalled_response_body_does_not_block_cancellation(self) -> None:
        events = self.prepare_confirmation("")
        with stalled_checkout_server() as (origin, headers_sent):
            self.page.goto(origin)
            self.action.config.allows_yodel_url = lambda value: value.startswith(
                origin + "/"
            )

            def cancel_when_headers_arrive():
                if headers_sent.is_set():
                    raise Cancelled("synthetic cancellation while body is pending")

            self.action.control.inbox.check_cancelled = cancel_when_headers_arrive
            started = time.monotonic()
            with patch("buntzen_actions.checkout.CONFIRMATION_TIMEOUT_SECONDS", 0.2):
                with self.assertRaises(OutcomeUnknown) as failure:
                    self.action._click_final_confirmation(
                        self.page.get_by_role("button", name="Yes"),
                        PASS_PREFERENCES["all_day"],
                    )
            self.assertTrue(
                headers_sent.is_set(), "fixture never sent the checkout headers"
            )
            self.assertLess(
                time.monotonic() - started, 2.0, "stalled body blocked cancellation"
            )
            self.assertIsInstance(failure.exception.__cause__, Cancelled)
            self.assertEqual(events, ["confirmation.starting", "confirmation.ready"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
