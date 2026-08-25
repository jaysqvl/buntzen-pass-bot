from __future__ import annotations

import sys
import tempfile
import types
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from buntzen_actions.config import ActionConfig
from buntzen_actions.errors import ActionError
from buntzen_actions.worker import _open_context, _read_start_or_cancel, run_action


class Tracing:
    def start(self, **kwargs):
        return None


class Page:
    def route(self, pattern, handler):
        return None

    def stop(self, **kwargs):
        return None


class Context:
    def __init__(self) -> None:
        self.pages = [Page()]
        self.tracing = Tracing()
        self.closed = 0
        self.default_timeout = None
        self.navigation_timeout = None

    def set_default_timeout(self, value):
        self.default_timeout = value

    def set_default_navigation_timeout(self, value):
        self.navigation_timeout = value

    def close(self):
        self.closed += 1


class Chromium:
    def __init__(self, context) -> None:
        self.context = context
        self.launch_options = None
        self.executable_path = "/synthetic/chromium"

    def launch_persistent_context(self, **kwargs):
        self.launch_options = kwargs
        return self.context


class Playwright:
    def __init__(self, context) -> None:
        self.chromium = Chromium(context)


class PlaywrightManager:
    last_playwright = None

    def __init__(self, context) -> None:
        self.value = Playwright(context)
        PlaywrightManager.last_playwright = self.value

    def __enter__(self):
        return self.value

    def __exit__(self, exc_type, exc, traceback):
        return False


def make_config(profile_dir: Path) -> ActionConfig:
    return ActionConfig(
        run_id="job-1",
        command="auth-check",
        mode="manual",
        profile_dir=profile_dir,
        target_date=__import__("datetime").date(2030, 1, 15),
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

    def test_browser_context_closes_when_action_fails(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            context = Context()
            sync_api = types.ModuleType("playwright.sync_api")
            sync_api.sync_playwright = lambda: PlaywrightManager(context)
            playwright = types.ModuleType("playwright")
            playwright.sync_api = sync_api
            modules = {"playwright": playwright, "playwright.sync_api": sync_api}
            with (
                patch.dict(sys.modules, modules),
                patch(
                    "buntzen_actions.worker.YodelAction.execute",
                    side_effect=ActionError("synthetic failure"),
                ),
                patch(
                    "buntzen_actions.worker.subprocess.run",
                    return_value=SimpleNamespace(
                        returncode=0,
                        stdout="Chromium 1.55.5010.0123",
                        stderr="",
                    ),
                ),
            ):
                with self.assertRaises(ActionError):
                    run_action(make_config(Path(directory) / "profile"), object())
            self.assertEqual(context.closed, 1)
            self.assertEqual(context.default_timeout, 15_000)
            self.assertEqual(context.navigation_timeout, 15_000)
            self.assertIs(
                PlaywrightManager.last_playwright.chromium.launch_options[
                    "chromium_sandbox"
                ],
                True,
            )
            self.assertEqual(
                PlaywrightManager.last_playwright.chromium.launch_options[
                    "service_workers"
                ],
                "allow",
            )
            self.assertIn(
                "Chrome/1.55.5010.0123",
                PlaywrightManager.last_playwright.chromium.launch_options[
                    "user_agent"
                ],
            )
            self.assertNotIn(
                "HeadlessChrome",
                PlaywrightManager.last_playwright.chromium.launch_options[
                    "user_agent"
                ],
            )
            self.assertNotIn(
                "--no-sandbox",
                PlaywrightManager.last_playwright.chromium.launch_options.get(
                    "args", []
                ),
            )

    def test_explicit_chrome_uses_its_exact_version_without_headless_token(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            context = Context()
            playwright = Playwright(context)
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
            launch = playwright.chromium.launch_options
            self.assertEqual(launch["executable_path"], executable)
            self.assertIn("Chrome/1.55.5010.0123", launch["user_agent"])
            self.assertNotIn("HeadlessChrome", launch["user_agent"])

    def test_insecure_tls_test_seam_is_explicit_and_loopback_only(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            context = Context()
            config = replace(
                make_config(Path(directory) / "profile"),
                login_probe_url="https://127.0.0.1:8443/login",
                allowed_yodel_origins=frozenset({"https://127.0.0.1:8443"}),
            )
            playwright = Playwright(context)
            with patch.dict(
                "os.environ", {"BUNTZEN_ACTIONPROC_HELPER": "e2e-local-tls"}
            ), patch(
                "buntzen_actions.worker.subprocess.run",
                return_value=SimpleNamespace(
                    returncode=0, stdout="Chromium 1.55.5010.0123", stderr=""
                ),
            ):
                _open_context(playwright, config)
            self.assertIs(playwright.chromium.launch_options["ignore_https_errors"], True)

            context = Context()
            playwright = Playwright(context)
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
            self.assertNotIn("ignore_https_errors", playwright.chromium.launch_options)


if __name__ == "__main__":
    unittest.main()
