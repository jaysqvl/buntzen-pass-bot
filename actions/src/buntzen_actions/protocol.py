from __future__ import annotations

import json
import queue
import threading
from collections import deque
from typing import Any, BinaryIO, Callable, Iterable, Mapping, Optional

from .errors import Cancelled, ProtocolError


PROTOCOL_VERSION = 1
MAX_FRAME_BYTES = 64 * 1024

# The child must never send any of these fields to the control plane. This is a
# second line of defence against accidentally echoing a secret in a new event.
_FORBIDDEN_OUTPUT_KEYS = frozenset(
    {
        "code",
        "credential",
        "credentials",
        "email",
        "otp",
        "password",
        "secret",
        "token",
    }
)

_FORBIDDEN_START_KEYS = frozenset(
    {
        "account_sid",
        "auth_token",
        "bluebubbles_password",
        "code",
        "credential",
        "credentials",
        "email",
        "master_key",
        "otp",
        "password",
        "secret",
        "token",
        "twilio_auth_token",
        "yodel_email",
        "yodel_password",
    }
)


def _has_forbidden_key(value: Any, forbidden: frozenset[str]) -> bool:
    if isinstance(value, Mapping):
        for key, child in value.items():
            normalized = str(key).strip().lower()
            if normalized in forbidden or _has_forbidden_key(child, forbidden):
                return True
    elif isinstance(value, (list, tuple)):
        return any(_has_forbidden_key(item, forbidden) for item in value)
    return False


def validate_start_has_no_secrets(frame: Mapping[str, Any]) -> None:
    config = frame.get("config")
    if _has_forbidden_key(config, _FORBIDDEN_START_KEYS):
        raise ProtocolError(
            "run.start must not contain credentials, OTPs, or provider secrets"
        )


class JsonLineStream:
    """Reads and writes bounded UTF-8 JSON objects, one per line."""

    def __init__(
        self, reader: BinaryIO, writer: BinaryIO, max_bytes: int = MAX_FRAME_BYTES
    ) -> None:
        self._reader = reader
        self._writer = writer
        self._max_bytes = max_bytes
        self._write_lock = threading.Lock()

    def read(self) -> dict[str, Any]:
        raw = self._reader.readline(self._max_bytes + 1)
        if not raw:
            raise EOFError("control stream closed")
        if len(raw) > self._max_bytes:
            raise ProtocolError("control frame exceeds the size limit")
        if not raw.endswith(b"\n"):
            raise ProtocolError("control frame must end with a newline")
        try:
            value = json.loads(
                raw.decode("utf-8"),
                parse_constant=lambda _value: (_ for _ in ()).throw(
                    ValueError("non-finite JSON number")
                ),
            )
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ProtocolError("control frame is not valid UTF-8 JSON") from exc
        except ValueError as exc:
            raise ProtocolError("control frame contains a non-finite number") from exc
        if not isinstance(value, dict):
            raise ProtocolError("control frame must be a JSON object")
        version = value.get("v")
        if (
            isinstance(version, bool)
            or not isinstance(version, int)
            or version != PROTOCOL_VERSION
        ):
            raise ProtocolError("unsupported protocol version")
        event_type = value.get("type")
        if not isinstance(event_type, str) or not event_type:
            raise ProtocolError("control frame requires a non-empty type")
        return value

    def write(self, event_type: str, **payload: Any) -> None:
        if not event_type:
            raise ProtocolError("event type cannot be empty")
        if _has_forbidden_key(payload, _FORBIDDEN_OUTPUT_KEYS):
            raise ProtocolError("refusing to emit a secret-bearing field")
        frame = {"v": PROTOCOL_VERSION, "type": event_type, **payload}
        try:
            raw = (
                json.dumps(
                    frame, separators=(",", ":"), ensure_ascii=False, allow_nan=False
                )
                + "\n"
            ).encode("utf-8")
        except (TypeError, ValueError) as exc:
            raise ProtocolError("outgoing event is not JSON serializable") from exc
        if len(raw) > self._max_bytes:
            raise ProtocolError("outgoing event exceeds the size limit")
        with self._write_lock:
            self._writer.write(raw)
            self._writer.flush()


class ControlInbox:
    """Continuously drains stdin so cancellation is observable between actions."""

    def __init__(self, stream: JsonLineStream) -> None:
        self._stream = stream
        self._queue: queue.Queue[Any] = queue.Queue()
        self._deferred: deque[dict[str, Any]] = deque()
        self._deferred_lock = threading.Lock()
        self._ignored_otp_challenges: set[str] = set()
        self._thread = threading.Thread(
            target=self._read_loop, name="control-inbox", daemon=True
        )
        self._thread.start()

    def _read_loop(self) -> None:
        while True:
            try:
                self._queue.put(self._stream.read())
            except BaseException as exc:
                self._queue.put(exc)
                return

    def _next(self, timeout: Optional[float]) -> dict[str, Any]:
        while True:
            with self._deferred_lock:
                item = self._deferred.popleft() if self._deferred else None
            if item is None:
                try:
                    item = self._queue.get(timeout=timeout)
                except queue.Empty:
                    raise
            if isinstance(item, EOFError):
                raise Cancelled("control stream closed") from item
            if isinstance(item, BaseException):
                raise item
            with self._deferred_lock:
                ignored = item.get("challenge_id") in self._ignored_otp_challenges
            if ignored and item.get("type", "").startswith("otp."):
                continue
            return item

    def receive(
        self,
        expected: Iterable[str],
        timeout: Optional[float] = None,
        predicate: Optional[Callable[[dict[str, Any]], bool]] = None,
    ) -> dict[str, Any]:
        expected_set = frozenset(expected)
        while True:
            frame = self._next(timeout)
            frame_type = frame["type"]
            if frame_type == "control.cancel":
                raise Cancelled("cancelled by control plane")
            if frame_type not in expected_set:
                names = ", ".join(sorted(expected_set))
                raise ProtocolError(f"expected {names}; received {frame_type}")
            if predicate is not None and not predicate(frame):
                raise ProtocolError(f"received mismatched {frame_type} correlation id")
            return frame

    def check_cancelled(self) -> None:
        """Raise promptly for cancellation without consuming future replies."""

        while True:
            try:
                item = self._queue.get_nowait()
            except queue.Empty:
                return
            if isinstance(item, EOFError):
                raise Cancelled("control stream closed") from item
            if isinstance(item, BaseException):
                raise item
            if item.get("type") == "control.cancel":
                raise Cancelled("cancelled by control plane")
            with self._deferred_lock:
                ignored = item.get("challenge_id") in self._ignored_otp_challenges
                if not (ignored and item.get("type", "").startswith("otp.")):
                    self._deferred.append(item)

    def ignore_otp_challenge(self, challenge_id: str) -> None:
        """Discard late provider replies after an OTP is no longer needed."""

        with self._deferred_lock:
            self._ignored_otp_challenges.add(challenge_id)
            self._deferred = deque(
                item
                for item in self._deferred
                if not (
                    item.get("challenge_id") == challenge_id
                    and item.get("type", "").startswith("otp.")
                )
            )
