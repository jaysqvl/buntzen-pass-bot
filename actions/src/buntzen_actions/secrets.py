from __future__ import annotations

import logging
import re
from threading import Lock
from typing import Any
from urllib.parse import urlsplit, urlunsplit


_URL_PATTERN = re.compile(r"https?://[^\s\"'<>]+", re.IGNORECASE)
_SENSITIVE_VALUE_PATTERN = re.compile(
    r"\b(password|passwd|auth_token|access_token|refresh_token|api_key|secret|otp|passcode|code)\b"
    r"([\"']?)(\s*[:=]\s*)(\"[^\"]*\"|'[^']*'|[^\s,;]+)",
    re.IGNORECASE,
)
_AUTHORIZATION_PATTERN = re.compile(
    r"\bauthorization\b([\"']?)(\s*[:=]\s*)[^\r\n,;]+", re.IGNORECASE
)
_STANDALONE_CODE_PATTERN = re.compile(r"(?<!\d)\d{4,8}(?!\d)")


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
        rendered = _URL_PATTERN.sub(_sanitize_url, rendered)
        rendered = _AUTHORIZATION_PATTERN.sub(
            lambda match: f"authorization{match.group(1)}{match.group(2)}[REDACTED]",
            rendered,
        )
        return _SENSITIVE_VALUE_PATTERN.sub(
            lambda match: (
                f"{match.group(1)}{match.group(2)}{match.group(3)}[REDACTED]"
            ),
            rendered,
        )

    def redact_diagnostic(self, value: Any) -> str:
        """Apply the stricter policy reserved for non-protocol stderr."""

        safe_urls: list[str] = []

        def hold_url(match: re.Match[str]) -> str:
            token = f"__BUNTZEN_SAFE_URL_{len(safe_urls):x}__"
            safe_urls.append(match.group(0))
            return token

        rendered = _URL_PATTERN.sub(hold_url, self.redact(value))
        rendered = _STANDALONE_CODE_PATTERN.sub("[REDACTED-CODE]", rendered)
        for index, safe_url in enumerate(safe_urls):
            rendered = rendered.replace(
                f"__BUNTZEN_SAFE_URL_{index:x}__", safe_url
            )
        return rendered


def _sanitize_url(match: re.Match[str]) -> str:
    try:
        parsed = urlsplit(match.group(0))
        if not parsed.scheme or not parsed.hostname:
            return "[REDACTED-URL]"
        hostname = parsed.hostname
        if ":" in hostname and not hostname.startswith("["):
            hostname = f"[{hostname}]"
        port = f":{parsed.port}" if parsed.port is not None else ""
        return urlunsplit((parsed.scheme, f"{hostname}{port}", parsed.path, "", ""))
    except (TypeError, ValueError):
        return "[REDACTED-URL]"


class RedactingLogFilter(logging.Filter):
    def __init__(self, redactor: SecretRedactor) -> None:
        super().__init__()
        self._redactor = redactor

    def filter(self, record: logging.LogRecord) -> bool:
        try:
            message = record.getMessage()
        except Exception:
            message = "log message unavailable"
        record.msg = self._redactor.redact_diagnostic(message)
        record.args = ()
        return True
