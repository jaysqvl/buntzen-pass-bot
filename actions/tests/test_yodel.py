from __future__ import annotations

import unittest
from datetime import date, datetime, timedelta, timezone
from types import SimpleNamespace
from unittest.mock import patch

from buntzen_actions.control import Credentials
from buntzen_actions.errors import ActionError, Cancelled, OutcomeUnknown, ProtocolError
from buntzen_actions.pass_types import PASS_PREFERENCES
from buntzen_actions.yodel import (
    BookingResult,
    LOGIN_EMAIL_SELECTORS,
    LOGIN_PASSWORD_SELECTORS,
    LOGIN_SUBMIT_SELECTORS,
    YodelAction,
)


class Locator:
    def __init__(self, events, name, fails=False) -> None:
        self.events = events
        self.name = name
        self.fails = fails

    def fill(self, value) -> None:
        self.events.append(("fill", self.name))

    def click(self, **kwargs) -> None:
        self.events.append(("click", self.name))
        if self.fails:
            raise RuntimeError("ambiguous click failure")


class Inbox:
    def check_cancelled(self) -> None:
        return None


class FakeControl:
    def __init__(self, events) -> None:
        self.events = events
        self.inbox = Inbox()

    def request_credentials(self):
        self.events.append(("credentials.request", None))
        return Credentials("person@example.test", "private-password")

    def prepare_otp(self, trigger, deadline_at=None):
        self.events.append(("otp.prepare", trigger))
        return "challenge"

    def otp_triggered(self, challenge_id):
        self.events.append(("otp.triggered", challenge_id))

    def otp_failed(self, challenge_id, reason):
        self.events.append(("otp.failed", reason))

    def await_confirmation_ready(self, pass_key, label):
        confirmation_id = "confirmation"
        self.events.append(
            (
                "confirmation.starting",
                {
                    "confirmation_id": confirmation_id,
                    "pass_key": pass_key,
                    "label": label,
                },
            )
        )
        self.events.append(("confirmation.ready", confirmation_id))
        return confirmation_id

    def emit(self, event_type, **payload):
        self.events.append((event_type, payload))

    def status(self, phase, message):
        self.events.append(("status", phase))


class LoginAction(YodelAction):
    def __init__(self, events, submit, next_state="otp") -> None:
        self.events = events
        self.control = FakeControl(events)
        self.page = object()
        self.next_state = next_state
        self._email = Locator(events, "email")
        self._password = Locator(events, "password")
        self._submit = submit

    def _visible_locator(self, selectors, root=None, timeout_ms=1000):
        if selectors is LOGIN_EMAIL_SELECTORS:
            return self._email
        if selectors is LOGIN_PASSWORD_SELECTORS:
            return self._password
        if selectors is LOGIN_SUBMIT_SELECTORS:
            return self._submit
        return None

    def _human_pause(self, minimum=0, maximum=0):
        return None

    def _wait_for_auth_or_otp(self, timeout_seconds=15, auth_deadline_at=None):
        return self.next_state

    def _fill_and_submit_otp(self, challenge_id, auth_deadline_at=None):
        self.events.append(("otp.fill", challenge_id))


class DeadlinePage:
    def __init__(self, events) -> None:
        self.events = events

    def goto(self, url, wait_until):
        self.events.append(("goto", url))

    def evaluate(self, expression):
        self.events.append(("evaluate", None))


class DeadlineDiagnostics:
    def __init__(self, events) -> None:
        self.events = events

    def pause_for_auth(self):
        self.events.append(("diagnostics", "paused"))

    def authenticated(self):
        self.events.append(("diagnostics", "authenticated"))


class DeadlineAction(YodelAction):
    def __init__(self, events, authenticated=False) -> None:
        self.events = events
        self.page = DeadlinePage(events)
        self.control = FakeControl(events)
        self.diagnostics = DeadlineDiagnostics(events)
        self.config = SimpleNamespace(login_probe_url="https://example.test")
        self.authenticated = authenticated

    def _settle_page(self, timeout_ms=15_000):
        return None

    def _is_authenticated(self):
        return self.authenticated

    def _has_otp_challenge(self, timeout_ms=500):
        raise AssertionError("OTP challenge must not be inspected after the deadline")

    def _has_login_form(self):
        raise AssertionError("login form must not be used after the deadline")


