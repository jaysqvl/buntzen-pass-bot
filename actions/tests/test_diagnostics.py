from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from buntzen_actions.diagnostics import SafeDiagnostics


class SensitiveBrowser:
    def __getattr__(self, name):
        raise AssertionError("diagnostics accessed raw browser content: " + name)


class DiagnosticsTests(unittest.TestCase):
    def test_authenticated_diagnostics_never_touch_browser_content(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            diagnostics = SafeDiagnostics(SensitiveBrowser(), Path(directory))
            for _ in range(2):
                diagnostics.pause_for_auth()
                self.assertFalse(diagnostics.safe)
                self.assertIsNone(diagnostics.screenshot(SensitiveBrowser(), "phone-5559876543"))
                diagnostics.authenticated()
                self.assertTrue(diagnostics.safe)
                self.assertIsNone(diagnostics.screenshot(SensitiveBrowser(), "otp-729104"))
                diagnostics.suspend_trace()
                diagnostics.authenticated()
            diagnostics.close()
            diagnostics.close()
            self.assertFalse(diagnostics.safe)
            self.assertEqual(list(Path(directory).rglob("*")), [])

    def test_no_artifact_directory_also_never_captures(self) -> None:
        diagnostics = SafeDiagnostics(SensitiveBrowser(), None)
        diagnostics.authenticated()
        self.assertIsNone(diagnostics.screenshot(SensitiveBrowser(), "private-name"))
        diagnostics.pause_for_auth()
        diagnostics.close()


if __name__ == "__main__":
    unittest.main()
