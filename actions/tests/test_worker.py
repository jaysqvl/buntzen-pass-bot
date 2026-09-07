from __future__ import annotations

import os
import sys
import time
import tempfile
import unittest
from dataclasses import replace
from datetime import date
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from buntzen_actions.config import ActionConfig
from buntzen_actions.errors import ActionError, Cancelled, OutcomeUnknown, ProtocolError
from buntzen_actions.worker import _chromium_user_agent, _open_context, _read_start_or_cancel, run_action
from buntzen_actions.yodel import BookingResult


def browser_launcher():
    return SimpleNamespace(chromium=SimpleNamespace(
        executable_path="/synthetic/chromium",
        launch_persistent_context=Mock(),
    ))


def make_config(profile_dir: Path) -> ActionConfig:
    return ActionConfig(
        run_id="job-1",
        command="auth-check",
        mode="manual",
        profile_dir=profile_dir,
        target_date=date(2030, 1, 15),
        timezone_name="UTC",
        login_probe_url="https://example.test",
        allowed_yodel_origins=frozenset({"https://example.test"}),
        all_day_pass_url=None,
        half_day_pass_url=None,
        vehicle_keyword="car",
        pass_order=(),
        headless=True,
        browser_channel=None,
        executable_path=None,
        default_timeout_ms=15_000,
        poll_deadline_seconds=120,
        poll_min_seconds=1,
        poll_max_seconds=2,
        artifacts_dir=None,
        release_at=None,
        auth_deadline_at=None,
    )


