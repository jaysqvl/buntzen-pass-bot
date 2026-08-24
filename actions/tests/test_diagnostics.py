from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from buntzen_actions.diagnostics import SafeDiagnostics


class Tracing:
    def __init__(self) -> None:
        self.calls = []

    def start(self, **kwargs) -> None:
        self.calls.append(("start", kwargs))

    def stop(self, **kwargs) -> None:
        self.calls.append(("stop", kwargs))


class Context:
    def __init__(self) -> None:
        self.tracing = Tracing()


class Page:
    def __init__(self) -> None:
        self.paths = []

    def screenshot(self, path, full_page) -> None:
        self.paths.append(path)


class DiagnosticsTests(unittest.TestCase):
    def test_auth_state_never_captures_artifacts(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            context = Context()
            page = Page()
            diagnostics = SafeDiagnostics(context, Path(directory))
            self.assertIsNone(diagnostics.screenshot(page, "login"))
            self.assertEqual(context.tracing.calls, [])
            diagnostics.authenticated()
            self.assertEqual(context.tracing.calls[0][0], "start")
            self.assertIsNotNone(diagnostics.screenshot(page, "booking"))
            diagnostics.pause_for_auth()
            self.assertEqual(context.tracing.calls[-1][0], "stop")
            self.assertIsNone(diagnostics.screenshot(page, "otp"))


if __name__ == "__main__":
    unittest.main()
