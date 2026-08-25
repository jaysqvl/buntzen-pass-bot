from __future__ import annotations

import ipaddress
import logging
import os
import re
import shutil
import subprocess
import sys
from typing import Any, Optional
from urllib.parse import urlparse

from .config import ActionConfig
from .control import ControlPort
from .diagnostics import SafeDiagnostics
from .errors import ActionError, Cancelled, OutcomeUnknown, ProtocolError
from .protocol import ControlInbox, JsonLineStream, PROTOCOL_VERSION
from .secrets import RedactingLogFilter, SecretRedactor
from .yodel import YodelAction


logger = logging.getLogger("buntzen_actions.worker")

EXIT_FAILED = 1
EXIT_CANCELLED = 2
EXIT_OUTCOME_UNKNOWN = 3
EXIT_PROTOCOL = 64

_CHROMIUM_VERSION_PATTERN = re.compile(r"\b(\d+\.\d+\.\d+\.\d+)\b")


def configure_logging(redactor: SecretRedactor) -> None:
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(
        logging.Formatter("%(levelname)s %(name)s: %(message)s")
    )
    handler.addFilter(RedactingLogFilter(redactor))
    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    level_name = os.environ.get("BUNTZEN_ACTION_LOG_LEVEL", "info").strip().lower()
    levels = {
        "debug": logging.DEBUG,
        "info": logging.INFO,
        "warn": logging.WARNING,
        "warning": logging.WARNING,
        "error": logging.ERROR,
    }
    root.setLevel(levels.get(level_name, logging.INFO))


def _open_context(playwright: Any, config: ActionConfig) -> Any:
    launch: dict[str, Any] = {
        "user_data_dir": str(config.profile_dir),
        "headless": config.headless,
        "chromium_sandbox": True,
        "viewport": {"width": 1365, "height": 900},
        "locale": "en-CA",
        "timezone_id": config.timezone_name,
        # Yodel reloads until it observes a service-worker controller. Blocking
        # registration therefore creates an infinite navigation loop. The
        # exact-origin navigation and secret-fill guards remain in YodelAction.
        "service_workers": "allow",
    }
    if config.executable_path:
        launch["executable_path"] = config.executable_path
        user_agent_executable = config.executable_path
    elif config.browser_channel:
        launch["channel"] = config.browser_channel
        user_agent_executable = _browser_channel_executable(config.browser_channel)
    else:
        user_agent_executable = str(playwright.chromium.executable_path)
    launch["user_agent"] = _chromium_user_agent(user_agent_executable)
    # Integration tests use an ephemeral self-signed HTTPS server. Keep this
    # escape hatch both explicit and loopback-only so normal workers retain
    # strict certificate validation even if their launch configuration is
    # otherwise operator-controlled.
    if _allow_insecure_loopback_tls(config):
        launch["ignore_https_errors"] = True
    return playwright.chromium.launch_persistent_context(**launch)


