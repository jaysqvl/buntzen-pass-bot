from __future__ import annotations

import unittest
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

from src.twilio_utils import TwilioService, extract_otp, latest_fresh_otp


@dataclass
class Message:
    body: str
    date_sent: datetime
    to: str = "+16045551212"
    direction: str = "inbound"
    sid: str = "SM123"
    from_: str = "+15550001111"


class OtpTests(unittest.TestCase):
    def test_extracts_common_otp_lengths(self) -> None:
        self.assertEqual(extract_otp("Your code is 1234."), "1234")
        self.assertEqual(extract_otp("Use 123456 to verify."), "123456")
        self.assertEqual(extract_otp("Yodel passcode: 12345678"), "12345678")

    def test_ignores_messages_before_request(self) -> None:
        requested = datetime.now(timezone.utc)
        messages = [Message("Code 111111", requested - timedelta(seconds=1))]
        self.assertIsNone(latest_fresh_otp(messages, requested_after=requested, to_number="+16045551212"))

    def test_returns_latest_fresh_inbound_code(self) -> None:
        requested = datetime.now(timezone.utc)
        older = Message("Code 111111", requested + timedelta(seconds=1), sid="older")
        newer = Message("Code 222222", requested + timedelta(seconds=2), sid="newer")
        match = latest_fresh_otp([older, newer], requested_after=requested, to_number="+16045551212")
        self.assertIsNotNone(match)
        self.assertEqual(match.code, "222222")
        self.assertEqual(match.sid, "newer")

    def test_ignores_wrong_destination_and_outbound(self) -> None:
        requested = datetime.now(timezone.utc)
        messages = [
            Message("Code 111111", requested + timedelta(seconds=1), to="+16045550000"),
            Message("Code 222222", requested + timedelta(seconds=2), direction="outbound-api"),
        ]
        self.assertIsNone(latest_fresh_otp(messages, requested_after=requested, to_number="+16045551212"))

    def test_twilio_service_polling_with_mocked_client(self) -> None:
        requested = datetime.now(timezone.utc)
        calls = []

        class Messages:
            def list(self, to, limit):
                calls.append((to, limit))
                return [Message("Your Yodel code is 333333", requested + timedelta(seconds=1))]

        class Client:
            messages = Messages()

        service = TwilioService(
            client=Client(),
            otp_number="+16045551212",
            alert_to_number=None,
            alerts_enabled=False,
            otp_timeout_seconds=1,
            otp_poll_interval_seconds=0.01,
        )
        match = service.wait_for_otp(requested_after=requested)
        self.assertEqual(match.code, "333333")
        self.assertEqual(calls, [("+16045551212", 20)])


if __name__ == "__main__":
    unittest.main()
