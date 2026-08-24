from __future__ import annotations

import logging
from threading import Lock
from typing import Any


class SecretRedactor:
    """Best-effort in-memory redaction for errors and stderr logging."""

    def __init__(self) -> None:
        self._values: set[str] = set()
        self._lock = Lock()

    def add(self, value: Any) -> None:
        if not isinstance(value, str) or not value:
            return
        with self._lock:
            self._values.add(value)

    def redact(self, value: Any) -> str:
        rendered = str(value)
        with self._lock:
            secrets = sorted(self._values, key=len, reverse=True)
        for secret in secrets:
            rendered = rendered.replace(secret, "[REDACTED]")
        return rendered


class RedactingLogFilter(logging.Filter):
    def __init__(self, redactor: SecretRedactor) -> None:
        super().__init__()
        self._redactor = redactor

    def filter(self, record: logging.LogRecord) -> bool:
        try:
            message = record.getMessage()
        except Exception:
            message = "log message unavailable"
        record.msg = self._redactor.redact(message)
        record.args = ()
        return True
