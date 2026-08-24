from __future__ import annotations

import logging
import sys
from typing import Any, Optional

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


def configure_logging(redactor: SecretRedactor) -> None:
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(
        logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s")
    )
    handler.addFilter(RedactingLogFilter(redactor))
    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(logging.INFO)


def _open_context(playwright: Any, config: ActionConfig) -> Any:
    launch: dict[str, Any] = {
        "user_data_dir": str(config.profile_dir),
        "headless": config.headless,
        "chromium_sandbox": True,
        "viewport": {"width": 1365, "height": 900},
        "locale": "en-CA",
        "timezone_id": config.timezone_name,
    }
    if config.executable_path:
        launch["executable_path"] = config.executable_path
    elif config.browser_channel:
        launch["channel"] = config.browser_channel
    return playwright.chromium.launch_persistent_context(**launch)


def run_action(config: ActionConfig, control: ControlPort) -> tuple[str, Optional[str]]:
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        raise ActionError("Playwright is not installed in the action runtime") from exc

    config.profile_dir.mkdir(parents=True, exist_ok=True)
    diagnostics: Optional[SafeDiagnostics] = None
    context = None
    with sync_playwright() as playwright:
        try:
            context = _open_context(playwright, config)
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
                except Exception:
                    logger.warning("Browser context did not close cleanly")


def _safe_complete(
    stream: JsonLineStream, status: str, message: str, **payload: object
) -> None:
    try:
        stream.write("run.complete", status=status, message=message, **payload)
    except Exception:
        logger.error("Could not emit terminal run event")


def main() -> int:
    redactor = SecretRedactor()
    configure_logging(redactor)
    stream = JsonLineStream(sys.stdin.buffer, sys.stdout.buffer)
    try:
        stream.write(
            "worker.ready",
            protocol=PROTOCOL_VERSION,
            action="yodel",
            commands=["auth-check", "dry-run", "book"],
        )
        start = stream.read()
        config = ActionConfig.from_start(start)
        inbox = ControlInbox(stream)
        control = ControlPort(stream=stream, inbox=inbox, redactor=redactor)
        control.status("starting", "Starting isolated Yodel browser action.")
        message, pass_key = run_action(config, control)
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