def _chromium_user_agent(executable: str) -> str:
    """Return a normal Chrome UA whose version matches the launched binary."""

    try:
        completed = subprocess.run(
            [executable, "--version"],
            check=False,
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise ActionError("Chromium version could not be determined") from exc
    match = _CHROMIUM_VERSION_PATTERN.search(
        f"{completed.stdout}\n{completed.stderr}"
    )
    if completed.returncode != 0 or match is None:
        raise ActionError("Chromium version could not be determined")
    version = match.group(1)
    # Yodel's load balancer rejects Playwright's HeadlessChrome product token.
    # This only substitutes Chrome's ordinary product token for the same exact
    # binary version; it does not add stealth flags or bypass an auth challenge.
    platform_token = (
        "Macintosh; Intel Mac OS X 10_15_7"
        if sys.platform == "darwin"
        else "X11; Linux x86_64"
    )
    return (
        f"Mozilla/5.0 ({platform_token}) AppleWebKit/537.36 "
        f"(KHTML, like Gecko) Chrome/{version} Safari/537.36"
    )


def _browser_channel_executable(channel: str) -> str:
    """Resolve the installed Chrome executable selected by Playwright."""

    candidates = {
        "chrome": (
            "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
            "/opt/google/chrome/chrome",
            "google-chrome",
            "google-chrome-stable",
        ),
        "chrome-beta": (
            "/Applications/Google Chrome Beta.app/Contents/MacOS/Google Chrome Beta",
            "/opt/google/chrome-beta/chrome",
            "google-chrome-beta",
        ),
        "chrome-dev": (
            "/Applications/Google Chrome Dev.app/Contents/MacOS/Google Chrome Dev",
            "/opt/google/chrome-unstable/chrome",
            "google-chrome-unstable",
        ),
        "chrome-canary": (
            "/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
        ),
    }.get(channel.strip().lower(), ())
    for candidate in candidates:
        if os.path.isabs(candidate):
            if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
                return candidate
            continue
        resolved = shutil.which(candidate)
        if resolved:
            return resolved
    raise ActionError(
        "Chrome channel executable could not be resolved; configure its explicit path"
    )


def _allow_insecure_loopback_tls(config: ActionConfig) -> bool:
    if os.environ.get("BUNTZEN_ACTIONPROC_HELPER") != "e2e-local-tls":
        return False
    hostname = urlparse(config.login_probe_url).hostname
    if hostname == "localhost":
        return True
    try:
        return hostname is not None and ipaddress.ip_address(hostname).is_loopback
    except ValueError:
        return False


def run_action(config: ActionConfig, control: ControlPort) -> tuple[str, Optional[str]]:
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        raise ActionError("Playwright is not installed in the action runtime") from exc

    config.profile_dir.mkdir(parents=True, exist_ok=True)
    diagnostics: Optional[SafeDiagnostics] = None
    context = None
    logger.debug(
        "Launching persistent Chromium context run_id=%s command=%s mode=%s headless=%s",
        config.run_id,
        config.command,
        config.mode,
        config.headless,
    )
    with sync_playwright() as playwright:
        try:
            context = _open_context(playwright, config)
            logger.debug("Chromium context launched run_id=%s", config.run_id)
            context.set_default_timeout(config.default_timeout_ms)
            context.set_default_navigation_timeout(config.default_timeout_ms)
            diagnostics = SafeDiagnostics(
                context=context, base_dir=config.artifacts_dir
            )
            page = context.pages[0] if context.pages else context.new_page()
            action = YodelAction(
                page=page,
                context=context,
                config=config,
                control=control,
                diagnostics=diagnostics,
            )
            result = action.execute()
            if not result.success:
                raise ActionError(result.message)
            return result.message, result.pass_key
        finally:
            if diagnostics is not None:
                diagnostics.close()
            if context is not None:
                try:
                    context.close()
                    logger.debug("Chromium context closed run_id=%s", config.run_id)
                except Exception:
                    logger.warning("Browser context did not close cleanly")


def _safe_complete(
    stream: JsonLineStream, status: str, message: str, **payload: object
) -> None:
    try:
        stream.write("run.complete", status=status, message=message, **payload)
    except Exception:
        logger.error("Could not emit terminal run event")


def _read_start_or_cancel(stream: JsonLineStream) -> Optional[ActionConfig]:
    """Accept a normal run or a clean pre-start readiness-probe shutdown."""

    frame = stream.read()
    if frame.get("type") == "control.cancel":
        return None
    return ActionConfig.from_start(frame)


def main() -> int:
    redactor = SecretRedactor()
    configure_logging(redactor)
    # The control inbox remains blocked on stdin while the main thread exits.
    # Read from FileIO directly so interpreter shutdown never contends for a
    # BufferedReader lock held by that daemon thread after run.complete.
    stream = JsonLineStream(sys.stdin.buffer.raw, sys.stdout.buffer)
    try:
        stream.write(
            "worker.ready",
            protocol=PROTOCOL_VERSION,
            action="yodel",
            commands=["auth-check", "dry-run", "book"],
        )
        config = _read_start_or_cancel(stream)
        if config is None:
            logger.debug("Worker readiness probe completed before action start")
            return 0
        logger.debug(
            "Accepted action run run_id=%s command=%s mode=%s",
            config.run_id,
            config.command,
            config.mode,
        )
        inbox = ControlInbox(stream)
        control = ControlPort(stream=stream, inbox=inbox, redactor=redactor)
        control.status("starting", "Starting isolated Yodel browser action.")
        message, pass_key = run_action(config, control)
        logger.info(
            "Action completed run_id=%s command=%s status=succeeded",
            config.run_id,
            config.command,
        )
        payload: dict[str, object] = {}
        if pass_key is not None:
            payload["pass_key"] = pass_key
        _safe_complete(stream, "succeeded", redactor.redact(message), **payload)
        return 0
    except OutcomeUnknown as exc:
        message = redactor.redact(exc)
        logger.error("%s", message)
        _safe_complete(stream, "outcome_unknown", message)
        return EXIT_OUTCOME_UNKNOWN
    except Cancelled as exc:
        message = redactor.redact(exc)
        logger.info("%s", message)
        _safe_complete(stream, "cancelled", message)
        return EXIT_CANCELLED
    except ProtocolError as exc:
        message = redactor.redact(exc)
        logger.error("Protocol error: %s", message)
        _safe_complete(stream, "failed", message, failure_kind="protocol")
        return EXIT_PROTOCOL
    except ActionError as exc:
        message = redactor.redact(exc)
        logger.error("Action failed: %s", message)
        _safe_complete(stream, "failed", message, failure_kind="action")
        return EXIT_FAILED
    except KeyboardInterrupt:
        _safe_complete(stream, "cancelled", "Action process was interrupted.")
        return 130
    except Exception as exc:
        # Do not include a traceback anywhere. Playwright exception chains can
        # contain page state even when the top-level message has been redacted.
        message = redactor.redact(exc)
        logger.error("Unexpected action failure: %s", message)
        _safe_complete(stream, "failed", message, failure_kind="unexpected")
        return EXIT_FAILED


if __name__ == "__main__":
    raise SystemExit(main())
