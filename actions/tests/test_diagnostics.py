from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from buntzen_actions.diagnostics import SafeDiagnostics
from buntzen_actions.errors import ActionError


class Tracing:
    def __init__(self, stop_error=None) -> None:
        self.calls = []
        self.stop_error = stop_error

    def start(self, **kwargs) -> None:
        self.calls.append(("start", kwargs))

    def stop(self, **kwargs) -> None:
        self.calls.append(("stop", kwargs))
        if self.stop_error is not None:
            raise self.stop_error
        Path(kwargs["path"]).write_text(str(len(self.calls)), encoding="utf-8")


class Context:
    def __init__(self, tracing=None) -> None:
        self.tracing = tracing or Tracing()


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
            diagnostics.suspend_trace()
            self.assertEqual(context.tracing.calls[-1][0], "stop")
            self.assertTrue(diagnostics.safe)
            diagnostics.authenticated()
            self.assertEqual(context.tracing.calls[-1][0], "start")
            diagnostics.pause_for_auth()
            self.assertEqual(context.tracing.calls[-1][0], "stop")
            self.assertIsNone(diagnostics.screenshot(page, "otp"))

    def test_trace_segments_rotate_through_eight_fixed_paths(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            context = Context()
            diagnostics = SafeDiagnostics(context, Path(directory))
            for _index in range(10):
                diagnostics.authenticated()
                diagnostics.suspend_trace()

            stop_paths = [
                Path(payload["path"]).name
                for kind, payload in context.tracing.calls
                if kind == "stop"
            ]
            self.assertEqual(
                stop_paths,
                [
                    "trace-1.zip",
                    "trace-2.zip",
                    "trace-3.zip",
                    "trace-4.zip",
                    "trace-5.zip",
                    "trace-6.zip",
                    "trace-7.zip",
                    "trace-8.zip",
                    "trace-1.zip",
                    "trace-2.zip",
                ],
            )
            self.assertEqual(len(list(Path(directory).glob("trace-*.zip"))), 8)

    def test_trace_stop_failure_is_fatal_at_boundaries_but_close_is_best_effort(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            tracing = Tracing(stop_error=RuntimeError("synthetic trace stop failure"))
            diagnostics = SafeDiagnostics(Context(tracing), Path(directory))
            diagnostics.authenticated()

            with self.assertLogs(
                "buntzen_actions.diagnostics", level="WARNING"
            ) as captured:
                with self.assertRaises(ActionError):
                    diagnostics.pause_for_auth()
                self.assertFalse(diagnostics.safe)
                self.assertTrue(diagnostics._tracing)
                diagnostics.close()
            self.assertEqual(len(captured.output), 2)
            self.assertEqual(
                [kind for kind, _payload in tracing.calls],
                ["start", "stop", "stop"],
            )


if __name__ == "__main__":
    unittest.main()
