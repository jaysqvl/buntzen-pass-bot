from __future__ import annotations

from pathlib import Path
from typing import Any, Optional


class SafeDiagnostics:
    """Keep action lifecycle hooks without capturing private browser content.

    Authenticated pages can still contain OTP inputs, cookies, bearer tokens,
    and private responses. Playwright writes raw trace resources before ZIP
    export, so neither auth-state gating nor archive redaction is sufficient.
    Diagnostics use the existing allowlisted application status/events instead.
    """

    def __init__(self, context: Any, base_dir: Optional[Path]) -> None:
        self._safe = False
        if base_dir is not None:
            base_dir.mkdir(mode=0o700, parents=True, exist_ok=True)

    @property
    def safe(self) -> bool:
        return self._safe

    def pause_for_auth(self) -> None:
        self._safe = False

    def authenticated(self) -> None:
        self._safe = True

    def suspend_trace(self) -> None:
        """Retained lifecycle hook; raw tracing is always disabled."""

    def screenshot(self, page: Any, name: str) -> Optional[Path]:
        # Never inspect the page or serialize the supplied name. Masking known
        # inputs does not protect unknown session state or rendered secrets.
        return None

    def close(self) -> None:
        self._safe = False
