from __future__ import annotations

import logging
import re
from pathlib import Path
from typing import Any, Optional


logger = logging.getLogger("buntzen_actions.diagnostics")


class SafeDiagnostics:
    """Captures artifacts only while the browser is outside sensitive auth."""

    def __init__(self, context: Any, base_dir: Optional[Path]) -> None:
        self.context = context
        self.base_dir = base_dir
        self._safe = False
        self._tracing = False
        self._trace_index = 0
        if self.base_dir is not None:
            self.base_dir.mkdir(parents=True, exist_ok=True)

    @property
    def safe(self) -> bool:
        return self._safe

    def pause_for_auth(self) -> None:
        self._safe = False
        self._stop_trace()

    def authenticated(self) -> None:
        self._safe = True
        if self.base_dir is not None and not self._tracing:
            self.context.tracing.start(screenshots=True, snapshots=True, sources=True)
            self._tracing = True

    def screenshot(self, page: Any, name: str) -> Optional[Path]:
        if not self._safe or self.base_dir is None:
            return None
        path = self.base_dir / f"{_safe_name(name)}.png"
        try:
            page.screenshot(path=str(path), full_page=True)
            return path
        except Exception:
            logger.warning("Could not capture safe screenshot %s", _safe_name(name))
            return None

    def close(self) -> None:
        self._safe = False
        self._stop_trace()

    def _stop_trace(self) -> None:
        if not self._tracing:
            return
        self._tracing = False
        self._trace_index += 1
        if self.base_dir is None:
            return
        path = self.base_dir / f"trace-{self._trace_index}.zip"
        try:
            self.context.tracing.stop(path=str(path))
        except Exception:
            logger.warning("Could not finish safe Playwright trace segment")


def _safe_name(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "-", value).strip("-") or "artifact"
