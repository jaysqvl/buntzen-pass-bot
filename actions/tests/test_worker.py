from __future__ import annotations

import tempfile
import unittest
from dataclasses import replace
from datetime import date
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from buntzen_actions.config import ActionConfig
from buntzen_actions.errors import ActionError, Cancelled, OutcomeUnknown
from buntzen_actions.worker import _open_context, _read_start_or_cancel, run_action
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
        for failure_type in (ActionError, Cancelled, OutcomeUnknown):
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
            with patch("buntzen_actions.worker.subprocess.run", return_value=SimpleNamespace(
                returncode=0, stdout="Chromium 1.55.5010.0123", stderr="",
            )):
                _open_context(playwright, make_config(Path(directory) / "profile"))
            launch = playwright.chromium.launch_persistent_context.call_args.kwargs
            self.assertIs(launch["chromium_sandbox"], True)
            self.assertEqual(launch["service_workers"], "allow")
            self.assertIn("Chrome/1.55.5010.0123", launch["user_agent"])
            self.assertNotIn("HeadlessChrome", launch["user_agent"])
            self.assertNotIn("--no-sandbox", launch.get("args", []))

    def test_explicit_chrome_uses_its_exact_version_without_headless_token(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            playwright = browser_launcher()
            executable = "/synthetic/google-chrome"
            config = replace(
                make_config(Path(directory) / "profile"),
                executable_path=executable,
            )
            with patch(
                "buntzen_actions.worker.subprocess.run",
                return_value=SimpleNamespace(
                    returncode=0,
                    stdout="Google Chrome 1.55.5010.0123",
                    stderr="",
                ),
            ) as version:
                _open_context(playwright, config)
            version.assert_called_once_with(
                [executable, "--version"],
                check=False,
                capture_output=True,
                text=True,
                timeout=5,
            )
            launch = playwright.chromium.launch_persistent_context.call_args.kwargs
            self.assertEqual(launch["executable_path"], executable)
            self.assertIn("Chrome/1.55.5010.0123", launch["user_agent"])
            self.assertNotIn("HeadlessChrome", launch["user_agent"])

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
                "buntzen_actions.worker.subprocess.run",
                return_value=SimpleNamespace(
                    returncode=0, stdout="Chromium 1.55.5010.0123", stderr=""
                ),
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
                "buntzen_actions.worker.subprocess.run",
                return_value=SimpleNamespace(
                    returncode=0, stdout="Chromium 1.55.5010.0123", stderr=""
                ),
            ):
                _open_context(playwright, remote)
            launch = playwright.chromium.launch_persistent_context.call_args.kwargs
            self.assertNotIn("ignore_https_errors", launch)


if __name__ == "__main__":
    unittest.main()