class WorkerTests(unittest.TestCase):
    def test_pre_start_cancel_completes_readiness_probe_cleanly(self) -> None:
        stream = SimpleNamespace(read=lambda: {"v": 2, "type": "control.cancel"})
        self.assertIsNone(_read_start_or_cancel(stream))

    def test_browser_closes_and_preserves_action_failure_kind(self) -> None:
        for failure_type in (ActionError, Cancelled, OutcomeUnknown, ProtocolError):
            with self.subTest(failure_type=failure_type):
                context = Mock(pages=[object()])
                failure = failure_type("synthetic action outcome")
                with (
                    tempfile.TemporaryDirectory() as directory,
                    patch("playwright.sync_api.sync_playwright"),
                    patch("buntzen_actions.worker._open_context", return_value=context),
                    patch("buntzen_actions.worker.SafeDiagnostics") as diagnostics,
                    patch("buntzen_actions.worker.YodelAction") as action,
                ):
                    action.return_value.execute.side_effect = failure
                    with self.assertRaises(failure_type) as raised:
                        run_action(make_config(Path(directory) / "profile"), object())
                self.assertIs(raised.exception, failure)
                diagnostics.return_value.close.assert_called_once()
                context.close.assert_called_once()

    def test_diagnostic_cleanup_failure_does_not_hide_outcome_or_skip_browser_close(self) -> None:
        context = Mock(pages=[object()])
        with (
            tempfile.TemporaryDirectory() as directory,
            patch("playwright.sync_api.sync_playwright"),
            patch("buntzen_actions.worker._open_context", return_value=context),
            patch("buntzen_actions.worker.SafeDiagnostics") as diagnostics,
            patch("buntzen_actions.worker.YodelAction") as action,
            self.assertLogs("buntzen_actions.worker", level="WARNING"),
        ):
            diagnostics.return_value.close.side_effect = RuntimeError("cleanup failure")
            action.return_value.execute.return_value = BookingResult(True, "Confirmed", "all_day")
            result = run_action(make_config(Path(directory) / "profile"), object())
        self.assertEqual(result, ("Confirmed", "all_day"))
        context.close.assert_called_once()

    def test_failed_booking_result_is_not_reported_as_success(self) -> None:
        context = Mock(pages=[object()])
        with (
            tempfile.TemporaryDirectory() as directory,
            patch("playwright.sync_api.sync_playwright"),
            patch("buntzen_actions.worker._open_context", return_value=context),
            patch("buntzen_actions.worker.SafeDiagnostics"),
            patch("buntzen_actions.worker.YodelAction") as action,
        ):
            action.return_value.execute.return_value = BookingResult(False, "No pass available")
            with self.assertRaisesRegex(ActionError, "No pass available"):
                run_action(make_config(Path(directory) / "profile"), object())
        context.close.assert_called_once()

    def test_browser_launch_preserves_site_compatibility_and_sandbox(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            playwright = browser_launcher()
            with patch("buntzen_actions.worker._browser_version_output", return_value="Chromium 1.55.5010.0123"):
                _open_context(playwright, make_config(Path(directory) / "profile"))
            launch = playwright.chromium.launch_persistent_context.call_args.kwargs
            self.assertIs(launch["chromium_sandbox"], True)
            self.assertEqual(launch["service_workers"], "allow")
            self.assertIn("Chrome/1.55.5010.0123", launch["user_agent"])
            self.assertNotIn("HeadlessChrome", launch["user_agent"])
            self.assertNotIn("--no-sandbox", launch.get("args", []))

    def test_operator_chrome_uses_same_executable_for_version_and_launch(self) -> None:
        executable = "/synthetic/Google Chrome"
        playwright = browser_launcher()
        with patch.dict(os.environ, {"BUNTZEN_BROWSER_EXECUTABLE": executable}), patch(
            "buntzen_actions.worker._browser_version_output", return_value="Google Chrome 1.55.5010.0123"
        ) as version:
            _open_context(playwright, make_config(Path("/tmp/synthetic-profile")))
        version.assert_called_once_with(executable)
        launch = playwright.chromium.launch_persistent_context.call_args.kwargs
        self.assertEqual(launch["executable_path"], executable)
        self.assertIn("Chrome/1.55.5010.0123", launch["user_agent"])

    def test_constructed_config_cannot_select_member_executable(self) -> None:
        for override in ({"executable_path": "/tmp/member-program"}, {"browser_channel": "../chrome"}):
            with self.subTest(override=override), patch("buntzen_actions.worker.subprocess.Popen") as probe:
                playwright = browser_launcher()
                config = replace(make_config(Path("/tmp/synthetic-profile")), **override)
                with self.assertRaises(ProtocolError):
                    _open_context(playwright, config)
                probe.assert_not_called()
                playwright.chromium.launch_persistent_context.assert_not_called()

    def test_supported_channels_resolve_once_for_probe_and_launch(self) -> None:
        for channel in ("chrome", "chrome-beta", "chrome-dev", "chrome-canary", " CHROME "):
            with self.subTest(channel=channel), patch.dict(os.environ, {"BUNTZEN_BROWSER_EXECUTABLE": ""}), patch(
                "buntzen_actions.worker._browser_channel_executable", return_value="/synthetic/chrome"
            ) as resolve, patch(
                "buntzen_actions.worker._browser_version_output", return_value="Google Chrome 1.55.5010.0123"
            ) as version:
                playwright = browser_launcher()
                _open_context(playwright, replace(make_config(Path("/tmp/profile")), browser_channel=channel))
                resolve.assert_called_once_with(channel.strip().lower())
                version.assert_called_once_with("/synthetic/chrome")
                self.assertEqual(playwright.chromium.launch_persistent_context.call_args.kwargs["executable_path"], "/synthetic/chrome")

    def test_version_probe_bounds_real_stdout_stderr_and_runtime(self) -> None:
        cases = {
            "valid": ("print('Google Chrome 1.55.5010.0123')", None),
            "stderr version": ("import sys; print('Chromium 1.55.5010.0123', file=sys.stderr)", None),
            "stdout overflow": ("import os; os.write(1, b'x' * 65536)", "safety limit"),
            "stderr overflow": ("import os; os.write(2, b'x' * 65536)", "safety limit"),
            "malformed": ("print('not a browser')", "could not be determined"),
            "exit failure": ("print('Chrome 1.55.5010.0123'); raise SystemExit(1)", "could not be determined"),
            "timeout": ("import time; time.sleep(2)", "timed out"),
        }
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / "Test Chrome"
            for name, (body, error) in cases.items():
                with self.subTest(name=name), patch("buntzen_actions.worker._VERSION_TIMEOUT_SECONDS", 0.5):
                    executable.write_text(f"#!{sys.executable}\n{body}\n")
                    executable.chmod(0o700)
                    started = time.monotonic()
                    if error:
                        with self.assertRaisesRegex(ActionError, error):
                            _chromium_user_agent(str(executable))
                    else:
                        self.assertIn("Chrome/1.55.5010.0123", _chromium_user_agent(str(executable)))
                    self.assertLess(time.monotonic() - started, 1.5)

    def test_insecure_tls_test_seam_is_explicit_and_loopback_only(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = replace(
                make_config(Path(directory) / "profile"),
                login_probe_url="https://127.0.0.1:8443/login",
                allowed_yodel_origins=frozenset({"https://127.0.0.1:8443"}),
            )
            playwright = browser_launcher()
            with patch.dict(
                "os.environ", {"BUNTZEN_ACTIONPROC_HELPER": "e2e-local-tls"}
            ), patch(
                "buntzen_actions.worker._browser_version_output",
                return_value="Chromium 1.55.5010.0123",
            ):
                _open_context(playwright, config)
            launch = playwright.chromium.launch_persistent_context.call_args.kwargs
            self.assertIs(launch["ignore_https_errors"], True)

            playwright = browser_launcher()
            remote = replace(
                config,
                login_probe_url="https://example.test/login",
                allowed_yodel_origins=frozenset({"https://example.test"}),
            )
            with patch.dict(
                "os.environ", {"BUNTZEN_ACTIONPROC_HELPER": "e2e-local-tls"}
            ), patch(
                "buntzen_actions.worker._browser_version_output",
                return_value="Chromium 1.55.5010.0123",
            ):
                _open_context(playwright, remote)
            launch = playwright.chromium.launch_persistent_context.call_args.kwargs
            self.assertNotIn("ignore_https_errors", launch)


if __name__ == "__main__":
    unittest.main()
