from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import date, datetime, time, timedelta
from pathlib import Path
from typing import Iterable
from urllib.parse import urlparse
from zoneinfo import ZoneInfo


RUN_MODES = {"dry-run", "manual", "auto"}
COMMANDS_REQUIRING_TWILIO = {"auth-check", "dry-run", "book"}


class ConfigError(ValueError):
    """Raised when environment configuration is missing or invalid."""


@dataclass(frozen=True)
class BotConfig:
    user_data_dir: Path
    target_date: date
    schedule: bool
    slow_poll_until: time
    start_time: time
    prep_minutes_before: int
    auth_deadline_minutes_before: int
    poll_deadline_seconds: int
    poll_min_seconds: float
    poll_max_seconds: float
    run_mode: str
    headless: bool
    browser_channel: str | None
    default_timeout_ms: int
    timezone_name: str
    all_day_pass_url: str | None
    half_day_pass_url: str | None
    vehicle_keyword: str
    check_all_day: bool
    check_morning: bool
    check_afternoon: bool
    yodel_email: str | None
    yodel_password: str | None
    twilio_account_sid: str
    twilio_auth_token: str
    twilio_otp_number: str
    twilio_alert_to_number: str | None
    twilio_alerts_enabled: bool
    otp_timeout_seconds: int
    otp_poll_interval_seconds: float

    @property
    def timezone(self) -> ZoneInfo:
        return ZoneInfo(self.timezone_name)

    @property
    def release_at(self) -> datetime:
        release_date = self.target_date - timedelta(days=1)
        return datetime.combine(release_date, self.start_time, tzinfo=self.timezone)

    @property
    def prep_at(self) -> datetime:
        return self.release_at - timedelta(minutes=self.prep_minutes_before)

    @property
    def auth_deadline_at(self) -> datetime:
        return self.release_at - timedelta(minutes=self.auth_deadline_minutes_before)

    @property
    def login_probe_url(self) -> str:
        return self.all_day_pass_url or self.half_day_pass_url or "https://yodelportal.com/buntzen-lake"

    def safe_summary(self) -> dict[str, object]:
        return {
            "user_data_dir": str(self.user_data_dir),
            "target_date": self.target_date.isoformat(),
            "schedule": self.schedule,
            "release_at": self.release_at.isoformat(),
            "run_mode": self.run_mode,
            "headless": self.headless,
            "browser_channel": self.browser_channel,
            "check_all_day": self.check_all_day,
            "check_morning": self.check_morning,
            "check_afternoon": self.check_afternoon,
            "twilio_alerts_enabled": self.twilio_alerts_enabled,
            "twilio_otp_number": _mask_phone(self.twilio_otp_number),
            "twilio_alert_to_number": _mask_phone(self.twilio_alert_to_number),
            "has_yodel_email": bool(self.yodel_email),
            "has_yodel_password": bool(self.yodel_password),
        }


def load_config(command: str | None = None) -> BotConfig:
    target_date = _date_env("TARGET_DATE", required=True)
    user_data_dir = _path_env("USER_DATA_DIR") or Path(__file__).resolve().parents[1] / "playwright-profile"

    config = BotConfig(
        user_data_dir=user_data_dir,
        target_date=target_date,
        schedule=_bool_env("SCHEDULE", default=True),
        slow_poll_until=_time_env("SLOW_POLL_UNTIL", default="06:59"),
        start_time=_time_env("START_TIME", default="07:00"),
        prep_minutes_before=_int_env("PREP_MINUTES_BEFORE", default=30, minimum=1),
        auth_deadline_minutes_before=_int_env("AUTH_DEADLINE_MINUTES_BEFORE", default=5, minimum=1),
        poll_deadline_seconds=_int_env("POLL_DEADLINE_SECONDS", default=120, minimum=5),
        poll_min_seconds=_float_env("POLL_MIN_SECONDS", default=1.4, minimum=0.5),
        poll_max_seconds=_float_env("POLL_MAX_SECONDS", default=3.6, minimum=0.6),
        run_mode=_choice_env("RUN_MODE", RUN_MODES, default="manual"),
        headless=_bool_env("HEADLESS", default=False),
        browser_channel=_optional_env("BROWSER_CHANNEL", default="chrome"),
        default_timeout_ms=_int_env("DEFAULT_TIMEOUT_MS", default=15000, minimum=1000),
        timezone_name=_optional_env("TIMEZONE", default="America/Vancouver") or "America/Vancouver",
        all_day_pass_url=_optional_env("ALL_DAY_PASS_URL"),
        half_day_pass_url=_optional_env("HALF_DAY_PASS_URL"),
        vehicle_keyword=_required_env("VEHICLE_KEYWORD"),
        check_all_day=_bool_env("CHECK_ALL_DAY", default=False),
        check_morning=_bool_env("CHECK_MORNING", default=False),
        check_afternoon=_bool_env("CHECK_AFTERNOON", default=False),
        yodel_email=_optional_env("YODEL_EMAIL"),
        yodel_password=_optional_env("YODEL_PASSWORD"),
        twilio_account_sid=_optional_env("TWILIO_ACCOUNT_SID") or "",
        twilio_auth_token=_optional_env("TWILIO_AUTH_TOKEN") or "",
        twilio_otp_number=_optional_env("TWILIO_OTP_NUMBER") or "",
        twilio_alert_to_number=_optional_env("TWILIO_ALERT_TO_NUMBER"),
        twilio_alerts_enabled=_bool_env("TWILIO_ALERTS_ENABLED", default=True),
        otp_timeout_seconds=_int_env("OTP_TIMEOUT_SECONDS", default=120, minimum=10),
        otp_poll_interval_seconds=_float_env("OTP_POLL_INTERVAL_SECONDS", default=3.0, minimum=1.0),
    )

    _validate(config, command=command)
    config.user_data_dir.mkdir(parents=True, exist_ok=True)
    return config


