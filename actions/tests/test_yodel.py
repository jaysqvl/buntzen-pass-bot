from __future__ import annotations

import unittest
from datetime import date, datetime, timedelta, timezone
from types import SimpleNamespace
from unittest.mock import Mock, patch
from urllib.parse import urlparse

from buntzen_actions.control import Credentials
from buntzen_actions.errors import ActionError, Cancelled, OutcomeUnknown, ProtocolError
from buntzen_actions.pass_types import PASS_PREFERENCES
from buntzen_actions.yodel import (
    BookingResult,
    LOGIN_PHONE_SELECTORS,
    LOGIN_SUBMIT_SELECTORS,
    OTP_INPUT_SELECTORS,
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
        return Credentials("5559876543")

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


def allows_example_origin(value: str) -> bool:
    parsed = urlparse(value)
    return parsed.scheme == "https" and parsed.netloc == "example.test"


def configure_example_origin(action) -> None:
    action.config = SimpleNamespace(allows_yodel_url=allows_example_origin)
    action.page = SimpleNamespace(url="https://example.test/login")


class LoginAction(YodelAction):
    def __init__(self, events, submit, next_state="otp") -> None:
        self.events = events
        self.control = FakeControl(events)
        self.page = SimpleNamespace(url="https://example.test/login")
        self.config = SimpleNamespace(allows_yodel_url=allows_example_origin)
        self.next_state = next_state
        self._phone = Locator(events, "phone")
        self._submit = submit

    def _visible_locator(self, selectors, root=None, timeout_ms=1000):
        if selectors is LOGIN_PHONE_SELECTORS:
            return self._phone
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
        self.url = "https://example.test/login"

    def goto(self, url, wait_until):
        self.events.append(("goto", url))
        self.url = url

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
        self.config = SimpleNamespace(
            login_probe_url="https://example.test",
            allows_yodel_url=allows_example_origin,
        )
        self.authenticated = authenticated

    def _settle_page(self, timeout_ms=15_000):
        return None

    def _dismiss_public_notices(self):
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
        self.url = "https://example.test"

    def goto(self, url, wait_until):
        self.events.append(("goto", url))
        self.url = url


class BookingControl(FakeControl):
    def wait_for_approval(self, **kwargs):
        self.events.append(("approval.wait", kwargs["pass_key"]))


class BookingAction(YodelAction):
    def __init__(self, events) -> None:
        self.events = events
        self.page = BookingPage(events)
        self.control = BookingControl(events)
        self.diagnostics = SimpleNamespace(
            suspend_trace=lambda: events.append(("trace", "suspended")),
            authenticated=lambda: events.append(("trace", "resumed")),
        )
        self.config = SimpleNamespace(
            target_date=date(2030, 1, 15),
            vehicle_keyword="Example Vehicle",
            all_day_pass_url="https://example.test/all",
            half_day_pass_url="https://example.test/half",
            allows_yodel_url=allows_example_origin,
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


class ReleaseDiagnostics:
    def __init__(self, events, suspend_error=None) -> None:
        self.events = events
        self.suspend_error = suspend_error

    def suspend_trace(self):
        self.events.append(("trace", "suspended"))
        if self.suspend_error is not None:
            raise self.suspend_error

    def authenticated(self):
        self.events.append(("trace", "resumed"))


class ReleaseInbox:
    def __init__(self, events, failure=None) -> None:
        self.events = events
        self.failure = failure

    def check_cancelled(self):
        self.events.append(("cancel", "checked"))
        if self.failure is not None:
            raise self.failure


class ReleaseControl(FakeControl):
    def __init__(self, events, cancellation=None) -> None:
        super().__init__(events)
        self.inbox = ReleaseInbox(events, cancellation)

    def heartbeat(self, phase):
        self.events.append(("heartbeat", phase))


class ReleaseAction(YodelAction):
    def __init__(self, events, release_at, keepalive_error=None, suspend_error=None) -> None:
        self.events = events
        self.diagnostics = ReleaseDiagnostics(events, suspend_error)
        self.control = ReleaseControl(events)
        self.config = SimpleNamespace(
            release_at=release_at,
            auth_deadline_at=release_at - timedelta(minutes=5),
        )
        self.keepalive_error = keepalive_error

    def keep_session_warm(self, auth_deadline_at=None):
        self.events.append(("keepalive", "started"))
        # A successful re-authentication would resume tracing. The release
        # wait must immediately suspend it again.
        self.diagnostics.authenticated()
        if self.keepalive_error is not None:
            raise self.keepalive_error

    def _is_authenticated(self):
        self.events.append(("auth", "checked"))
        return True


class YodelTests(unittest.TestCase):
    def test_visible_mobile_number_input_is_not_an_otp_challenge(self) -> None:
        class TestLocator:
            def __init__(self, visible: bool) -> None:
                self.first = self
                self.visible = visible

            def wait_for(self, **kwargs) -> None:
                if not self.visible:
                    raise RuntimeError("not visible")

        class PhoneOnlyPage:
            def locator(self, selector):
                return TestLocator(selector in LOGIN_PHONE_SELECTORS)

        action = object.__new__(YodelAction)
        action.page = PhoneOnlyPage()
        self.assertTrue(action._has_login_form())
        self.assertFalse(action._has_otp_challenge())
        self.assertNotIn("input[inputmode='numeric']", OTP_INPUT_SELECTORS)

    def test_auth_does_not_navigate_or_request_credentials_when_trace_stop_fails(self) -> None:
        events = []
        action = DeadlineAction(events)
        action.diagnostics.pause_for_auth = lambda: (_ for _ in ()).throw(
            ActionError("synthetic trace stop failure")
        )

        with self.assertRaises(ActionError):
            action.ensure_authenticated()

        self.assertNotIn("goto", [event[0] for event in events])
        self.assertNotIn("credentials.request", [event[0] for event in events])

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
        self.assertNotIn("5559876543", repr(events))

    def test_credentials_are_not_requested_after_cross_origin_navigation(self) -> None:
        events = []
        action = LoginAction(events, Locator(events, "login"))
        action.page.url = "https://attacker.example/login"
        with self.assertRaises(ActionError):
            action._complete_login_form()
        self.assertNotIn("credentials.request", [event[0] for event in events])

    def test_goto_rejects_cross_origin_redirect_target(self) -> None:
        events = []

        class RedirectPage:
            url = "about:blank"

            def goto(self, url, wait_until):
                events.append(("goto", url))
                self.url = "https://attacker.example/redirected"

        action = object.__new__(YodelAction)
        action.page = RedirectPage()
        action.config = SimpleNamespace(allows_yodel_url=allows_example_origin)
        with self.assertRaises(ActionError):
            action._goto_allowed("https://example.test/login")
        self.assertEqual(events, [("goto", "https://example.test/login")])

    def test_navigation_guard_aborts_cross_origin_top_level_request(self) -> None:
        events = []
        frame = object()
        action = object.__new__(YodelAction)
        action.page = SimpleNamespace(main_frame=frame)
        action.config = SimpleNamespace(allows_yodel_url=allows_example_origin)
        route = SimpleNamespace(
            abort=lambda reason: events.append(("abort", reason)),
            continue_=lambda: events.append(("continue", None)),
        )
        request = SimpleNamespace(
            is_navigation_request=lambda: True,
            frame=frame,
            url="https://attacker.example/redirected",
        )
        action._guard_navigation(route, request)
        self.assertEqual(events, [("abort", "blockedbyclient")])

    def test_failed_login_click_reports_trigger_failure(self) -> None:
        events = []
        action = LoginAction(events, Locator(events, "login", fails=True))
        with self.assertRaises(ActionError):
            action._complete_login_form()
        self.assertIn(("otp.failed", "trigger_failed"), events)

    def test_mobile_login_fails_closed_without_an_otp_challenge(self) -> None:
        events = []
        action = LoginAction(events, Locator(events, "next"), next_state="unknown")
        with self.assertRaises(ActionError):
            action._complete_login_form()
        self.assertIn(("otp.failed", "challenge_not_visible"), events)
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

    def test_future_release_wait_suspends_reauth_trace_until_release(self) -> None:
        events = []
        release_at = datetime(2030, 1, 14, 7, 0, tzinfo=timezone.utc)
        before_release = release_at - timedelta(hours=1)
        action = ReleaseAction(events, release_at)
        clock = SimpleNamespace(
            now=Mock(
                side_effect=[
                    before_release,
                    before_release,
                    before_release,
                    release_at,
                ]
            )
        )

        with patch("buntzen_actions.yodel.datetime", clock), patch(
            "buntzen_actions.yodel.time.monotonic", return_value=1.0
        ), patch("buntzen_actions.yodel.time.sleep"):
            action.wait_for_release_if_needed()

        trace_events = [event for event in events if event[0] == "trace"]
        self.assertEqual(
            trace_events,
            [
                ("trace", "suspended"),
                ("trace", "resumed"),
                ("trace", "suspended"),
                ("trace", "resumed"),
            ],
        )
        self.assertLess(
            events.index(("trace", "suspended"), 1),
            events.index(("auth", "checked")),
        )
        self.assertLess(
            events.index(("trace", "resumed"), events.index(("auth", "checked"))),
            events.index(("release.ready", {})),
        )

    def test_release_wait_does_not_begin_when_trace_stop_fails(self) -> None:
        events = []
        release_at = datetime(2030, 1, 14, 7, 0, tzinfo=timezone.utc)
        before_release = release_at - timedelta(hours=1)
        action = ReleaseAction(
            events,
            release_at,
            suspend_error=ActionError("synthetic trace stop failure"),
        )

        with patch(
            "buntzen_actions.yodel.datetime",
            SimpleNamespace(now=Mock(return_value=before_release)),
        ), self.assertRaises(ActionError):
            action.wait_for_release_if_needed()

        self.assertEqual(events, [("trace", "suspended")])

    def test_release_wait_cancellation_never_resumes_trace(self) -> None:
        events = []
        release_at = datetime(2030, 1, 14, 7, 0, tzinfo=timezone.utc)
        before_release = release_at - timedelta(hours=1)
        action = ReleaseAction(events, release_at)
        action.control.inbox.failure = Cancelled("cancelled during release wait")
        clock = SimpleNamespace(now=Mock(side_effect=[before_release, before_release]))

        with patch("buntzen_actions.yodel.datetime", clock), self.assertRaises(
            Cancelled
        ):
            action.wait_for_release_if_needed()

        self.assertEqual(
            [event for event in events if event[0] == "trace"],
            [("trace", "suspended")],
        )
        self.assertNotIn(("release.ready", {}), events)

    def test_release_wait_error_resuspends_trace_and_does_not_resume(self) -> None:
        events = []
        release_at = datetime(2030, 1, 14, 7, 0, tzinfo=timezone.utc)
        before_release = release_at - timedelta(hours=1)
        action = ReleaseAction(events, release_at, ActionError("keepalive failed"))
        clock = SimpleNamespace(now=Mock(side_effect=[before_release, before_release]))

        with patch("buntzen_actions.yodel.datetime", clock), patch(
            "buntzen_actions.yodel.time.monotonic", return_value=1.0
        ), self.assertRaises(ActionError):
            action.wait_for_release_if_needed()

        self.assertEqual(
            [event for event in events if event[0] == "trace"],
            [
                ("trace", "suspended"),
                ("trace", "resumed"),
                ("trace", "suspended"),
            ],
        )
        self.assertNotIn(("release.ready", {}), events)

    def test_release_gate_resuspends_final_reauthentication_before_resume(self) -> None:
        events = []
        release_at = datetime(2030, 1, 14, 7, 0, tzinfo=timezone.utc)
        before_release = release_at - timedelta(hours=1)
        action = ReleaseAction(events, release_at)
        action.config.auth_deadline_at = None
        action._is_authenticated = lambda: events.append(
            ("auth", "checked")
        ) or False

        def reauthenticate(auth_deadline_at=None):
            events.append(("reauth", "started"))
            action.diagnostics.authenticated()
            return True

        action.ensure_authenticated = reauthenticate
        clock = SimpleNamespace(now=Mock(side_effect=[before_release, release_at]))

        with patch("buntzen_actions.yodel.datetime", clock):
            action.wait_for_release_if_needed()

        trace_events = [event for event in events if event[0] == "trace"]
        self.assertEqual(
            trace_events,
            [
                ("trace", "suspended"),
                ("trace", "resumed"),
                ("trace", "suspended"),
                ("trace", "resumed"),
            ],
        )
        reauth_resume = events.index(
            ("trace", "resumed"), events.index(("reauth", "started"))
        )
        resuspended = events.index(("trace", "suspended"), reauth_resume)
        final_resume = events.index(("trace", "resumed"), resuspended)
        self.assertLess(reauth_resume, resuspended)
        self.assertLess(resuspended, final_resume)
        self.assertIn(("release.ready", {}), events)

    def test_final_click_is_bracketed(self) -> None:
        events = []
        action = object.__new__(YodelAction)
        action.control = FakeControl(events)
        configure_example_origin(action)
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
                configure_example_origin(action)
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
        configure_example_origin(action)
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
        self.assertLess(
            manual_events.index(("trace", "suspended")),
            manual_events.index(("approval.wait", "all_day")),
        )
        self.assertLess(
            manual_events.index(("approval.wait", "all_day")),
            manual_events.index(("trace", "resumed")),
        )
        self.assertLess(
            manual_events.index(("trace", "resumed")),
            manual_events.index(("confirmation.click", "all_day")),
        )

        auto_events = []
        automatic = BookingAction(auto_events)
        self.assertTrue(
            automatic._try_pass(PASS_PREFERENCES["all_day"], mode="auto").success
        )
        auto_names = [event[0] for event in auto_events]
        self.assertNotIn("approval.wait", auto_names)
        self.assertNotIn("trace", auto_names)
        self.assertIn("confirmation.click", auto_names)

    def test_manual_wait_does_not_begin_when_trace_stop_fails(self) -> None:
        events = []
        action = BookingAction(events)

        def fail_trace_stop():
            events.append(("trace", "suspended"))
            raise ActionError("synthetic trace stop failure")

        action.diagnostics.suspend_trace = fail_trace_stop
        with self.assertRaises(ActionError):
            action._try_pass(PASS_PREFERENCES["all_day"], mode="manual")

        names = [event[0] for event in events]
        self.assertNotIn("approval.wait", names)
        self.assertNotIn("confirmation.click", names)


if __name__ == "__main__":
    unittest.main()
