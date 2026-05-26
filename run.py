"""
CLI entry point for the fully unattended Buntzen Pass Bot.
"""
from __future__ import annotations

import argparse
import logging
import sys
import time
from dataclasses import replace
from datetime import datetime

try:
    from dotenv import load_dotenv
except ImportError:
    def load_dotenv(*args, **kwargs):
        return False

from src.diagnostics import Diagnostics
from src.env_utils import ConfigError, load_config
from src.scheduler import sleep_until, wait_with_keepalive
from src.twilio_utils import TwilioService


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Buntzen pass booking bot")
    parser.add_argument(
        "command",
        choices=("auth-check", "dry-run", "book"),
        help="auth-check validates unattended login, dry-run checks booking flow without checkout, book runs the scheduled booking flow",
    )
    parser.add_argument(
        "--mode",
        choices=("dry-run", "manual", "auto"),
        help="Override RUN_MODE for the current command",
    )
    parser.add_argument(
        "--log-level",
        default="INFO",
        choices=("DEBUG", "INFO", "WARNING", "ERROR"),
        help="Logging verbosity",
    )
    return parser


def configure_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )


def open_persistent_context(playwright, config):
    launch_kwargs = {
        "user_data_dir": str(config.user_data_dir),
        "headless": config.headless,
        "viewport": {"width": 1365, "height": 900},
        "locale": "en-CA",
        "timezone_id": config.timezone_name,
        "args": [
            "--disable-dev-shm-usage",
            "--no-sandbox",
        ],
    }
    if config.browser_channel:
        launch_kwargs["channel"] = config.browser_channel
    return playwright.chromium.launch_persistent_context(**launch_kwargs)


def run_auth_check(bot: BookingBot) -> int:
    ok = bot.ensure_authenticated()
    if ok:
        bot.alert("Buntzen bot auth-check passed. Yodel session is ready.")
        return 0
    bot.alert("Buntzen bot auth-check failed. See diagnostics for details.", urgent=True)
    return 2


def run_dry_run(bot: BookingBot) -> int:
    if not bot.ensure_authenticated():
        bot.alert("Buntzen bot dry-run failed before booking: not authenticated.", urgent=True)
        return 2
    result = bot.try_booking_once(mode="dry-run")
    if result.success:
        bot.alert(f"Buntzen bot dry-run passed: {result.message}")
        return 0
    bot.alert(f"Buntzen bot dry-run failed: {result.message}", urgent=True)
    return 3


def run_book(bot: BookingBot) -> int:
    config = bot.config
    logger = logging.getLogger("buntzen_pass_bot.run")

    if not config.schedule:
        bot.alert("Buntzen bot immediate booking run started because SCHEDULE=false.")
        if not bot.ensure_authenticated():
            bot.alert("Buntzen bot stopped: Yodel auth is not ready.", urgent=True)
            return 2
        result = bot.poll_for_booking(mode=config.run_mode)
        if result.success:
            bot.alert(f"Buntzen bot success: {result.message}")
            return 0
        bot.alert(f"Buntzen bot failed: {result.message}", urgent=True)
        return 4

    now = datetime.now(config.timezone)
    if now < config.prep_at:
        logger.info("Waiting for prep window at %s", config.prep_at.isoformat())
        sleep_until(config.prep_at, config.timezone)

    bot.alert(
        "Buntzen bot prep started. Browser is opening, auth will be checked before release."
    )

    if not bot.ensure_authenticated(deadline=config.auth_deadline_at):
        bot.alert(
            "Buntzen bot stopped: Yodel auth was not ready before the release deadline.",
            urgent=True,
        )
        return 2

    logger.info("Authenticated. Keeping session warm until %s", config.release_at.isoformat())
    wait_with_keepalive(
        until=config.release_at,
        timezone=config.timezone,
        keepalive=lambda: bot.keep_session_warm(),
        min_interval_seconds=35,
        max_interval_seconds=95,
    )

    sleep_until(config.release_at, config.timezone, final_poll_seconds=0.05)
    bot.alert(f"Buntzen bot started booking at {config.release_at.strftime('%H:%M:%S')}.")

    result = bot.poll_for_booking(mode=config.run_mode)
    if result.success:
        bot.alert(f"Buntzen bot success: {result.message}")
        return 0

    bot.alert(f"Buntzen bot failed: {result.message}", urgent=True)
    return 4


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    configure_logging(args.log_level)
    logger = logging.getLogger("buntzen_pass_bot.run")

    load_dotenv(override=True)
    try:
        config = load_config(command=args.command)
    except ConfigError as exc:
        logger.error("Configuration error: %s", exc)
        return 1

    if args.command == "dry-run":
        config = replace(config, run_mode="dry-run")
    if args.mode:
        config = replace(config, run_mode=args.mode)

    logger.info("Loaded config: %s", config.safe_summary())
    diagnostics = Diagnostics()

    try:
        twilio = TwilioService.from_config(config)
    except Exception as exc:
        logger.error("Twilio setup failed: %s", exc)
        return 1

    try:
        from playwright.sync_api import sync_playwright
        from src.booking import BookingBot, BookingError
    except ImportError as exc:
        logger.error("Missing browser dependency: %s", exc)
        logger.error("Install dependencies with: uv sync")
        return 1

    with sync_playwright() as playwright:
        context = open_persistent_context(playwright, config)
        context.set_default_timeout(config.default_timeout_ms)
        context.tracing.start(screenshots=True, snapshots=True, sources=True)
        page = context.pages[0] if context.pages else context.new_page()
        bot = BookingBot(page=page, context=context, config=config, twilio=twilio, diagnostics=diagnostics)

        exit_code = 1
        try:
            if args.command == "auth-check":
                exit_code = run_auth_check(bot)
            elif args.command == "dry-run":
                exit_code = run_dry_run(bot)
            else:
                exit_code = run_book(bot)
        except KeyboardInterrupt:
            logger.warning("Interrupted by user.")
            exit_code = 130
        except BookingError as exc:
            logger.error("Booking error: %s", exc)
            bot.capture_failure("booking-error")
            bot.alert(f"Buntzen bot stopped: {exc}", urgent=True)
            exit_code = 5
        except Exception as exc:
            logger.exception("Unexpected failure")
            bot.capture_failure("unexpected-error")
            bot.alert(f"Buntzen bot crashed: {exc}", urgent=True)
            exit_code = 6
        finally:
            trace_path = diagnostics.path_for("trace", "zip")
            try:
                context.tracing.stop(path=str(trace_path))
                logger.info("Saved Playwright trace: %s", trace_path)
            except Exception as exc:
                logger.warning("Could not save Playwright trace: %s", exc)
            time.sleep(1)
            context.close()

    return exit_code


if __name__ == "__main__":
    sys.exit(main())