class BookingPage:
    def __init__(self, events) -> None:
        self.events = events

    def goto(self, url, wait_until):
        self.events.append(("goto", url))


class BookingControl(FakeControl):
    def wait_for_approval(self, **kwargs):
        self.events.append(("approval.wait", kwargs["pass_key"]))


class BookingAction(YodelAction):
    def __init__(self, events) -> None:
        self.events = events
        self.page = BookingPage(events)
        self.control = BookingControl(events)
        self.config = SimpleNamespace(
            target_date=date(2030, 1, 15),
            vehicle_keyword="Example Vehicle",
            all_day_pass_url="https://example.test/all",
            half_day_pass_url="https://example.test/half",
        )
        self.container = object()
        self.final_confirm = object()

    def _settle_page(self, timeout_ms=15_000):
        self.events.append(("settle", timeout_ms))

    def _select_target_date(self):
        self.events.append(("date.select", None))
        return True

    def _find_pass_container(self, preference):
        self.events.append(("pass.find", preference.key))
        return self.container

    def _pass_is_available(self, container):
        self.events.append(("pass.available", container is self.container))
        return True

    def _select_vehicle(self, container):
        self.events.append(("vehicle.select", self.config.vehicle_keyword))
        return True

    def _click_first(self, root, selectors, timeout_ms):
        self.events.append(("checkout.click", timeout_ms))
        return True

    def _visible_locator(self, selectors, root=None, timeout_ms=1_000):
        self.events.append(("confirmation.find", timeout_ms))
        return self.final_confirm

    def _click_final_confirmation(self, locator, preference):
        self.events.append(("confirmation.click", preference.key))

    def _capture_failure(self, name):
        self.events.append(("diagnostic", name))


