from __future__ import annotations

import queue
import re
import time
import uuid
from dataclasses import dataclass
from datetime import datetime
from typing import Callable, Optional

from .errors import ActionError, ApprovalExpired, Cancelled, ProtocolError
from .protocol import ControlInbox, JsonLineStream
from .secrets import SecretRedactor


_OTP_PATTERN = re.compile(r"[0-9]{4,8}")


@dataclass
class Credentials:
    phone: Optional[str]

    def clear(self) -> None:
        self.phone = None


class ControlPort:
    """High-level, correlated request/reply operations over the JSONL stream."""

    def __init__(
        self, stream: JsonLineStream, inbox: ControlInbox, redactor: SecretRedactor
    ) -> None:
        self.stream = stream
        self.inbox = inbox
        self.redactor = redactor

    def emit(self, event_type: str, **payload: object) -> None:
        self.stream.write(event_type, **payload)

    def status(self, phase: str, message: str) -> None:
        self.emit("run.status", phase=phase, message=self.redactor.redact(message))

    def heartbeat(self, phase: str) -> None:
        self.emit("heartbeat", phase=phase)

    def request_credentials(self) -> Credentials:
        request_id = uuid.uuid4().hex
        self.emit("credentials.request", request_id=request_id)
        frame = self.inbox.receive(
            {"credentials.provide"},
            predicate=lambda item: item.get("request_id") == request_id,
        )
        phone = frame.get("phone")
        if phone is not None and not isinstance(phone, str):
            raise ProtocolError("credential phone must be a string or null")
        self.redactor.add(phone)
        return Credentials(phone=phone or None)

    def prepare_otp(self, trigger: str, deadline_at: Optional[datetime] = None) -> str:
        timeout = _remaining_seconds(deadline_at)
        if timeout is not None and timeout <= 0:
            raise ActionError(
                "Authentication deadline passed before a new OTP could be requested"
            )
        challenge_id = uuid.uuid4().hex
        self.emit("otp.prepare", challenge_id=challenge_id, trigger=trigger)
        try:
            frame = self.inbox.receive(
                {"otp.ready", "otp.error", "otp.expired"},
                timeout=timeout,
                predicate=lambda item: item.get("challenge_id") == challenge_id,
            )
        except queue.Empty as exc:
            self.otp_failed(challenge_id, "auth_deadline")
            raise ActionError(
                "Authentication deadline passed while arming the OTP provider"
            ) from exc
        if deadline_at is not None and _remaining_seconds(deadline_at) <= 0:
            self.otp_failed(challenge_id, "auth_deadline")
            raise ActionError(
                "Authentication deadline passed while arming the OTP provider"
            )
        if frame["type"] == "otp.expired":
            self.otp_failed(challenge_id, "expired_before_trigger")
            raise ActionError(
                "OTP provider expired before the login action was triggered"
            )
        if frame["type"] == "otp.error":
            self.otp_failed(challenge_id, "provider_error_before_trigger")
            raise ActionError("OTP provider could not arm for a new login code")
        return challenge_id

    def otp_triggered(self, challenge_id: str) -> None:
        self.emit("otp.triggered", challenge_id=challenge_id)

    def wait_for_otp(
        self, challenge_id: str, deadline_at: Optional[datetime] = None
    ) -> str:
        timeout = _remaining_seconds(deadline_at)
        if timeout is not None and timeout <= 0:
            self.otp_failed(challenge_id, "auth_deadline")
            raise ActionError("Authentication deadline passed before an OTP arrived")
        try:
            frame = self.inbox.receive(
                {"otp.provide", "otp.expired", "otp.error"},
                timeout=timeout,
                predicate=lambda item: item.get("challenge_id") == challenge_id,
            )
        except queue.Empty as exc:
            self.otp_failed(challenge_id, "auth_deadline")
            raise ActionError(
                "Authentication deadline passed while waiting for an OTP"
            ) from exc
        if deadline_at is not None and _remaining_seconds(deadline_at) <= 0:
            self.otp_failed(challenge_id, "auth_deadline")
            raise ActionError("Authentication deadline passed while waiting for an OTP")
        if frame["type"] == "otp.expired":
            self.otp_failed(challenge_id, "expired")
            raise ActionError("No fresh OTP arrived before the provider deadline")
        if frame["type"] == "otp.error":
            self.otp_failed(challenge_id, "provider_error")
            raise ActionError("OTP provider failed while waiting for a login code")
        value = frame.get("code")
        if not isinstance(value, str) or _OTP_PATTERN.fullmatch(value) is None:
            raise ProtocolError("OTP must contain 4 to 8 ASCII digits")
        self.redactor.add(value)
        return value

    def otp_submitted(self, challenge_id: str) -> None:
        self.emit("otp.submitted", challenge_id=challenge_id)
        self.inbox.ignore_otp_challenge(challenge_id)

    def otp_not_required(self, challenge_id: str) -> None:
        self.emit("otp.not_required", challenge_id=challenge_id)
        self.inbox.ignore_otp_challenge(challenge_id)

    def otp_failed(self, challenge_id: str, reason: str) -> None:
        self.emit("otp.failed", challenge_id=challenge_id, reason=reason)
        self.inbox.ignore_otp_challenge(challenge_id)

    def await_confirmation_ready(self, pass_key: str, label: str) -> str:
        """Wait until Go durably records the pre-click confirmation marker."""

        confirmation_id = uuid.uuid4().hex
        self.emit(
            "confirmation.starting",
            confirmation_id=confirmation_id,
            pass_key=pass_key,
            label=label,
        )
        try:
            frame = self.inbox.receive(
                {"confirmation.ready", "confirmation.error"},
                predicate=lambda item: item.get("confirmation_id") == confirmation_id,
            )
        except queue.Empty as exc:
            # Production waits indefinitely here. This guard keeps a broken or
            # synthetic inbox from accidentally allowing the click.
            raise ActionError(
                "Control plane did not acknowledge final confirmation"
            ) from exc
        if frame["type"] == "confirmation.error":
            raise ActionError(
                "Control plane could not record final confirmation durably"
            )
        return confirmation_id

    def wait_for_approval(
        self,
        pass_key: str,
        label: str,
        browser_is_ready: Callable[[], bool],
        heartbeat_seconds: float = 15.0,
        check_seconds: float = 1.0,
    ) -> None:
        approval_id = uuid.uuid4().hex
        self.emit(
            "approval.request", approval_id=approval_id, pass_key=pass_key, label=label
        )
        last_heartbeat = time.monotonic()
        while True:
            if not browser_is_ready():
                self.emit(
                    "approval.expired",
                    approval_id=approval_id,
                    reason="browser_or_session_expired",
                )
                raise ApprovalExpired("Manual confirmation expired in the browser")
            try:
                frame = self.inbox.receive(
                    {"approval.approve", "approval.cancel"},
                    timeout=check_seconds,
                    predicate=lambda item: item.get("approval_id") == approval_id,
                )
            except queue.Empty:
                now = time.monotonic()
                if now - last_heartbeat >= heartbeat_seconds:
                    self.heartbeat("awaiting_approval")
                    last_heartbeat = now
                continue
            if frame["type"] == "approval.cancel":
                self.emit("approval.cancelled", approval_id=approval_id)
                raise Cancelled("manual booking cancelled")
            self.emit("approval.approved", approval_id=approval_id)
            return


def _remaining_seconds(deadline_at: Optional[datetime]) -> Optional[float]:
    if deadline_at is None:
        return None
    if deadline_at.tzinfo is None:
        raise ProtocolError("authentication deadline must include a UTC offset")
    return (deadline_at - datetime.now(deadline_at.tzinfo)).total_seconds()
