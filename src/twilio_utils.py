from __future__ import annotations

import logging
import re
import time
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Iterable


logger = logging.getLogger("buntzen_pass_bot.twilio")
OTP_RE = re.compile(r"(?<!\d)(\d{4,8})(?!\d)")


@dataclass(frozen=True)
class OtpMessage:
    code: str
    body: str
    from_number: str | None
    date_sent: datetime | None
    sid: str | None


class TwilioService:
    def __init__(
        self,
        client: Any,
        otp_number: str,
        alert_to_number: str | None,
        alerts_enabled: bool,
        otp_timeout_seconds: int,
        otp_poll_interval_seconds: float,
    ) -> None:
        self.client = client
        self.otp_number = otp_number
        self.alert_to_number = alert_to_number
        self.alerts_enabled = alerts_enabled
        self.otp_timeout_seconds = otp_timeout_seconds
        self.otp_poll_interval_seconds = otp_poll_interval_seconds

    @classmethod
    def from_config(cls, config) -> "TwilioService":
        try:
            from twilio.rest import Client
        except ImportError as exc:
            raise RuntimeError("Install dependencies first: uv sync") from exc
        client = Client(config.twilio_account_sid, config.twilio_auth_token)
        return cls(
            client=client,
            otp_number=config.twilio_otp_number,
            alert_to_number=config.twilio_alert_to_number,
            alerts_enabled=config.twilio_alerts_enabled,
            otp_timeout_seconds=config.otp_timeout_seconds,
            otp_poll_interval_seconds=config.otp_poll_interval_seconds,
        )

    def send_alert(self, message: str, urgent: bool = False) -> None:
        if not self.alerts_enabled or not self.alert_to_number:
            return
        prefix = "URGENT: " if urgent else ""
        body = f"{prefix}{message}"
        try:
            self.client.messages.create(
                from_=self.otp_number,
                to=self.alert_to_number,
                body=body[:1500],
            )
        except Exception as exc:
            logger.warning("Could not send Twilio alert: %s", exc)

    def wait_for_otp(self, requested_after: datetime, timeout_seconds: int | None = None) -> OtpMessage:
        deadline = time.monotonic() + (timeout_seconds or self.otp_timeout_seconds)
        while time.monotonic() < deadline:
            match = self.latest_fresh_otp(requested_after=requested_after)
            if match:
                return match
            time.sleep(self.otp_poll_interval_seconds)
        raise TimeoutError("Timed out waiting for Twilio OTP SMS.")

    def latest_fresh_otp(self, requested_after: datetime) -> OtpMessage | None:
        messages = self._list_recent_inbound_messages()
        return latest_fresh_otp(messages, requested_after=requested_after, to_number=self.otp_number)

    def _list_recent_inbound_messages(self) -> Iterable[Any]:
        try:
            return self.client.messages.list(to=self.otp_number, limit=20)
        except Exception as exc:
            logger.warning("Could not read Twilio messages: %s", exc)
            return []


def extract_otp(body: str) -> str | None:
    matches = OTP_RE.findall(body or "")
    if not matches:
        return None
    return matches[0]


def latest_fresh_otp(
    messages: Iterable[Any],
    requested_after: datetime,
    to_number: str,
) -> OtpMessage | None:
    requested_after = _ensure_aware_utc(requested_after)
    newest: OtpMessage | None = None
    newest_date = datetime.min.replace(tzinfo=timezone.utc)

    for message in messages:
        direction = str(getattr(message, "direction", "") or "").lower()
        message_to = str(getattr(message, "to", "") or "")
        if direction and "inbound" not in direction:
            continue
        if message_to and _normalize_phone(message_to) != _normalize_phone(to_number):
            continue

        date_sent = _coerce_datetime(getattr(message, "date_sent", None))
        if date_sent and date_sent < requested_after:
            continue

        code = extract_otp(str(getattr(message, "body", "") or ""))
        if not code:
            continue

        effective_date = date_sent or requested_after - timedelta(seconds=1)
        if effective_date >= newest_date:
            newest_date = effective_date
            newest = OtpMessage(
                code=code,
                body=str(getattr(message, "body", "") or ""),
                from_number=getattr(message, "from_", None) or getattr(message, "from_number", None),
                date_sent=date_sent,
                sid=getattr(message, "sid", None),
            )
    return newest


def _coerce_datetime(value: Any) -> datetime | None:
    if value is None:
        return None
    if isinstance(value, datetime):
        return _ensure_aware_utc(value)
    return None


def _ensure_aware_utc(value: datetime) -> datetime:
    if value.tzinfo is None:
        return value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc)


def _normalize_phone(value: str) -> str:
    return "".join(ch for ch in value if ch.isdigit())