class YodelTests(unittest.TestCase):
    def test_login_arms_provider_before_clicking(self) -> None:
        events = []
        submit = Locator(events, "login")
        action = LoginAction(events, submit)
        action._complete_login_form()
        names = [
            event[0] if event[0] != "click" else f"click.{event[1]}" for event in events
        ]
        self.assertLess(names.index("otp.prepare"), names.index("click.login"))
        self.assertLess(names.index("click.login"), names.index("otp.triggered"))
        self.assertLess(names.index("otp.triggered"), names.index("otp.fill"))
        self.assertNotIn("private-password", repr(events))

    def test_failed_login_click_reports_trigger_failure(self) -> None:
        events = []
        action = LoginAction(events, Locator(events, "login", fails=True))
        with self.assertRaises(ActionError):
            action._complete_login_form()
        self.assertIn(("otp.failed", "trigger_failed"), events)

    def test_split_login_screen_does_not_wait_for_an_otp(self) -> None:
        events = []
        action = LoginAction(events, Locator(events, "continue"), next_state="login")
        action.control.otp_not_required = lambda challenge_id: events.append(
            ("otp.not_required", challenge_id)
        )
        action._complete_login_form()
        self.assertIn(("otp.not_required", "challenge"), events)
        self.assertNotIn("otp.fill", [event[0] for event in events])

    def test_past_deadline_accepts_existing_session_without_starting_mfa(self) -> None:
        events = []
        action = DeadlineAction(events, authenticated=True)
        deadline = datetime.now(timezone.utc) - timedelta(seconds=1)
        self.assertTrue(action.ensure_authenticated(auth_deadline_at=deadline))
        self.assertNotIn("otp.prepare", [event[0] for event in events])
        self.assertIn(("diagnostics", "authenticated"), events)

    def test_past_deadline_rejects_logged_out_session_without_starting_mfa(
        self,
    ) -> None:
        events = []
        action = DeadlineAction(events, authenticated=False)
        deadline = datetime.now(timezone.utc) - timedelta(seconds=1)
        self.assertFalse(action.ensure_authenticated(auth_deadline_at=deadline))
        self.assertNotIn("otp.prepare", [event[0] for event in events])
        self.assertIn(("status", "auth_deadline"), events)

    def test_keepalive_logout_after_deadline_does_not_reauthenticate(self) -> None:
        events = []
        action = DeadlineAction(events, authenticated=False)
        deadline = datetime.now(timezone.utc) - timedelta(seconds=1)
        action.ensure_authenticated = lambda **kwargs: (_ for _ in ()).throw(
            AssertionError("reauthentication must not start after the deadline")
        )
        with patch("buntzen_actions.yodel.random.random", return_value=1.0):
            with self.assertRaises(ActionError):
                action.keep_session_warm(auth_deadline_at=deadline)
        self.assertNotIn("otp.prepare", [event[0] for event in events])
        self.assertIn(("status", "auth_expired"), events)

    def test_final_click_is_bracketed(self) -> None:
        events = []
        action = object.__new__(YodelAction)
        action.control = FakeControl(events)
        locator = Locator(events, "confirm")
        action._click_final_confirmation(locator, PASS_PREFERENCES["all_day"])
        self.assertEqual(events[0][0], "confirmation.starting")
        self.assertEqual(events[1], ("confirmation.ready", "confirmation"))
        self.assertEqual(events[2], ("click", "confirm"))
        self.assertEqual(events[3][0], "confirmation.completed")
        self.assertEqual(events[3][1]["confirmation_id"], "confirmation")

    def test_final_click_does_not_run_without_confirmation_ack(self) -> None:
        failures = (
            Cancelled("stream closed before ack"),
            ProtocolError("wrong or mismatched ack"),
            ActionError("durable marker failed"),
        )
        for failure in failures:
            with self.subTest(failure=str(failure)):
                events = []
                action = object.__new__(YodelAction)
                control = FakeControl(events)

                def reject_confirmation(pass_key, label):
                    events.append(("confirmation.starting", pass_key))
                    raise failure

                control.await_confirmation_ready = reject_confirmation
                action.control = control
                with self.assertRaises(ActionError):
                    action._click_final_confirmation(
                        Locator(events, "confirm"),
                        PASS_PREFERENCES["all_day"],
                    )
                self.assertNotIn(("click", "confirm"), events)

    def test_final_click_failure_is_outcome_unknown(self) -> None:
        events = []
        action = object.__new__(YodelAction)
        action.control = FakeControl(events)
        with self.assertRaises(OutcomeUnknown):
            action._click_final_confirmation(
                Locator(events, "confirm", fails=True),
                PASS_PREFERENCES["all_day"],
            )
        self.assertEqual(events[0][0], "confirmation.starting")
        self.assertEqual(events[1][0], "confirmation.ready")
        self.assertNotIn("confirmation.completed", [event[0] for event in events])

    def test_passes_are_tried_in_configured_order(self) -> None:
        events = []
        action = object.__new__(YodelAction)
        action.config = SimpleNamespace(pass_order=("all_day", "afternoon", "morning"))
        action.control = SimpleNamespace(inbox=Inbox())

        def try_pass(preference, mode):
            events.append(preference.key)
            return BookingResult(
                preference.key == "morning", preference.key, preference.key
            )

        action._try_pass = try_pass
        result = action.try_booking_once("auto")
        self.assertTrue(result.success)
        self.assertEqual(events, ["all_day", "afternoon", "morning"])

    def test_dry_run_stops_after_date_pass_and_vehicle_selection(self) -> None:
        events = []
        action = BookingAction(events)
        result = action._try_pass(PASS_PREFERENCES["all_day"], mode="dry-run")
        self.assertTrue(result.success)
        names = [event[0] for event in events]
        self.assertIn("date.select", names)
        self.assertIn("pass.available", names)
        self.assertIn("vehicle.select", names)
        self.assertNotIn("checkout.click", names)
        self.assertNotIn("confirmation.click", names)

    def test_manual_pauses_before_confirmation_but_auto_does_not(self) -> None:
        manual_events = []
        manual = BookingAction(manual_events)
        self.assertTrue(
            manual._try_pass(PASS_PREFERENCES["all_day"], mode="manual").success
        )
        manual_names = [event[0] for event in manual_events]
        self.assertLess(
            manual_names.index("approval.wait"),
            manual_names.index("confirmation.click"),
        )

        auto_events = []
        automatic = BookingAction(auto_events)
        self.assertTrue(
            automatic._try_pass(PASS_PREFERENCES["all_day"], mode="auto").success
        )
        auto_names = [event[0] for event in auto_events]
        self.assertNotIn("approval.wait", auto_names)
        self.assertIn("confirmation.click", auto_names)


if __name__ == "__main__":
    unittest.main()
