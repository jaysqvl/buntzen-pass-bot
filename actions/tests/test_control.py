from __future__ import annotations

import queue
import unittest
from datetime import datetime, timedelta, timezone
from unittest.mock import patch

from buntzen_actions.control import ControlPort
from buntzen_actions.errors import ActionError, ApprovalExpired, Cancelled, ProtocolError
from buntzen_actions.secrets import SecretRedactor


class FakeStream:
    def __init__(self) -> None:
        self.events = []

    def write(self, event_type, **payload) -> None:
        self.events.append((event_type, payload))


class FakeInbox:
    def __init__(self, frames) -> None:
        self.frames = list(frames)
        self.ignored = []

    def receive(self, expected, timeout=None, predicate=None):
        if not self.frames:
            raise queue.Empty
        frame = self.frames.pop(0)
        if predicate is not None and not predicate(frame):
            raise AssertionError("test supplied a mismatched frame")
        return frame

    def ignore_otp_challenge(self, challenge_id) -> None:
        self.ignored.append(challenge_id)


class ControlTests(unittest.TestCase):
    def test_otp_is_received_but_never_echoed(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox([])
        control = ControlPort(stream, inbox, SecretRedactor())

        # Freeze the correlation id by capturing it after otp.prepare.
        def receive(expected, timeout=None, predicate=None):
            challenge_id = stream.events[0][1]["challenge_id"]
            frame_type = "otp.ready" if len(stream.events) == 1 else "otp.provide"
            frame = {"v": 2, "type": frame_type, "challenge_id": challenge_id}
            if frame_type == "otp.provide":
                frame["code"] = "654321"
            self.assertTrue(predicate(frame))
            return frame

        inbox.receive = receive
        challenge_id = control.prepare_otp("login_submit")
        control.otp_triggered(challenge_id)
        value = control.wait_for_otp(challenge_id)
        self.assertEqual(value, "654321")
        control.otp_submitted(challenge_id)
        self.assertNotIn("654321", repr(stream.events))
        self.assertEqual(
            [event[0] for event in stream.events],
            ["otp.prepare", "otp.triggered", "otp.submitted"],
        )

    def test_provider_failure_closes_the_challenge_at_both_wait_stages(self) -> None:
        for stage in ("prepare", "receive"):
            for response_type in ("otp.expired", "otp.error"):
                with self.subTest(stage=stage, response_type=response_type):
                    stream = FakeStream()
                    inbox = FakeInbox([{
                        "v": 2, "type": response_type, "challenge_id": "challenge",
                    }])
                    control = ControlPort(stream, inbox, SecretRedactor())
                    with patch("buntzen_actions.control.uuid.uuid4") as identifier:
                        identifier.return_value.hex = "challenge"
                        with self.assertRaises(ActionError):
                            if stage == "prepare":
                                control.prepare_otp("resend")
                            else:
                                control.wait_for_otp("challenge")
                    self.assertEqual(stream.events[-1][0], "otp.failed")
                    self.assertEqual(inbox.ignored, ["challenge"])

    def test_otp_contract_accepts_only_four_to_eight_ascii_digits(self) -> None:
        for value in ("0000", "12345678", "123", "123456789", "１２３４", "١٢٣٤", "12a4", 1234):
            with self.subTest(value=value):
                stream = FakeStream()
                inbox = FakeInbox([{
                    "v": 2, "type": "otp.provide", "challenge_id": "challenge", "code": value,
                }])
                control = ControlPort(stream, inbox, SecretRedactor())
                if value in ("0000", "12345678"):
                    self.assertEqual(control.wait_for_otp("challenge"), value)
                else:
                    with self.assertRaisesRegex(ProtocolError, "ASCII digits"):
                        control.wait_for_otp("challenge")
                self.assertEqual(stream.events, [], "receiving a code must never echo it")

    def test_auth_deadline_stops_otp_wait_and_clears_challenge(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox([])
        control = ControlPort(stream, inbox, SecretRedactor())
        with self.assertRaises(ActionError):
            control.wait_for_otp(
                "challenge",
                deadline_at=datetime.now(timezone.utc) + timedelta(seconds=1),
            )
        self.assertEqual(stream.events[-1][0], "otp.failed")
        self.assertEqual(stream.events[-1][1]["reason"], "auth_deadline")
        self.assertEqual(inbox.ignored, ["challenge"])

    def test_past_auth_deadline_does_not_arm_provider(self) -> None:
        stream = FakeStream()
        control = ControlPort(stream, FakeInbox([]), SecretRedactor())
        with self.assertRaises(ActionError):
            control.prepare_otp(
                "login_submit",
                deadline_at=datetime.now(timezone.utc) - timedelta(seconds=1),
            )
        self.assertEqual(stream.events, [])

    def test_otp_arriving_after_deadline_is_not_returned(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox(
            [
                {
                    "v": 2,
                    "type": "otp.provide",
                    "challenge_id": "challenge",
                    "code": "123456",
                }
            ]
        )
        control = ControlPort(stream, inbox, SecretRedactor())
        with patch(
            "buntzen_actions.control._remaining_seconds", side_effect=[1.0, -0.01]
        ):
            with self.assertRaises(ActionError):
                control.wait_for_otp(
                    "challenge", deadline_at=datetime.now(timezone.utc)
                )
        self.assertNotIn("123456", repr(stream.events))
        self.assertEqual(stream.events[-1][1]["reason"], "auth_deadline")

    def test_manual_wait_has_no_deadline_and_can_cancel(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox([])
        calls = 0

        def receive(expected, timeout=None, predicate=None):
            nonlocal calls
            calls += 1
            if calls < 3:
                raise queue.Empty
            approval_id = stream.events[0][1]["approval_id"]
            return {"v": 2, "type": "approval.cancel", "approval_id": approval_id}

        inbox.receive = receive
        control = ControlPort(stream, inbox, SecretRedactor())
        with self.assertRaises(Cancelled):
            control.wait_for_approval(
                "all_day",
                "All-day",
                browser_is_ready=lambda: True,
                heartbeat_seconds=0,
                check_seconds=0,
            )
        self.assertIn("heartbeat", [event[0] for event in stream.events])
        self.assertEqual(stream.events[-1][0], "approval.cancelled")

    def test_confirmation_barrier_emits_id_and_waits_for_matching_ack(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox([])

        def receive(expected, timeout=None, predicate=None):
            confirmation_id = stream.events[0][1]["confirmation_id"]
            frame = {
                "v": 2,
                "type": "confirmation.ready",
                "confirmation_id": confirmation_id,
            }
            self.assertEqual(expected, {"confirmation.ready", "confirmation.error"})
            self.assertTrue(predicate(frame))
            return frame

        inbox.receive = receive
        control = ControlPort(stream, inbox, SecretRedactor())
        confirmation_id = control.await_confirmation_ready("all_day", "All-day")
        self.assertEqual(confirmation_id, stream.events[0][1]["confirmation_id"])
        self.assertEqual(len(confirmation_id), 32)
        self.assertEqual(stream.events[0][0], "confirmation.starting")

    def test_confirmation_error_or_missing_ack_never_returns_ready(self) -> None:
        for response in ("confirmation.error", None):
            with self.subTest(response=response):
                stream = FakeStream()
                inbox = FakeInbox([])
                if response is not None:

                    def receive(expected, timeout=None, predicate=None):
                        confirmation_id = stream.events[0][1]["confirmation_id"]
                        return {
                            "v": 2,
                            "type": response,
                            "confirmation_id": confirmation_id,
                        }

                    inbox.receive = receive
                control = ControlPort(stream, inbox, SecretRedactor())
                with self.assertRaises(ActionError):
                    control.await_confirmation_ready("all_day", "All-day")
                self.assertEqual(stream.events[0][0], "confirmation.starting")

    def test_manual_wait_expires_when_browser_is_no_longer_ready(self) -> None:
        stream = FakeStream()
        control = ControlPort(stream, FakeInbox([]), SecretRedactor())
        with self.assertRaises(ApprovalExpired):
            control.wait_for_approval(
                "all_day",
                "All-day",
                browser_is_ready=lambda: False,
                heartbeat_seconds=0,
                check_seconds=0,
            )
        self.assertEqual(stream.events[-1][0], "approval.expired")

    def test_credentials_v2_accepts_only_phone_and_redacts_it(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox([])

        def receive(expected, timeout=None, predicate=None):
            request_id = stream.events[0][1]["request_id"]
            frame = {
                "v": 2,
                "type": "credentials.provide",
                "request_id": request_id,
                "phone": "5559876543",
            }
            self.assertTrue(predicate(frame))
            return frame

        inbox.receive = receive
        redactor = SecretRedactor()
        credentials = ControlPort(stream, inbox, redactor).request_credentials()
        self.assertEqual(credentials.phone, "5559876543")
        self.assertNotIn("5559876543", repr(stream.events))
        self.assertEqual(redactor.redact("dial 5559876543"), "dial [REDACTED]")

    def test_credentials_v2_rejects_non_string_phone(self) -> None:
        stream = FakeStream()
        inbox = FakeInbox([])
        inbox.receive = lambda expected, timeout=None, predicate=None: {
            "v": 2,
            "type": "credentials.provide",
            "request_id": stream.events[0][1]["request_id"],
            "phone": 5559876543,
        }
        with self.assertRaises(ProtocolError):
            ControlPort(stream, inbox, SecretRedactor()).request_credentials()


if __name__ == "__main__":
    unittest.main()