def _validate(config: BotConfig, command: str | None) -> None:
    _validate_timezone(config.timezone_name)
    if config.poll_min_seconds > config.poll_max_seconds:
        raise ConfigError("POLL_MIN_SECONDS cannot be greater than POLL_MAX_SECONDS.")
    if not (config.check_all_day or config.check_morning or config.check_afternoon):
        raise ConfigError("Enable at least one pass type: CHECK_ALL_DAY, CHECK_MORNING, or CHECK_AFTERNOON.")
    if config.check_all_day:
        _validate_url("ALL_DAY_PASS_URL", config.all_day_pass_url)
    if config.check_morning or config.check_afternoon:
        _validate_url("HALF_DAY_PASS_URL", config.half_day_pass_url)
    if not config.vehicle_keyword.strip():
        raise ConfigError("VEHICLE_KEYWORD cannot be blank.")
    if bool(config.yodel_email) != bool(config.yodel_password):
        raise ConfigError("Set both YODEL_EMAIL and YODEL_PASSWORD, or leave both blank for profile-only auth.")
    if command in COMMANDS_REQUIRING_TWILIO:
        for name, value in (
            ("TWILIO_ACCOUNT_SID", config.twilio_account_sid),
            ("TWILIO_AUTH_TOKEN", config.twilio_auth_token),
            ("TWILIO_OTP_NUMBER", config.twilio_otp_number),
        ):
            if not value:
                raise ConfigError(f"{name} is required for unattended OTP handling.")
        _validate_phone("TWILIO_OTP_NUMBER", config.twilio_otp_number)
    if config.twilio_alerts_enabled:
        if not config.twilio_alert_to_number:
            raise ConfigError("TWILIO_ALERT_TO_NUMBER is required when TWILIO_ALERTS_ENABLED=true.")
        _validate_phone("TWILIO_ALERT_TO_NUMBER", config.twilio_alert_to_number)


def _required_env(name: str) -> str:
    value = _optional_env(name)
    if value is None:
        raise ConfigError(f"{name} is required.")
    return value


def _optional_env(name: str, default: str | None = None) -> str | None:
    value = os.getenv(name)
    if value is None:
        return default
    value = value.strip()
    return value or default


def _path_env(name: str) -> Path | None:
    value = _optional_env(name)
    return Path(value).expanduser() if value else None


def _bool_env(name: str, default: bool) -> bool:
    value = _optional_env(name)
    if value is None:
        return default
    normalized = value.lower()
    if normalized in {"1", "true", "yes", "y", "on"}:
        return True
    if normalized in {"0", "false", "no", "n", "off"}:
        return False
    raise ConfigError(f"{name} must be true or false.")


def _int_env(name: str, default: int, minimum: int | None = None) -> int:
    value = _optional_env(name)
    if value is None:
        result = default
    else:
        try:
            result = int(value)
        except ValueError as exc:
            raise ConfigError(f"{name} must be an integer.") from exc
    if minimum is not None and result < minimum:
        raise ConfigError(f"{name} must be at least {minimum}.")
    return result


def _float_env(name: str, default: float, minimum: float | None = None) -> float:
    value = _optional_env(name)
    if value is None:
        result = default
    else:
        try:
            result = float(value)
        except ValueError as exc:
            raise ConfigError(f"{name} must be a number.") from exc
    if minimum is not None and result < minimum:
        raise ConfigError(f"{name} must be at least {minimum}.")
    return result


def _choice_env(name: str, choices: Iterable[str], default: str) -> str:
    value = _optional_env(name, default=default) or default
    if value not in choices:
        valid = ", ".join(sorted(choices))
        raise ConfigError(f"{name} must be one of: {valid}.")
    return value


def _date_env(name: str, required: bool) -> date:
    value = _optional_env(name)
    if value is None:
        if required:
            raise ConfigError(f"{name} is required.")
        return date.today()
    try:
        return datetime.strptime(value, "%Y-%m-%d").date()
    except ValueError as exc:
        raise ConfigError(f"{name} must use YYYY-MM-DD format.") from exc


def _time_env(name: str, default: str) -> time:
    value = _optional_env(name, default=default) or default
    try:
        return datetime.strptime(value, "%H:%M").time()
    except ValueError as exc:
        raise ConfigError(f"{name} must use HH:MM 24-hour format.") from exc


def _validate_url(name: str, value: str | None) -> None:
    if not value:
        raise ConfigError(f"{name} is required for the selected pass types.")
    parsed = urlparse(value)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        raise ConfigError(f"{name} must be a valid http(s) URL.")


def _validate_phone(name: str, value: str) -> None:
    if not value.startswith("+") or not value[1:].replace(" ", "").replace("-", "").isdigit():
        raise ConfigError(f"{name} must be an E.164-style phone number, e.g. +16045551212.")


def _validate_timezone(timezone_name: str) -> None:
    try:
        ZoneInfo(timezone_name)
    except Exception as exc:
        raise ConfigError(f"TIMEZONE is invalid: {timezone_name}") from exc


def _mask_phone(value: str | None) -> str | None:
    if not value:
        return None
    digits = "".join(ch for ch in value if ch.isdigit())
    if len(digits) <= 4:
        return "***"
    return f"+***{digits[-4:]}"
